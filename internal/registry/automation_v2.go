package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ricardossiqueira/iot-gateway/internal/automationrule"
	"github.com/ricardossiqueira/iot-gateway/internal/devicev2"
)

type V2AutomationRule struct {
	ID             string
	Enabled        bool
	SourceDeviceID string
	OutputChannel  string
	EventType      string
	IgnoreRetained bool
	ConditionJSON  string
	TargetDeviceID string
	CommandType    string
	ParametersJSON string
	UpdatedAt      time.Time
}

var (
	ErrV2AutomationRuleNotFound = errors.New("v2 automation rule not found")
	ErrV2AutomationRuleExists   = errors.New("v2 automation rule ID already exists")
	ErrV2AutomationRuleInvalid  = errors.New("invalid v2 automation rule")
)

// V2RuntimeDevice is the immutable manifest binding used by MQTT at runtime.
// Only desired_state=active instances are returned by ListV2RuntimeDevices.
type V2RuntimeDevice struct {
	Device   V2Device
	Manifest devicev2.Manifest
}

// ListV2RuntimeDevices reads the active v2 policy in one query. Keeping this
// projection in registry prevents MQTT from accidentally consulting v1 state.
func (s *Store) ListV2RuntimeDevices(ctx context.Context) ([]V2RuntimeDevice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.device_id,d.device_uid,d.manifest_id,d.manifest_revision,d.manifest_sha256,d.firmware_version,d.identity_public_key,d.desired_state,d.active_state,d.created_at_ns,d.updated_at_ns,m.document_json FROM registry_v2_devices d JOIN registry_v2_manifests m ON m.manifest_id=d.manifest_id AND m.revision=d.manifest_revision WHERE d.desired_state='active' ORDER BY d.device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []V2RuntimeDevice{}
	for rows.Next() {
		var item V2RuntimeDevice
		var created, updated int64
		var document string
		if err := rows.Scan(&item.Device.DeviceID, &item.Device.DeviceUID, &item.Device.ManifestID, &item.Device.ManifestRevision, &item.Device.ManifestSHA256, &item.Device.FirmwareVersion, &item.Device.IdentityPublicKey, &item.Device.DesiredState, &item.Device.ActiveState, &created, &updated, &document); err != nil {
			return nil, err
		}
		item.Device.CreatedAt, item.Device.UpdatedAt = time.Unix(0, created).UTC(), time.Unix(0, updated).UTC()
		manifest, _, _, err := devicev2.Parse(document)
		if err != nil {
			return nil, fmt.Errorf("parse bound v2 manifest for %s: %w", item.Device.DeviceID, err)
		}
		item.Manifest = manifest
		items = append(items, item)
	}
	return items, rows.Err()
}

// ReserveV2AutomationExecution atomically claims an action before it is sent
// to MQTT. A failed publication remains visible in the audit trail and is not
// retried automatically for the same causation message.
func (s *Store) ReserveV2AutomationExecution(ctx context.Context, ruleID, messageID, commandID, targetDeviceID string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO registry_v2_automation_executions(rule_id,causation_message_id,command_id,target_device_id,status,created_at_ns) VALUES(?,?,?,?, 'reserved',?) ON CONFLICT(rule_id,causation_message_id) DO NOTHING`, ruleID, messageID, commandID, targetDeviceID, s.now().UnixNano())
	if err != nil {
		return false, fmt.Errorf("reserve v2 automation execution: %w", err)
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (s *Store) CompleteV2AutomationExecution(ctx context.Context, ruleID, messageID string, published bool) error {
	status := "failed"
	if published {
		status = "published"
	}
	result, err := s.db.ExecContext(ctx, `UPDATE registry_v2_automation_executions SET status=?,completed_at_ns=? WHERE rule_id=? AND causation_message_id=? AND status='reserved'`, status, s.now().UnixNano(), ruleID, messageID)
	if err != nil {
		return fmt.Errorf("complete v2 automation execution: %w", err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return errors.New("v2 automation execution reservation not found")
	}
	return nil
}

func (s *Store) CountV2PublishedAutomationExecutionsSince(ctx context.Context, ruleID string, since time.Time) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM registry_v2_automation_executions WHERE rule_id=? AND status='published' AND completed_at_ns>=?`, ruleID, since.UnixNano()).Scan(&count)
	return count, err
}

func (s *Store) CreateV2AutomationRule(ctx context.Context, r V2AutomationRule) (V2AutomationRule, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM registry_v2_automation_rules WHERE id=?)`, r.ID).Scan(&exists); err != nil {
		return V2AutomationRule{}, fmt.Errorf("check v2 automation rule ID: %w", err)
	}
	if exists != 0 {
		return V2AutomationRule{}, fmt.Errorf("%w: %s", ErrV2AutomationRuleExists, r.ID)
	}
	if err := s.validateV2Rule(ctx, &r); err != nil {
		return V2AutomationRule{}, err
	}
	r.UpdatedAt = s.now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO registry_v2_automation_rules(id,enabled,source_device_id,output_channel,event_type,ignore_retained,condition_json,target_device_id,command_type,parameters_json,updated_at_ns) VALUES(?,?,?,?,NULLIF(?,''),?,NULLIF(?,''),?,?,?,?)`, r.ID, boolInt(r.Enabled), r.SourceDeviceID, r.OutputChannel, r.EventType, boolInt(r.IgnoreRetained), r.ConditionJSON, r.TargetDeviceID, r.CommandType, r.ParametersJSON, r.UpdatedAt.UnixNano())
	if err != nil {
		// A concurrent creator may have won after the existence check. Preserve
		// the stable-ID contract and report that race as a conflict.
		if checkErr := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM registry_v2_automation_rules WHERE id=?)`, r.ID).Scan(&exists); checkErr == nil && exists != 0 {
			return V2AutomationRule{}, fmt.Errorf("%w: %s", ErrV2AutomationRuleExists, r.ID)
		}
		return V2AutomationRule{}, fmt.Errorf("create v2 automation rule: %w", err)
	}
	return r, nil
}
func (s *Store) ListV2AutomationRules(ctx context.Context) ([]V2AutomationRule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,enabled,source_device_id,output_channel,COALESCE(event_type,''),ignore_retained,COALESCE(condition_json,''),target_device_id,command_type,parameters_json,updated_at_ns FROM registry_v2_automation_rules WHERE deleted_at_ns IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []V2AutomationRule{}
	for rows.Next() {
		var r V2AutomationRule
		var enabled, ignore int
		var updated int64
		if err := rows.Scan(&r.ID, &enabled, &r.SourceDeviceID, &r.OutputChannel, &r.EventType, &ignore, &r.ConditionJSON, &r.TargetDeviceID, &r.CommandType, &r.ParametersJSON, &updated); err != nil {
			return nil, err
		}
		r.Enabled = enabled != 0
		r.IgnoreRetained = ignore != 0
		r.UpdatedAt = time.Unix(0, updated).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateV2AutomationRule replaces a rule's editable fields after validating
// them against the current device manifests. The server owns UpdatedAt and ID.
func (s *Store) UpdateV2AutomationRule(ctx context.Context, r V2AutomationRule) (V2AutomationRule, error) {
	if err := s.validateV2Rule(ctx, &r); err != nil {
		return V2AutomationRule{}, err
	}
	r.UpdatedAt = s.now()
	result, err := s.db.ExecContext(ctx, `UPDATE registry_v2_automation_rules SET enabled=?,source_device_id=?,output_channel=?,event_type=NULLIF(?,''),ignore_retained=?,condition_json=NULLIF(?,''),target_device_id=?,command_type=?,parameters_json=?,updated_at_ns=? WHERE id=? AND deleted_at_ns IS NULL`, boolInt(r.Enabled), r.SourceDeviceID, r.OutputChannel, r.EventType, boolInt(r.IgnoreRetained), r.ConditionJSON, r.TargetDeviceID, r.CommandType, r.ParametersJSON, r.UpdatedAt.UnixNano(), r.ID)
	if err != nil {
		return V2AutomationRule{}, fmt.Errorf("update v2 automation rule: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return V2AutomationRule{}, err
	} else if affected != 1 {
		return V2AutomationRule{}, fmt.Errorf("%w: %s", ErrV2AutomationRuleNotFound, r.ID)
	}
	return r, nil
}

// SetV2AutomationRuleEnabled changes only the enabled flag and server-owned
// update timestamp, so concurrent UI edits cannot overwrite other fields.
func (s *Store) SetV2AutomationRuleEnabled(ctx context.Context, id string, enabled bool) (V2AutomationRule, error) {
	updatedAt := s.now()
	var r V2AutomationRule
	var currentEnabled, ignore int
	var updated int64
	err := s.db.QueryRowContext(ctx, `UPDATE registry_v2_automation_rules SET enabled=?,updated_at_ns=? WHERE id=? AND deleted_at_ns IS NULL RETURNING id,enabled,source_device_id,output_channel,COALESCE(event_type,''),ignore_retained,COALESCE(condition_json,''),target_device_id,command_type,parameters_json,updated_at_ns`, boolInt(enabled), updatedAt.UnixNano(), id).Scan(&r.ID, &currentEnabled, &r.SourceDeviceID, &r.OutputChannel, &r.EventType, &ignore, &r.ConditionJSON, &r.TargetDeviceID, &r.CommandType, &r.ParametersJSON, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return V2AutomationRule{}, fmt.Errorf("%w: %s", ErrV2AutomationRuleNotFound, id)
	}
	if err != nil {
		return V2AutomationRule{}, fmt.Errorf("set v2 automation rule enabled: %w", err)
	}
	r.Enabled = currentEnabled != 0
	r.IgnoreRetained = ignore != 0
	r.UpdatedAt = time.Unix(0, updated).UTC()
	return r, nil
}

// RemoveV2AutomationRule hides a rule from both the API and runtime while
// retaining its row for execution-history foreign keys and deduplication.
func (s *Store) RemoveV2AutomationRule(ctx context.Context, id string) error {
	now := s.now().UnixNano()
	result, err := s.db.ExecContext(ctx, `UPDATE registry_v2_automation_rules SET deleted_at_ns=?,updated_at_ns=? WHERE id=? AND deleted_at_ns IS NULL`, now, now, id)
	if err != nil {
		return fmt.Errorf("remove v2 automation rule: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected != 1 {
		return fmt.Errorf("%w: %s", ErrV2AutomationRuleNotFound, id)
	}
	return nil
}

func (s *Store) validateV2Rule(ctx context.Context, r *V2AutomationRule) error {
	if !v2ID(r.ID) || !v2ID(r.SourceDeviceID) || !v2ID(r.TargetDeviceID) || strings.TrimSpace(r.CommandType) == "" {
		return fmt.Errorf("%w: rule id, source, target and command type are required", ErrV2AutomationRuleInvalid)
	}
	if r.OutputChannel != "telemetry" && r.OutputChannel != "state" && r.OutputChannel != "event" && r.OutputChannel != "command-result" {
		return fmt.Errorf("%w: invalid output channel", ErrV2AutomationRuleInvalid)
	}
	if r.OutputChannel == "event" && strings.TrimSpace(r.EventType) == "" {
		return fmt.Errorf("%w: event trigger requires event_type", ErrV2AutomationRuleInvalid)
	}
	if r.OutputChannel != "event" && r.EventType != "" {
		return fmt.Errorf("%w: event_type is only valid for event trigger", ErrV2AutomationRuleInvalid)
	}
	if r.OutputChannel == "state" && !r.IgnoreRetained {
		return fmt.Errorf("%w: state trigger requires ignore_retained", ErrV2AutomationRuleInvalid)
	}
	if strings.TrimSpace(r.ConditionJSON) != "" {
		if err := automationrule.ValidateCondition([]byte(r.ConditionJSON)); err != nil {
			return fmt.Errorf("%w: invalid rule condition: %v", ErrV2AutomationRuleInvalid, err)
		}
	}
	source, found, err := s.ResolveV2Manifest(ctx, r.SourceDeviceID)
	if err != nil {
		return fmt.Errorf("resolve source v2 manifest: %w", err)
	}
	if !found {
		return fmt.Errorf("%w: source v2 manifest not found", ErrV2AutomationRuleInvalid)
	}
	if _, ok := source.Output(r.OutputChannel); !ok {
		return fmt.Errorf("%w: source does not declare output channel", ErrV2AutomationRuleInvalid)
	}
	if r.OutputChannel == "event" {
		if _, ok := source.Event(r.EventType); !ok {
			return fmt.Errorf("%w: source does not declare event type", ErrV2AutomationRuleInvalid)
		}
	}
	target, found, err := s.ResolveV2Manifest(ctx, r.TargetDeviceID)
	if err != nil {
		return fmt.Errorf("resolve target v2 manifest: %w", err)
	}
	if !found {
		return fmt.Errorf("%w: target v2 manifest not found", ErrV2AutomationRuleInvalid)
	}
	command, ok := target.Command(r.CommandType)
	if !ok {
		return fmt.Errorf("%w: target does not declare command", ErrV2AutomationRuleInvalid)
	}
	canonical, err := devicev2.ValidateFields(command.Parameters, []byte(r.ParametersJSON))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrV2AutomationRuleInvalid, err)
	}
	r.ParametersJSON = string(canonical)
	return nil
}
