CREATE TABLE IF NOT EXISTS registry_meta (
    key TEXT PRIMARY KEY,
    value INTEGER NOT NULL
);

INSERT OR IGNORE INTO registry_meta(key, value) VALUES ('revision', 0);

CREATE TABLE IF NOT EXISTS registry_devices (
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL,
    profile TEXT NOT NULL DEFAULT '',
    enabled INTEGER NOT NULL CHECK(enabled IN (0, 1)),
    revision INTEGER NOT NULL,
    created_at_ns INTEGER NOT NULL,
    updated_at_ns INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS registry_device_topics (
    device_id TEXT NOT NULL REFERENCES registry_devices(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK(kind IN ('telemetry', 'state', 'event', 'command', 'command_result')),
    topic TEXT NOT NULL UNIQUE,
    PRIMARY KEY(device_id, kind)
);

CREATE TABLE IF NOT EXISTS registry_device_forwarding (
    device_id TEXT PRIMARY KEY REFERENCES registry_devices(id) ON DELETE CASCADE,
    telemetry_to_vps INTEGER NOT NULL CHECK(telemetry_to_vps IN (0, 1)),
    state_to_vps INTEGER NOT NULL CHECK(state_to_vps IN (0, 1)),
    events_to_vps INTEGER NOT NULL CHECK(events_to_vps IN (0, 1)),
    commands_from_vps INTEGER NOT NULL CHECK(commands_from_vps IN (0, 1))
);

CREATE TABLE IF NOT EXISTS registry_routes (
    id TEXT PRIMARY KEY,
    source_topic TEXT NOT NULL,
    destination_topic TEXT NOT NULL,
    transform_type TEXT NOT NULL,
    command_type TEXT NOT NULL,
    qos INTEGER NOT NULL CHECK(qos BETWEEN 0 AND 2),
    retain INTEGER NOT NULL CHECK(retain IN (0, 1)),
    revision INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS registry_operations (
    id TEXT PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE,
    kind TEXT NOT NULL,
    device_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('applied', 'failed')),
    revision INTEGER NOT NULL,
    error TEXT NOT NULL DEFAULT '',
    created_at_ns INTEGER NOT NULL,
    updated_at_ns INTEGER NOT NULL
);
