-- registry_inconsistencies is written by internal/admin when a best-effort
-- compensation (undoing a partially-applied provisioning step) itself
-- fails, leaving the registry and the Mosquitto broker disagreeing about a
-- device. It is deliberately separate from registry_operations, which only
-- ever records the registry's own successful writes for idempotency-key
-- dedup, not cross-system state.
CREATE TABLE IF NOT EXISTS registry_inconsistencies (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    device_id TEXT NOT NULL,
    cause TEXT NOT NULL,
    compensation_error TEXT NOT NULL,
    created_at_ns INTEGER NOT NULL,
    resolved_at_ns INTEGER
);
