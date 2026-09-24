package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// automationActivityRetention bounds how long registry_automation_events and
// registry_automation_commands keep rows, pruned lazily (same shape as
// internal/outbox's MaxAge expiry) on every write. Not exposed via
// gateway.yaml yet - a fixed constant is enough for Marco 5's item 2 and
// keeps this from growing unbounded on the Orange Pi's SD card, given local
// routes can fire every few seconds.
const automationActivityRetention = 30 * 24 * time.Hour

// AutomationEvent is one accepted event-kind message, persisted so a future
// rules engine (Marco 5, items 3+) can evaluate conditions against real
// history. Item 1 already guarantees this is never a retained replay.
type AutomationEvent struct {
	EventID    string
	DeviceID   string
	Topic      string
	EventType  string
	Payload    []byte
	OccurredAt time.Time
}

// AutomationCommand is one command the gateway published: operator/API-
// initiated (RouteID/RuleID/CausationMessageID/CausationKind/
// CausationDeviceID all empty), fired by a local route (RouteID set), or
// fired by an automation rule (RuleID set, Marco 5 item 4) in response to
// an inbound message of any kind.
type AutomationCommand struct {
	CommandID          string
	DeviceID           string
	Topic              string
	RouteID            string
	RuleID             string
	CausationMessageID string
	CausationKind      string
	CausationDeviceID  string
	PublishedAt        time.Time
}

// AutomationCommandResult correlates an inbound command-result message back
// to the command_id it responds to, when the payload carries one (optional -
// no firmware embeds this yet, see docs/api-v1.md).
type AutomationCommandResult struct {
	CommandID       string
	ResultMessageID string
	Payload         []byte
	RecordedAt      time.Time
}

// RecordAutomationEvent persists an accepted event-kind message. Idempotent
// on a duplicate event_id (QoS 1 redelivery), matching internal/outbox's own
// message_id dedup - a duplicate is treated as already recorded, not an
// error.
func (s *Store) RecordAutomationEvent(ctx context.Context, event AutomationEvent) error {
	if s == nil || s.db == nil {
		return errors.New("registry store is closed")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin record automation event: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := pruneAutomationActivity(ctx, tx, s.now()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO registry_automation_events
		(event_id, device_id, topic, event_type, payload, occurred_at_ns, recorded_at_ns)
		VALUES (?, ?, ?, NULLIF(?, ''), ?, ?, ?)`,
		event.EventID, event.DeviceID, event.Topic, event.EventType, event.Payload,
		event.OccurredAt.UnixNano(), s.now().UnixNano()); err != nil {
		return fmt.Errorf("record automation event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit automation event: %w", err)
	}
	return nil
}

// RecordAutomationCommand persists a command the gateway just published.
// command_id is always freshly generated at publish time, so a collision
// would indicate a real bug upstream, not a benign retry.
func (s *Store) RecordAutomationCommand(ctx context.Context, command AutomationCommand) error {
	if s == nil || s.db == nil {
		return errors.New("registry store is closed")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin record automation command: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := pruneAutomationActivity(ctx, tx, s.now()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO registry_automation_commands
		(command_id, device_id, topic, route_id, rule_id, causation_message_id, causation_kind, causation_device_id, published_at_ns)
		VALUES (?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?)`,
		command.CommandID, command.DeviceID, command.Topic, command.RouteID, command.RuleID,
		command.CausationMessageID, command.CausationKind, command.CausationDeviceID,
		command.PublishedAt.UnixNano()); err != nil {
		return fmt.Errorf("record automation command: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit automation command: %w", err)
	}
	return nil
}

// RecordAutomationCommandResult correlates an inbound command-result message
// to the command it responds to. matched is false (not an error) when
// commandID does not match any recorded command - it may belong to a
// long-pruned command, or the payload's command_id may not be one this
// gateway ever published.
func (s *Store) RecordAutomationCommandResult(ctx context.Context, result AutomationCommandResult) (matched bool, err error) {
	if s == nil || s.db == nil {
		return false, errors.New("registry store is closed")
	}
	exec, err := s.db.ExecContext(ctx, `UPDATE registry_automation_commands
		SET result_message_id = ?, result_payload = ?, result_recorded_at_ns = ?
		WHERE command_id = ?`,
		result.ResultMessageID, result.Payload, result.RecordedAt.UnixNano(), result.CommandID)
	if err != nil {
		return false, fmt.Errorf("record automation command result: %w", err)
	}
	count, err := exec.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count automation command result update: %w", err)
	}
	return count > 0, nil
}

func pruneAutomationActivity(ctx context.Context, tx *sql.Tx, now time.Time) error {
	cutoff := now.Add(-automationActivityRetention).UnixNano()
	if _, err := tx.ExecContext(ctx, `DELETE FROM registry_automation_events WHERE recorded_at_ns < ?`, cutoff); err != nil {
		return fmt.Errorf("prune automation events: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM registry_automation_commands WHERE published_at_ns < ?`, cutoff); err != nil {
		return fmt.Errorf("prune automation commands: %w", err)
	}
	return nil
}
