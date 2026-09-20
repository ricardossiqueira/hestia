ALTER TABLE outbox_messages ADD COLUMN attempt_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE outbox_messages ADD COLUMN lease_token TEXT NOT NULL DEFAULT '';
ALTER TABLE outbox_messages ADD COLUMN lease_until_ns INTEGER NOT NULL DEFAULT 0;
ALTER TABLE outbox_messages ADD COLUMN next_attempt_ns INTEGER NOT NULL DEFAULT 0;
ALTER TABLE outbox_messages ADD COLUMN last_error_code TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_outbox_messages_delivery
    ON outbox_messages(next_attempt_ns, lease_until_ns, id);
