-- Device Platform v2 is intentionally separate from the v1 registry. The
-- v1 tables can be deleted with the planned reset; keeping a new namespace in
-- this vertical slice avoids giving an existing v1 identity broader access.
CREATE TABLE IF NOT EXISTS registry_v2_manifests (
    manifest_id TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK(revision > 0),
    manifest_sha256 TEXT NOT NULL,
    document_json TEXT NOT NULL,
    issuer_fingerprint TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL CHECK(state IN ('accepted', 'archived')),
    created_at_ns INTEGER NOT NULL,
    PRIMARY KEY (manifest_id, revision),
    UNIQUE (manifest_sha256)
);
CREATE UNIQUE INDEX IF NOT EXISTS registry_v2_manifests_one_accepted
    ON registry_v2_manifests(manifest_id) WHERE state = 'accepted';

CREATE TABLE IF NOT EXISTS registry_v2_devices (
    device_id TEXT PRIMARY KEY,
    device_uid TEXT NOT NULL UNIQUE,
    manifest_id TEXT NOT NULL,
    manifest_revision INTEGER NOT NULL,
    manifest_sha256 TEXT NOT NULL,
    firmware_version TEXT NOT NULL,
    identity_public_key TEXT NOT NULL,
    desired_state TEXT NOT NULL CHECK(desired_state IN ('pending', 'active', 'disabled', 'removed')),
    active_state TEXT NOT NULL,
    created_at_ns INTEGER NOT NULL,
    updated_at_ns INTEGER NOT NULL,
    FOREIGN KEY (manifest_id, manifest_revision) REFERENCES registry_v2_manifests(manifest_id, revision)
);
