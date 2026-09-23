CREATE TABLE IF NOT EXISTS registry_device_manifest_binding_audit (
    id TEXT PRIMARY KEY,
    device_id TEXT NOT NULL,
    manifest_id TEXT NOT NULL,
    manifest_revision INTEGER NOT NULL,
    previous_profile TEXT NOT NULL,
    actor TEXT NOT NULL,
    created_at_ns INTEGER NOT NULL
);
