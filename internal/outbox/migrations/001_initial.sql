CREATE TABLE IF NOT EXISTS outbox_messages (
    id INTEGER PRIMARY KEY,
    message_id TEXT NOT NULL UNIQUE,
    device_id TEXT NOT NULL,
    topic TEXT NOT NULL,
    kind TEXT NOT NULL CHECK(kind IN ('telemetry', 'state', 'event')),
    payload BLOB NOT NULL,
    payload_bytes INTEGER NOT NULL CHECK(payload_bytes >= 0),
    enqueued_at_ns INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_outbox_messages_enqueued_at
    ON outbox_messages(enqueued_at_ns);

CREATE INDEX IF NOT EXISTS idx_outbox_messages_eviction
    ON outbox_messages(kind, enqueued_at_ns, id);
