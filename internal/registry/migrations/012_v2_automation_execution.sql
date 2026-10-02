-- A reservation is created before publishing an action.  The unique key is
-- the v2 exactly-once boundary: the same accepted MQTT message cannot cause
-- the same automation rule to publish twice, including after reconnects.
CREATE TABLE IF NOT EXISTS registry_v2_automation_executions (
    rule_id TEXT NOT NULL REFERENCES registry_v2_automation_rules(id),
    causation_message_id TEXT NOT NULL,
    command_id TEXT NOT NULL,
    target_device_id TEXT NOT NULL REFERENCES registry_v2_devices(device_id),
    status TEXT NOT NULL CHECK(status IN ('reserved', 'published', 'failed')),
    created_at_ns INTEGER NOT NULL,
    completed_at_ns INTEGER,
    PRIMARY KEY (rule_id, causation_message_id)
);
CREATE INDEX IF NOT EXISTS registry_v2_automation_executions_rate
    ON registry_v2_automation_executions(rule_id, status, completed_at_ns);
