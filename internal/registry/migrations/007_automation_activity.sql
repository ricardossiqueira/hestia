-- registry_automation_events records every accepted event-kind message
-- (docs/device-manifests.md Marco 5, item 2). event_id is the message's own
-- message_id: item 1 already guarantees an accepted event is never a
-- retained replay, so this is always a genuine transition.
CREATE TABLE registry_automation_events (
    event_id TEXT PRIMARY KEY,
    device_id TEXT NOT NULL,
    topic TEXT NOT NULL,
    event_type TEXT,
    payload BLOB NOT NULL,
    occurred_at_ns INTEGER NOT NULL,
    recorded_at_ns INTEGER NOT NULL
);
CREATE INDEX registry_automation_events_recorded_at ON registry_automation_events(recorded_at_ns);

-- registry_automation_commands records every command the gateway publishes,
-- whether operator/API-initiated (causation_message_id NULL) or fired by a
-- local route in response to an inbound message of any kind - telemetry and
-- state included, not just event (see docs/device-manifests.md Marco 5's
-- future cycle-prevention, which needs the causal chain regardless of what
-- triggered it). No foreign key to registry_automation_events: the causing
-- message may be telemetry/state, which is never persisted there.
CREATE TABLE registry_automation_commands (
    command_id TEXT PRIMARY KEY,
    device_id TEXT NOT NULL,
    topic TEXT NOT NULL,
    route_id TEXT,
    causation_message_id TEXT,
    causation_kind TEXT,
    causation_device_id TEXT,
    published_at_ns INTEGER NOT NULL,
    result_message_id TEXT,
    result_payload BLOB,
    result_recorded_at_ns INTEGER
);
CREATE INDEX registry_automation_commands_published_at ON registry_automation_commands(published_at_ns);
CREATE INDEX registry_automation_commands_causation_message_id ON registry_automation_commands(causation_message_id);
