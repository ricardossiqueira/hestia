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
	if err := s.validateV2Rule(ctx, &r); err != nil {
		return V2AutomationRule{}, err
	}
	r.UpdatedAt = s.now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO registry_v2_automation_rules(id,enabled,source_device_id,output_channel,event_type,ignore_retained,condition_json,target_device_id,command_type,parameters_json,updated_at_ns) VALUES(?,?,?,?,NULLIF(?,''),?,NULLIF(?,''),?,?,?,?)`, r.ID, boolInt(r.Enabled), r.SourceDeviceID, r.OutputChannel, r.EventType, boolInt(r.IgnoreRetained), r.ConditionJSON, r.TargetDeviceID, r.CommandType, r.ParametersJSON, r.UpdatedAt.UnixNano())
	if err != nil {
		return V2AutomationRule{}, fmt.Errorf("create v2 automation rule: %w", err)
	}
	return r, nil
}
func (s *Store) ListV2AutomationRules(ctx context.Context) ([]V2AutomationRule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,enabled,source_device_id,output_channel,COALESCE(event_type,''),ignore_retained,COALESCE(condition_json,''),target_device_id,command_type,parameters_json,updated_at_ns FROM registry_v2_automation_rules ORDER BY id`)
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
func (s *Store) validateV2Rule(ctx context.Context, r *V2AutomationRule) error {
	if !v2ID(r.ID) || !v2ID(r.SourceDeviceID) || !v2ID(r.TargetDeviceID) || strings.TrimSpace(r.CommandType) == "" {
		return errors.New("rule id, source, target and command type are required")
	}
	if r.OutputChannel != "telemetry" && r.OutputChannel != "state" && r.OutputChannel != "event" && r.OutputChannel != "command-result" {
		return errors.New("invalid output channel")
	}
	if r.OutputChannel == "event" && strings.TrimSpace(r.EventType) == "" {
		return errors.New("event trigger requires event_type")
	}
	if r.OutputChannel != "event" && r.EventType != "" {
		return errors.New("event_type is only valid for event trigger")
	}
	if r.OutputChannel == "state" && !r.IgnoreRetained {
		return errors.New("state trigger requires ignore_retained")
	}
	if strings.TrimSpace(r.ConditionJSON) != "" {
		if err := automationrule.ValidateCondition([]byte(r.ConditionJSON)); err != nil {
			return fmt.Errorf("invalid rule condition: %w", err)
		}
	}
	source, found, err := s.ResolveV2Manifest(ctx, r.SourceDeviceID)
	if err != nil || !found {
		return errors.New("source v2 manifest not found")
	}
	if _, ok := source.Output(r.OutputChannel); !ok {
		return errors.New("source does not declare output channel")
	}
	if r.OutputChannel == "event" {
		if _, ok := source.Event(r.EventType); !ok {
			return errors.New("source does not declare event type")
		}
	}
	target, found, err := s.ResolveV2Manifest(ctx, r.TargetDeviceID)
	if err != nil || !found {
		return errors.New("target v2 manifest not found")
	}
	command, ok := target.Command(r.CommandType)
	if !ok {
		return errors.New("target does not declare command")
	}
	canonical, err := devicev2.ValidateFields(command.Parameters, []byte(r.ParametersJSON))
	if err != nil {
		return err
	}
	r.ParametersJSON = string(canonical)
	return nil
}

var _ = sql.ErrNoRows
