package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ricardossiqueira/iot-gateway/internal/automationrule"
	"github.com/ricardossiqueira/iot-gateway/internal/config"
	"github.com/ricardossiqueira/iot-gateway/internal/devicemanifest"
)

// AutomationRule is one event -> condition -> action definition
// (docs/device-manifests.md Marco 5, item 3). Unlike DeviceManifest, it has
// no draft/publish revision history: it is edited in place and toggled by
// Enabled, the same mutability model registry_routes already uses.
//
// SourceDeviceID/EventType identify the trigger using the same columns
// registry_automation_events already stores an accepted event under, so a
// future execution engine (item 4) can match a freshly recorded event
// directly against WHERE enabled AND source_device_id AND event_type.
// ConditionJSON is empty when the rule has no condition (always fires).
// ActionParametersJSON is fixed at rule-authoring time - no templating from
// the triggering event's payload (see the plan this was built from).
type AutomationRule struct {
	ID                    string
	Enabled               bool
	SourceDeviceID        string
	EventType             string
	ConditionJSON         string
	ActionDeviceID        string
	ActionCommandType     string
	ActionParametersJSON  string
	ActionSchemaValidated bool
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// CreateAutomationRule validates and persists a new rule. Validation mirrors
// validateRoute's discipline (store.go): source/action devices must be
// enabled with the right topic kind configured. When a device has a
// manifest bound, its declared events/commands additionally constrain
// EventType/ActionCommandType+ActionParametersJSON (the same
// devicemanifest.ValidateCommandParameters PublishCommand already uses) -
// without one, both fall back to accepting any non-empty value, same
// posture PublishCommand already has for manifest-less devices.
func (s *Store) CreateAutomationRule(ctx context.Context, rule AutomationRule) (AutomationRule, error) {
	if s == nil || s.db == nil {
		return AutomationRule{}, errors.New("registry store is closed")
	}
	canonicalParameters, schemaValidated, err := s.validateAutomationRule(ctx, &rule)
	if err != nil {
		return AutomationRule{}, err
	}
	rule.ActionParametersJSON = canonicalParameters
	rule.ActionSchemaValidated = schemaValidated

	tx, err := s.begin(ctx)
	if err != nil {
		return AutomationRule{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if err := checkDeviceTopicKind(ctx, tx, rule.SourceDeviceID, "event"); err != nil {
		return AutomationRule{}, fmt.Errorf("rule.source_device_id: %w", err)
	}
	if err := checkDeviceTopicKind(ctx, tx, rule.ActionDeviceID, "command"); err != nil {
		return AutomationRule{}, fmt.Errorf("rule.action_device_id: %w", err)
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM registry_automation_rules WHERE id = ?`, rule.ID).Scan(&exists); err == nil {
		return AutomationRule{}, fmt.Errorf("%w: %q", ErrAutomationRuleAlreadyExists, rule.ID)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return AutomationRule{}, fmt.Errorf("check automation rule: %w", err)
	}

	now := s.now()
	rule.CreatedAt, rule.UpdatedAt = now, now
	if err := insertAutomationRule(ctx, tx, rule); err != nil {
		return AutomationRule{}, err
	}
	if err := tx.Commit(); err != nil {
		return AutomationRule{}, fmt.Errorf("commit automation rule: %w", err)
	}
	return rule, nil
}

// UpdateAutomationRule replaces every mutable field of an existing rule
// (ID and CreatedAt are fixed by the existing row). Validation is identical
// to CreateAutomationRule.
func (s *Store) UpdateAutomationRule(ctx context.Context, rule AutomationRule) (AutomationRule, error) {
	if s == nil || s.db == nil {
		return AutomationRule{}, errors.New("registry store is closed")
	}
	canonicalParameters, schemaValidated, err := s.validateAutomationRule(ctx, &rule)
	if err != nil {
		return AutomationRule{}, err
	}
	rule.ActionParametersJSON = canonicalParameters
	rule.ActionSchemaValidated = schemaValidated

	tx, err := s.begin(ctx)
	if err != nil {
		return AutomationRule{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if err := checkDeviceTopicKind(ctx, tx, rule.SourceDeviceID, "event"); err != nil {
		return AutomationRule{}, fmt.Errorf("rule.source_device_id: %w", err)
	}
	if err := checkDeviceTopicKind(ctx, tx, rule.ActionDeviceID, "command"); err != nil {
		return AutomationRule{}, fmt.Errorf("rule.action_device_id: %w", err)
	}
	var createdAtNS int64
	if err := tx.QueryRowContext(ctx, `SELECT created_at_ns FROM registry_automation_rules WHERE id = ?`, rule.ID).Scan(&createdAtNS); errors.Is(err, sql.ErrNoRows) {
		return AutomationRule{}, fmt.Errorf("%w: %q", ErrAutomationRuleNotFound, rule.ID)
	} else if err != nil {
		return AutomationRule{}, fmt.Errorf("check automation rule: %w", err)
	}
	rule.CreatedAt = time.Unix(0, createdAtNS).UTC()
	rule.UpdatedAt = s.now()
	if _, err := tx.ExecContext(ctx, `UPDATE registry_automation_rules SET
		enabled = ?, source_device_id = ?, event_type = ?, condition_json = NULLIF(?, ''),
		action_device_id = ?, action_command_type = ?, action_parameters_json = ?, action_schema_validated = ?,
		updated_at_ns = ? WHERE id = ?`,
		boolInt(rule.Enabled), rule.SourceDeviceID, rule.EventType, rule.ConditionJSON,
		rule.ActionDeviceID, rule.ActionCommandType, rule.ActionParametersJSON, boolInt(rule.ActionSchemaValidated),
		rule.UpdatedAt.UnixNano(), rule.ID); err != nil {
		return AutomationRule{}, fmt.Errorf("update automation rule: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return AutomationRule{}, fmt.Errorf("commit automation rule: %w", err)
	}
	return rule, nil
}

// GetAutomationRule reads one rule by ID.
func (s *Store) GetAutomationRule(ctx context.Context, id string) (AutomationRule, error) {
	if s == nil || s.db == nil {
		return AutomationRule{}, errors.New("registry store is closed")
	}
	rule, err := scanAutomationRule(s.db.QueryRowContext(ctx, automationRuleSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return AutomationRule{}, fmt.Errorf("%w: %q", ErrAutomationRuleNotFound, id)
	}
	if err != nil {
		return AutomationRule{}, fmt.Errorf("read automation rule: %w", err)
	}
	return rule, nil
}

// ListAutomationRules returns every rule, ordered by id.
func (s *Store) ListAutomationRules(ctx context.Context) ([]AutomationRule, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("registry store is closed")
	}
	rows, err := s.db.QueryContext(ctx, automationRuleSelect+` ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list automation rules: %w", err)
	}
	defer rows.Close()
	rules := make([]AutomationRule, 0)
	for rows.Next() {
		rule, err := scanAutomationRule(rows)
		if err != nil {
			return nil, fmt.Errorf("read automation rule: %w", err)
		}
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list automation rules: %w", err)
	}
	return rules, nil
}

// SetAutomationRuleEnabled toggles a rule without touching its trigger,
// condition or action.
func (s *Store) SetAutomationRuleEnabled(ctx context.Context, id string, enabled bool) (AutomationRule, error) {
	if s == nil || s.db == nil {
		return AutomationRule{}, errors.New("registry store is closed")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE registry_automation_rules SET enabled = ?, updated_at_ns = ? WHERE id = ?`,
		boolInt(enabled), s.now().UnixNano(), id)
	if err != nil {
		return AutomationRule{}, fmt.Errorf("set automation rule enabled: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return AutomationRule{}, fmt.Errorf("count automation rule update: %w", err)
	}
	if count == 0 {
		return AutomationRule{}, fmt.Errorf("%w: %q", ErrAutomationRuleNotFound, id)
	}
	return s.GetAutomationRule(ctx, id)
}

// RemoveAutomationRule deletes a rule permanently - there is no revision
// history to fall back to (see AutomationRule's doc comment).
func (s *Store) RemoveAutomationRule(ctx context.Context, id string) error {
	if s == nil || s.db == nil {
		return errors.New("registry store is closed")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM registry_automation_rules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("remove automation rule: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count removed automation rule: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("%w: %q", ErrAutomationRuleNotFound, id)
	}
	return nil
}

// validateAutomationRule runs every check that does not need to be inside
// the write transaction: field shape, condition well-formedness, and - when
// the relevant device has a manifest bound - that EventType is one of its
// declared events and ActionParametersJSON validates against
// ActionCommandType's declared schema. Manifest resolution deliberately
// happens here, before any transaction begins (ResolveDeviceManifest reads
// via s.db, not a *sql.Tx) - the same sequencing ProvisionDeviceByIP
// (internal/admin) already uses: resolve first, mutate after.
func (s *Store) validateAutomationRule(ctx context.Context, rule *AutomationRule) (canonicalParameters string, schemaValidated bool, err error) {
	if err := config.ValidateDeviceID("rule.id", rule.ID); err != nil {
		return "", false, err
	}
	if strings.TrimSpace(rule.EventType) == "" {
		return "", false, errors.New("rule.event_type is required")
	}
	if strings.TrimSpace(rule.ActionCommandType) == "" {
		return "", false, errors.New("rule.action_command_type is required")
	}
	if condition := strings.TrimSpace(rule.ConditionJSON); condition != "" {
		if err := automationrule.ValidateCondition(json.RawMessage(condition)); err != nil {
			return "", false, fmt.Errorf("rule.condition_json: %w", err)
		}
		rule.ConditionJSON = condition
	} else {
		rule.ConditionJSON = ""
	}

	if document, found, err := s.ResolveDeviceManifest(ctx, rule.SourceDeviceID); err != nil {
		return "", false, fmt.Errorf("resolve source device manifest: %w", err)
	} else if found {
		declared := false
		for _, event := range document.Capabilities.Events {
			if event.Type == rule.EventType {
				declared = true
				break
			}
		}
		if !declared {
			return "", false, fmt.Errorf("rule.event_type %q is not declared by %q's manifest", rule.EventType, rule.SourceDeviceID)
		}
	}

	parameters := []byte(rule.ActionParametersJSON)
	var object map[string]json.RawMessage
	if err := json.Unmarshal(parameters, &object); err != nil || object == nil {
		return "", false, errors.New("rule.action_parameters_json must be a JSON object")
	}
	document, found, err := s.ResolveDeviceManifest(ctx, rule.ActionDeviceID)
	if err != nil {
		return "", false, fmt.Errorf("resolve action device manifest: %w", err)
	}
	if !found {
		return string(parameters), false, nil
	}
	canonical, err := devicemanifest.ValidateCommandParameters(document, rule.ActionCommandType, parameters)
	if err != nil {
		return "", false, fmt.Errorf("rule.action_parameters_json: %w", err)
	}
	return string(canonical), true, nil
}

// checkDeviceTopicKind mirrors validateRoute's topic-existence check
// (store.go) but keyed by device_id + topic kind rather than a raw topic
// string, matching how AutomationRule references devices directly.
func checkDeviceTopicKind(ctx context.Context, tx *sql.Tx, deviceID, kind string) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM registry_device_topics t
		JOIN registry_devices d ON d.id = t.device_id
		WHERE d.enabled = 1 AND t.device_id = ? AND t.kind = ?`, deviceID, kind).Scan(&count); err != nil {
		return fmt.Errorf("validate device: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("must reference an enabled device with a %q topic configured (got %q)", kind, deviceID)
	}
	return nil
}

const automationRuleSelect = `SELECT id, enabled, source_device_id, event_type, COALESCE(condition_json, ''),
	action_device_id, action_command_type, action_parameters_json, action_schema_validated, created_at_ns, updated_at_ns
	FROM registry_automation_rules`

// rowScanner is satisfied by both *sql.Row and *sql.Rows, letting
// scanAutomationRule serve GetAutomationRule and ListAutomationRules alike.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanAutomationRule(row rowScanner) (AutomationRule, error) {
	var rule AutomationRule
	var enabled, schemaValidated int
	var createdAtNS, updatedAtNS int64
	if err := row.Scan(&rule.ID, &enabled, &rule.SourceDeviceID, &rule.EventType, &rule.ConditionJSON,
		&rule.ActionDeviceID, &rule.ActionCommandType, &rule.ActionParametersJSON, &schemaValidated,
		&createdAtNS, &updatedAtNS); err != nil {
		return AutomationRule{}, err
	}
	rule.Enabled = enabled != 0
	rule.ActionSchemaValidated = schemaValidated != 0
	rule.CreatedAt = time.Unix(0, createdAtNS).UTC()
	rule.UpdatedAt = time.Unix(0, updatedAtNS).UTC()
	return rule, nil
}

func insertAutomationRule(ctx context.Context, tx *sql.Tx, rule AutomationRule) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO registry_automation_rules
		(id, enabled, source_device_id, event_type, condition_json, action_device_id, action_command_type,
		 action_parameters_json, action_schema_validated, created_at_ns, updated_at_ns)
		VALUES (?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?)`,
		rule.ID, boolInt(rule.Enabled), rule.SourceDeviceID, rule.EventType, rule.ConditionJSON,
		rule.ActionDeviceID, rule.ActionCommandType, rule.ActionParametersJSON, boolInt(rule.ActionSchemaValidated),
		rule.CreatedAt.UnixNano(), rule.UpdatedAt.UnixNano()); err != nil {
		return fmt.Errorf("insert automation rule %q: %w", rule.ID, err)
	}
	return nil
}

// MatchAutomationRules returns every enabled rule whose trigger is exactly
// deviceID/eventType (Marco 5 item 4 - the execution engine, internal/mqtt's
// Gateway.fireAutomationRules). Uses the same partial index
// registry_automation_rules_source that item 3's migration already created
// for this exact access pattern. No caching: events are low-frequency
// compared to telemetry, so a direct query per accepted event stays cheap
// and is always current - no revision/hot-reload plumbing needed.
func (s *Store) MatchAutomationRules(ctx context.Context, deviceID, eventType string) ([]AutomationRule, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("registry store is closed")
	}
	rows, err := s.db.QueryContext(ctx, automationRuleSelect+` WHERE enabled = 1 AND source_device_id = ? AND event_type = ? ORDER BY id`,
		deviceID, eventType)
	if err != nil {
		return nil, fmt.Errorf("match automation rules: %w", err)
	}
	defer rows.Close()
	rules := make([]AutomationRule, 0)
	for rows.Next() {
		rule, err := scanAutomationRule(rows)
		if err != nil {
			return nil, fmt.Errorf("read automation rule: %w", err)
		}
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("match automation rules: %w", err)
	}
	return rules, nil
}

// AutomationCommandExistsForRuleAndCausation reports whether ruleID has
// already fired in response to causationMessageID - the dedup check that
// stops a redelivered (QoS 1) event from firing the same rule twice.
func (s *Store) AutomationCommandExistsForRuleAndCausation(ctx context.Context, ruleID, causationMessageID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("registry store is closed")
	}
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM registry_automation_commands
		WHERE rule_id = ? AND causation_message_id = ? LIMIT 1`, ruleID, causationMessageID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check automation command dedup: %w", err)
	}
	return true, nil
}

// CountAutomationCommandsForRuleSince counts how many times ruleID has
// fired since since. The execution engine uses this as a per-rule firing
// rate limit standing in for true cycle detection (the protocol gives no
// way to prove an event was itself caused by an earlier command - see
// docs/device-manifests.md Marco 5's cycle-prevention note).
func (s *Store) CountAutomationCommandsForRuleSince(ctx context.Context, ruleID string, since time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("registry store is closed")
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM registry_automation_commands
		WHERE rule_id = ? AND published_at_ns >= ?`, ruleID, since.UnixNano()).Scan(&count); err != nil {
		return 0, fmt.Errorf("count automation commands for rule: %w", err)
	}
	return count, nil
}
