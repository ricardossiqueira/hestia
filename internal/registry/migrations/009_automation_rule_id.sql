-- rule_id ties a published command back to the automation rule that fired
-- it (Marco 5, item 4), separate from route_id (local routes) and NULL for
-- operator/API-initiated commands. Used both for dedup (has this exact
-- event already fired this rule?) and for the per-rule firing-rate limit
-- that stands in for true cycle detection (the protocol has no way to
-- prove an event was caused by an earlier command - see
-- docs/device-manifests.md Marco 5's cycle-prevention note).
ALTER TABLE registry_automation_commands ADD COLUMN rule_id TEXT;
CREATE INDEX registry_automation_commands_rule_causation ON registry_automation_commands(rule_id, causation_message_id);
CREATE INDEX registry_automation_commands_rule_published ON registry_automation_commands(rule_id, published_at_ns);
