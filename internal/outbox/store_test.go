package outbox

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenEnqueueAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gateway.db")
	options := testOptions()
	store, err := Open(ctx, path, options)
	if err != nil {
		t.Fatal(err)
	}
	message := testMessage("message-1", Telemetry, []byte(`{"temperature_c":24.6}`))
	result, err := store.Enqueue(ctx, message)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Stored || result.Duplicate || result.DiscardReason != "" {
		t.Fatalf("Enqueue() = %#v", result)
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Messages != 1 || stats.PayloadBytes != int64(len(message.Payload)) {
		t.Errorf("Stats() = %#v", stats)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(ctx, path, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	stats, err = store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Messages != 1 || stats.PayloadBytes != int64(len(message.Payload)) {
		t.Errorf("Stats() after reopen = %#v", stats)
	}
}

func TestEnqueueDeduplicatesMessageID(t *testing.T) {
	store := openTestStore(t, testOptions())
	ctx := context.Background()
	message := testMessage("same-id", Telemetry, []byte(`{"value":1}`))
	if _, err := store.Enqueue(ctx, message); err != nil {
		t.Fatal(err)
	}
	result, err := store.Enqueue(ctx, message)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Stored || !result.Duplicate {
		t.Errorf("duplicate result = %#v", result)
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Messages != 1 {
		t.Errorf("messages = %d, want 1", stats.Messages)
	}
}

func TestEnqueueExpiresByEnqueuedTimeNotPayloadTime(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	options := testOptions()
	options.Now = func() time.Time { return now }
	options.MaxAge = time.Hour
	store := openTestStore(t, options)
	ctx := context.Background()
	old := testMessage("old", Telemetry, []byte(`{"timestamp":"2099-01-01T00:00:00Z"}`))
	if _, err := store.Enqueue(ctx, old); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour + time.Nanosecond)
	if _, err := store.Enqueue(ctx, testMessage("new", Event, []byte(`{}`))); err != nil {
		t.Fatal(err)
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Messages != 1 {
		t.Errorf("messages after expiry = %d, want 1", stats.Messages)
	}
}

func TestSnapshotIsReadOnlyAndExcludesExpiredMessages(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	options := testOptions()
	options.MaxAge = time.Hour
	options.Now = func() time.Time { return now }
	store := openTestStore(t, options)
	ctx := context.Background()
	if _, err := store.Enqueue(ctx, testMessage("old", Telemetry, []byte(`{"n":1}`))); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)

	before := store.messageIDs(t)
	snapshot, err := store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Messages != 0 || snapshot.PayloadBytes != 0 {
		t.Errorf("Snapshot() = %#v", snapshot)
	}
	if snapshot.OldestEnqueuedAt != nil {
		t.Errorf("oldest = %v, want nil", snapshot.OldestEnqueuedAt)
	}
	if after := store.messageIDs(t); after != before {
		t.Errorf("Snapshot modified database: before %q after %q", before, after)
	}
}

func TestSnapshotReportsOldestLogicalMessage(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	options := testOptions()
	options.Now = func() time.Time { return now }
	store := openTestStore(t, options)
	if _, err := store.Enqueue(context.Background(), testMessage("first", Telemetry, []byte(`{"n":1}`))); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err := store.Enqueue(context.Background(), testMessage("second", Event, []byte(`{"n":2}`))); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Messages != 2 || snapshot.OldestEnqueuedAt == nil || !snapshot.OldestEnqueuedAt.Equal(now.Add(-time.Minute)) {
		t.Errorf("Snapshot() = %#v", snapshot)
	}
}

func TestEnqueueEvictsLowerPriorityFirst(t *testing.T) {
	options := testOptions()
	options.MaxMessages = 2
	options.MaxBytes = 1024
	store := openTestStore(t, options)
	ctx := context.Background()
	if _, err := store.Enqueue(ctx, testMessage("telemetry", Telemetry, []byte(`{"n":1}`))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enqueue(ctx, testMessage("state", State, []byte(`{"n":2}`))); err != nil {
		t.Fatal(err)
	}
	result, err := store.Enqueue(ctx, testMessage("event", Event, []byte(`{"n":3}`)))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Stored || result.Evicted != 1 {
		t.Fatalf("event result = %#v", result)
	}
	if got := store.messageIDs(t); got != "event,state" {
		t.Errorf("message IDs = %q, want event,state", got)
	}
}

func TestEnqueueDoesNotDisplaceHigherPriorityMessage(t *testing.T) {
	options := testOptions()
	options.MaxMessages = 1
	options.MaxBytes = 1024
	store := openTestStore(t, options)
	ctx := context.Background()
	if _, err := store.Enqueue(ctx, testMessage("event", Event, []byte(`{"n":1}`))); err != nil {
		t.Fatal(err)
	}
	result, err := store.Enqueue(ctx, testMessage("telemetry", Telemetry, []byte(`{"n":2}`)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Stored || result.DiscardReason == "" {
		t.Errorf("telemetry result = %#v, want discarded", result)
	}
	if got := store.messageIDs(t); got != "event" {
		t.Errorf("message IDs = %q, want event", got)
	}
}

func TestEnqueueHonorsPayloadByteLimit(t *testing.T) {
	options := testOptions()
	options.MaxMessages = 10
	options.MaxBytes = 10
	store := openTestStore(t, options)
	result, err := store.Enqueue(context.Background(), testMessage("large", Event, []byte(`01234567890`)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Stored || result.DiscardReason != "message exceeds max_outbox_bytes" {
		t.Errorf("result = %#v", result)
	}
}

func openTestStore(t *testing.T, options Options) *Store {
	t.Helper()
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "gateway.db"), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func testOptions() Options {
	return Options{MaxMessages: 100, MaxBytes: 1024 * 1024, MaxAge: 24 * time.Hour}
}

func testMessage(id string, kind Kind, payload []byte) Message {
	return Message{MessageID: id, DeviceID: "device-1", Topic: "devices/device-1/" + string(kind), Kind: kind, Payload: payload}
}

func (s *Store) messageIDs(t *testing.T) string {
	t.Helper()
	rows, err := s.db.Query(`SELECT message_id FROM outbox_messages ORDER BY message_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ids) == 0 {
		return ""
	}
	result := ids[0]
	for _, id := range ids[1:] {
		result += "," + id
	}
	return result
}
