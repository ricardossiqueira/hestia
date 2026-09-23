CREATE TABLE IF NOT EXISTS registry_device_manifests (
    id TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    published_revision INTEGER NOT NULL,
    created_at_ns INTEGER NOT NULL,
    updated_at_ns INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS registry_device_manifest_revisions (
    manifest_id TEXT NOT NULL REFERENCES registry_device_manifests(id) ON DELETE CASCADE,
    revision INTEGER NOT NULL CHECK(revision > 0),
    state TEXT NOT NULL CHECK(state IN ('draft', 'published', 'archived')),
    document_json TEXT NOT NULL,
    created_by TEXT NOT NULL,
    created_at_ns INTEGER NOT NULL,
    PRIMARY KEY(manifest_id, revision)
);

CREATE UNIQUE INDEX IF NOT EXISTS registry_one_published_manifest_revision
    ON registry_device_manifest_revisions(manifest_id)
    WHERE state = 'published';

CREATE TABLE IF NOT EXISTS registry_device_manifest_bindings (
    device_id TEXT PRIMARY KEY REFERENCES registry_devices(id) ON DELETE CASCADE,
    manifest_id TEXT NOT NULL,
    manifest_revision INTEGER NOT NULL,
    FOREIGN KEY(manifest_id, manifest_revision)
        REFERENCES registry_device_manifest_revisions(manifest_id, revision)
);

CREATE TABLE IF NOT EXISTS registry_device_manifest_audit (
    id TEXT PRIMARY KEY,
    manifest_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    action TEXT NOT NULL CHECK(action IN ('seed', 'create_draft', 'publish', 'archive')),
    actor TEXT NOT NULL,
    document_json TEXT NOT NULL,
    created_at_ns INTEGER NOT NULL,
    FOREIGN KEY(manifest_id, revision)
        REFERENCES registry_device_manifest_revisions(manifest_id, revision)
);
