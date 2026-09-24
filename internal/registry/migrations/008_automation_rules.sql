-- registry_automation_rules models "event -> condition -> action"
-- (docs/device-manifests.md Marco 5, item 3). source_device_id/event_type
-- match the columns registry_automation_events already records an accepted
-- event under, so the future execution engine (item 4) can match a
-- freshly recorded event directly against this table. No revision history:
-- a rule is edited in place and toggled by `enabled`, the same mutability
-- model registry_routes already uses.
CREATE TABLE registry_automation_rules (
    id TEXT PRIMARY KEY,
    enabled INTEGER NOT NULL CHECK(enabled IN (0, 1)),
    source_device_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    condition_json TEXT,
    action_device_id TEXT NOT NULL,
    action_command_type TEXT NOT NULL,
    action_parameters_json TEXT NOT NULL,
    action_schema_validated INTEGER NOT NULL CHECK(action_schema_validated IN (0, 1)),
    created_at_ns INTEGER NOT NULL,
    updated_at_ns INTEGER NOT NULL
);
CREATE INDEX registry_automation_rules_source ON registry_automation_rules(source_device_id, event_type) WHERE enabled = 1;
