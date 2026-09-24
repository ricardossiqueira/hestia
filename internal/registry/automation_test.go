package registry

import (
	"context"
	"testing"
	"time"
)

func TestRecordAutomationEventDedupsByEventID(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	event := AutomationEvent{
		EventID: "b4a5bb31-1710-4f7b-a043-1b6a292d04ad", DeviceID: "led-1", Topic: "devices/led-1/event",
		EventType: "button_pressed", Payload: []byte(`{"type":"button_pressed"}`), OccurredAt: time.Unix(0, 0).UTC(),
	}
	if err := store.RecordAutomationEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	// Redelivery under QoS 1 - the same event_id again must not error or
	// duplicate the row.
	if err := store.RecordAutomationEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM registry_automation_events WHERE event_id = ?`, event.EventID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("registry_automation_events rows for event_id = %d, want 1", count)
	}
}

func TestRecordAutomationCommandResultMatchesKnownCommand(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	command := AutomationCommand{
		CommandID: "a9f2290d-d1ee-4cbc-841d-03e29a7f028c", DeviceID: "led-1", Topic: "devices/led-1/command",
		PublishedAt: time.Unix(1000, 0).UTC(),
	}
	if err := store.RecordAutomationCommand(ctx, command); err != nil {
		t.Fatal(err)
	}

	matched, err := store.RecordAutomationCommandResult(ctx, AutomationCommandResult{
		CommandID: command.CommandID, ResultMessageID: "e9f2290d-d1ee-4cbc-841d-03e29a7f028c",
		Payload: []byte(`{"status":"ok"}`), RecordedAt: time.Unix(1001, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Error("RecordAutomationCommandResult() matched = false, want true")
	}

	var resultMessageID string
	if err := store.db.QueryRowContext(ctx, `SELECT result_message_id FROM registry_automation_commands WHERE command_id = ?`, command.CommandID).Scan(&resultMessageID); err != nil {
		t.Fatal(err)
	}
	if resultMessageID != "e9f2290d-d1ee-4cbc-841d-03e29a7f028c" {
		t.Errorf("result_message_id = %q", resultMessageID)
	}
}

func TestRecordAutomationCommandResultUnknownCommandIsNotAnError(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	matched, err := store.RecordAutomationCommandResult(ctx, AutomationCommandResult{
		CommandID: "00000000-0000-4000-8000-000000000000", ResultMessageID: "e9f2290d-d1ee-4cbc-841d-03e29a7f028c",
		Payload: []byte(`{}`), RecordedAt: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Error("RecordAutomationCommandResult() matched = true for an unknown command_id, want false")
	}
}

func TestAutomationActivityPruningRemovesOldRowsKeepsRecent(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0).UTC()
	store.now = func() time.Time { return base }

	oldEvent := AutomationEvent{EventID: "b4a5bb31-1710-4f7b-a043-1b6a292d04ad", DeviceID: "led-1", Topic: "devices/led-1/event", Payload: []byte(`{}`), OccurredAt: base}
	if err := store.RecordAutomationEvent(ctx, oldEvent); err != nil {
		t.Fatal(err)
	}
	oldCommand := AutomationCommand{CommandID: "a9f2290d-d1ee-4cbc-841d-03e29a7f028c", DeviceID: "led-1", Topic: "devices/led-1/command", PublishedAt: base}
	if err := store.RecordAutomationCommand(ctx, oldCommand); err != nil {
		t.Fatal(err)
	}

	// Move the clock past retention, then write one more row of each kind -
	// pruning runs lazily on every write, so this is what triggers it.
	store.now = func() time.Time { return base.Add(automationActivityRetention + time.Hour) }
	newEvent := AutomationEvent{EventID: "e9f2290d-d1ee-4cbc-841d-03e29a7f028c", DeviceID: "led-1", Topic: "devices/led-1/event", Payload: []byte(`{}`), OccurredAt: store.now()}
	if err := store.RecordAutomationEvent(ctx, newEvent); err != nil {
		t.Fatal(err)
	}
	newCommand := AutomationCommand{CommandID: "c3b5a2f1-0e2a-4a9b-9c1a-7f6b5d4e3c2b", DeviceID: "led-1", Topic: "devices/led-1/command", PublishedAt: store.now()}
	if err := store.RecordAutomationCommand(ctx, newCommand); err != nil {
		t.Fatal(err)
	}

	var eventCount, commandCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM registry_automation_events`).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM registry_automation_commands`).Scan(&commandCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Errorf("registry_automation_events rows = %d, want 1 (old pruned, new kept)", eventCount)
	}
	if commandCount != 1 {
		t.Errorf("registry_automation_commands rows = %d, want 1 (old pruned, new kept)", commandCount)
	}
}
