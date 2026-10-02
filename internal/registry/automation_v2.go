package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

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
