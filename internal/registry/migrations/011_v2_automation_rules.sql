CREATE TABLE IF NOT EXISTS registry_v2_automation_rules (
    id TEXT PRIMARY KEY,
    enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),
    source_device_id TEXT NOT NULL REFERENCES registry_v2_devices(device_id),
    output_channel TEXT NOT NULL CHECK(output_channel IN ('telemetry','state','event','command-result')),
    event_type TEXT,
    ignore_retained INTEGER NOT NULL CHECK(ignore_retained IN (0,1)),
    condition_json TEXT,
    target_device_id TEXT NOT NULL REFERENCES registry_v2_devices(device_id),
    command_type TEXT NOT NULL,
    parameters_json TEXT NOT NULL,
    updated_at_ns INTEGER NOT NULL
);
