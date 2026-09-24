package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ricardossiqueira/iot-gateway/internal/config"
	"github.com/ricardossiqueira/iot-gateway/internal/devicemanifest"
)

const manifestSeedActor = "system:bootstrap"

// CreateDeviceManifestDraft validates and creates the first editable revision
// of a new manifest. A draft never affects provisioning until Publish is
// called explicitly.
func (s *Store) CreateDeviceManifestDraft(ctx context.Context, document, actor string) (DeviceManifest, error) {
	parsed, canonical, err := devicemanifest.Parse(document)
	if err != nil {
		return DeviceManifest{}, err
	}
	actor, err = validateManifestActor(actor)
	if err != nil {
		return DeviceManifest{}, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return DeviceManifest{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO registry_device_manifests
		(id, display_name, published_revision, created_at_ns, updated_at_ns) VALUES (?, ?, 0, ?, ?)`,
		parsed.ID, parsed.DisplayName, s.now().UnixNano(), s.now().UnixNano()); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return DeviceManifest{}, fmt.Errorf("%w: %q", ErrManifestAlreadyExists, parsed.ID)
		}
		return DeviceManifest{}, fmt.Errorf("create device manifest %q: %w", parsed.ID, err)
	}
	if err := insertManifestRevision(ctx, tx, parsed.ID, 1, "draft", canonical, actor, s.now().UnixNano()); err != nil {
		return DeviceManifest{}, err
	}
	if err := insertManifestAudit(ctx, tx, parsed.ID, 1, "create_draft", actor, canonical, nil, s.now().UnixNano()); err != nil {
		return DeviceManifest{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeviceManifest{}, fmt.Errorf("commit device manifest draft: %w", err)
	}
	return DeviceManifest{ID: parsed.ID, DisplayName: parsed.DisplayName, Revision: 1, Document: canonical, CreatedBy: actor, CreatedAt: s.now()}, nil
}

// CreateDeviceManifestRevisionDraft adds a new draft to an existing manifest.
// The document ID is immutable: changing it would make instances and audit
// history point at two unrelated device families.
func (s *Store) CreateDeviceManifestRevisionDraft(ctx context.Context, manifestID, document, actor string) (DeviceManifest, error) {
	parsed, canonical, err := devicemanifest.Parse(document)
	if err != nil {
		return DeviceManifest{}, err
	}
	if parsed.ID != strings.TrimSpace(manifestID) {
		return DeviceManifest{}, errors.New("manifest document id must match manifest_id")
	}
	actor, err = validateManifestActor(actor)
	if err != nil {
		return DeviceManifest{}, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return DeviceManifest{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var previousRevision uint64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(revision) FROM registry_device_manifest_revisions WHERE manifest_id = ?`, manifestID).Scan(&previousRevision); err != nil {
		return DeviceManifest{}, fmt.Errorf("read manifest revision: %w", err)
	}
	if previousRevision == 0 {
		return DeviceManifest{}, fmt.Errorf("%w: %q", ErrManifestNotFound, manifestID)
	}
	revision := previousRevision + 1
	if err := insertManifestRevision(ctx, tx, manifestID, revision, "draft", canonical, actor, s.now().UnixNano()); err != nil {
		return DeviceManifest{}, err
	}
	previous := previousRevision
	if err := insertManifestAudit(ctx, tx, manifestID, revision, "create_draft", actor, canonical, &previous, s.now().UnixNano()); err != nil {
		return DeviceManifest{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeviceManifest{}, fmt.Errorf("commit device manifest draft: %w", err)
	}
	return DeviceManifest{ID: manifestID, DisplayName: parsed.DisplayName, Revision: revision, Document: canonical, CreatedBy: actor, CreatedAt: s.now()}, nil
}

// PublishDeviceManifest promotes exactly one validated draft. The old
// published revision is archived in the same transaction; active device
// bindings remain pinned to their historic revision.
func (s *Store) PublishDeviceManifest(ctx context.Context, manifestID string, revision uint64, actor string) (DeviceManifest, error) {
	actor, err := validateManifestActor(actor)
	if err != nil {
		return DeviceManifest{}, err
	}
	if revision == 0 {
		return DeviceManifest{}, errors.New("manifest revision is required")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return DeviceManifest{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var state, document string
	if err := tx.QueryRowContext(ctx, `SELECT state, document_json FROM registry_device_manifest_revisions
		WHERE manifest_id = ? AND revision = ?`, manifestID, revision).Scan(&state, &document); errors.Is(err, sql.ErrNoRows) {
		return DeviceManifest{}, fmt.Errorf("%w: %q revision %d", ErrManifestRevisionNotFound, manifestID, revision)
	} else if err != nil {
		return DeviceManifest{}, fmt.Errorf("read device manifest revision: %w", err)
	}
	if state != "draft" {
		return DeviceManifest{}, fmt.Errorf("%w: %q revision %d", ErrManifestRevisionNotDraft, manifestID, revision)
	}
	parsed, _, err := devicemanifest.Parse(document)
	if err != nil {
		return DeviceManifest{}, fmt.Errorf("stored device manifest is invalid: %w", err)
	}
	var previousRevision uint64
	if err := tx.QueryRowContext(ctx, `SELECT published_revision FROM registry_device_manifests WHERE id = ?`, manifestID).Scan(&previousRevision); errors.Is(err, sql.ErrNoRows) {
		return DeviceManifest{}, fmt.Errorf("%w: %q", ErrManifestNotFound, manifestID)
	} else if err != nil {
		return DeviceManifest{}, fmt.Errorf("read device manifest: %w", err)
	}
	if previousRevision != 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE registry_device_manifest_revisions SET state = 'archived'
			WHERE manifest_id = ? AND revision = ?`, manifestID, previousRevision); err != nil {
			return DeviceManifest{}, fmt.Errorf("archive old device manifest revision: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE registry_device_manifest_revisions SET state = 'published'
		WHERE manifest_id = ? AND revision = ?`, manifestID, revision); err != nil {
		return DeviceManifest{}, fmt.Errorf("publish device manifest revision: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE registry_device_manifests
		SET display_name = ?, published_revision = ?, updated_at_ns = ? WHERE id = ?`, parsed.DisplayName, revision, s.now().UnixNano(), manifestID); err != nil {
		return DeviceManifest{}, fmt.Errorf("update published device manifest: %w", err)
	}
	var previous *uint64
	if previousRevision != 0 {
		previous = &previousRevision
	}
	if err := insertManifestAudit(ctx, tx, manifestID, revision, "publish", actor, document, previous, s.now().UnixNano()); err != nil {
		return DeviceManifest{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeviceManifest{}, fmt.Errorf("commit device manifest publication: %w", err)
	}
	return DeviceManifest{ID: manifestID, DisplayName: parsed.DisplayName, Revision: revision, Document: document, CreatedBy: actor, CreatedAt: s.now()}, nil
}

// ListPublishedManifests returns the catalog available to provisioners and the
// future text editor. Drafts are intentionally not visible here: a partially
// edited document must never become a provisioning policy by accident.
func (s *Store) ListPublishedManifests(ctx context.Context) ([]DeviceManifest, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("registry store is closed")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.display_name, r.revision, r.document_json, r.created_by, r.created_at_ns
		FROM registry_device_manifests m
		JOIN registry_device_manifest_revisions r
		  ON r.manifest_id = m.id AND r.revision = m.published_revision
		WHERE r.state = 'published'
		ORDER BY m.id`)
	if err != nil {
		return nil, fmt.Errorf("list published device manifests: %w", err)
	}
	defer rows.Close()
	manifests := make([]DeviceManifest, 0)
	for rows.Next() {
		item, err := scanDeviceManifest(rows)
		if err != nil {
			return nil, err
		}
		manifests = append(manifests, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate published device manifests: %w", err)
	}
	return manifests, nil
}

// GetPublishedManifest reads exactly one published revision by its stable ID.
func (s *Store) GetPublishedManifest(ctx context.Context, id string) (DeviceManifest, error) {
	if s == nil || s.db == nil {
		return DeviceManifest{}, errors.New("registry store is closed")
	}
	row := s.db.QueryRowContext(ctx, `SELECT m.id, m.display_name, r.revision, r.document_json, r.created_by, r.created_at_ns
		FROM registry_device_manifests m
		JOIN registry_device_manifest_revisions r
		  ON r.manifest_id = m.id AND r.revision = m.published_revision
		WHERE m.id = ? AND r.state = 'published'`, strings.TrimSpace(id))
	item, err := scanDeviceManifest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return DeviceManifest{}, fmt.Errorf("%w: %q", ErrManifestNotFound, id)
	}
	if err != nil {
		return DeviceManifest{}, err
	}
	return item, nil
}

// BindDeviceManifest links an existing instance to an immutable published
// revision. It is intentionally separate from AddDevice until the generic
// provisioner replaces the legacy device-specific flows.
func (s *Store) BindDeviceManifest(ctx context.Context, deviceID, manifestID string, revision uint64) error {
	if s == nil || s.db == nil {
		return errors.New("registry store is closed")
	}
	if strings.TrimSpace(deviceID) == "" || strings.TrimSpace(manifestID) == "" || revision == 0 {
		return errors.New("device id, manifest id and manifest revision are required")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var published int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM registry_device_manifest_revisions
		WHERE manifest_id = ? AND revision = ? AND state = 'published'`, manifestID, revision).Scan(&published); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %q revision %d", ErrManifestNotFound, manifestID, revision)
	} else if err != nil {
		return fmt.Errorf("read published device manifest: %w", err)
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM registry_devices WHERE id = ?`, deviceID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %q", ErrDeviceNotFound, deviceID)
	} else if err != nil {
		return fmt.Errorf("read registry device: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO registry_device_manifest_bindings(device_id, manifest_id, manifest_revision)
		VALUES (?, ?, ?)
		ON CONFLICT(device_id) DO UPDATE SET manifest_id = excluded.manifest_id, manifest_revision = excluded.manifest_revision`,
		deviceID, manifestID, revision); err != nil {
		return fmt.Errorf("bind device manifest: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit device manifest binding: %w", err)
	}
	return nil
}

// ListDeviceManifestBindings returns the immutable manifest revision attached
// to each provisioned instance. Devices created by legacy flows intentionally
// have no row and are therefore absent from this result.
func (s *Store) ListDeviceManifestBindings(ctx context.Context) ([]DeviceManifestBinding, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("registry store is closed")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT device_id, manifest_id, manifest_revision
		FROM registry_device_manifest_bindings ORDER BY device_id`)
	if err != nil {
		return nil, fmt.Errorf("list device manifest bindings: %w", err)
	}
	defer rows.Close()
	bindings := make([]DeviceManifestBinding, 0)
	for rows.Next() {
		var binding DeviceManifestBinding
		if err := rows.Scan(&binding.DeviceID, &binding.ManifestID, &binding.ManifestRevision); err != nil {
			return nil, fmt.Errorf("scan device manifest binding: %w", err)
		}
		bindings = append(bindings, binding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate device manifest bindings: %w", err)
	}
	return bindings, nil
}

// ResolveDeviceManifest reads the exact revision pinned to a device. It may
// be archived today, which is intentional: later publications must not alter
// command policy for an existing device implicitly.
func (s *Store) ResolveDeviceManifest(ctx context.Context, deviceID string) (devicemanifest.Document, bool, error) {
	var document string
	err := s.db.QueryRowContext(ctx, `SELECT r.document_json FROM registry_device_manifest_bindings b
		JOIN registry_device_manifest_revisions r ON r.manifest_id = b.manifest_id AND r.revision = b.manifest_revision
		WHERE b.device_id = ?`, strings.TrimSpace(deviceID)).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return devicemanifest.Document{}, false, nil
	}
	if err != nil {
		return devicemanifest.Document{}, false, fmt.Errorf("resolve device manifest: %w", err)
	}
	parsed, _, err := devicemanifest.Parse(document)
	if err != nil {
		return devicemanifest.Document{}, false, fmt.Errorf("stored device manifest is invalid: %w", err)
	}
	return parsed, true, nil
}

// MigrateDeviceToManifest binds a pre-manifest device to a pinned, published
// manifest. It never changes a broker credential or device NVS.
func (s *Store) MigrateDeviceToManifest(ctx context.Context, deviceID, manifestID, actor string) (config.Device, error) {
	actor, err := validateManifestActor(actor)
	if err != nil {
		return config.Device{}, err
	}
	manifest, err := s.GetPublishedManifest(ctx, manifestID)
	if err != nil {
		return config.Device{}, err
	}
	document, _, err := devicemanifest.Parse(manifest.Document)
	if err != nil {
		return config.Device{}, err
	}
	snapshot, err := s.Snapshot(ctx)
	if err != nil {
		return config.Device{}, err
	}
	var device config.Device
	found := false
	for _, candidate := range snapshot.Devices {
		if candidate.ID == deviceID {
			device, found = candidate, true
			break
		}
	}
	if !found {
		return config.Device{}, fmt.Errorf("%w: %q", ErrDeviceNotFound, deviceID)
	}
	var bound int
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM registry_device_manifest_bindings WHERE device_id = ?)`, deviceID).Scan(&bound); err != nil {
		return config.Device{}, fmt.Errorf("check device manifest binding: %w", err)
	}
	if bound != 0 {
		return config.Device{}, errors.New("device is already manifest-managed")
	}
	for _, topic := range document.MQTT.Topics {
		if !deviceHasTopic(device, topic) {
			return config.Device{}, fmt.Errorf("device topic %q is incompatible with manifest", topic)
		}
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return config.Device{}, err
	}
	defer func() { _ = tx.Rollback() }()
	revision, err := nextRevision(ctx, tx)
	if err != nil {
		return config.Device{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE registry_devices SET revision=?, updated_at_ns=? WHERE id=?`, revision, s.now().UnixNano(), deviceID); err != nil {
		return config.Device{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO registry_device_manifest_bindings(device_id,manifest_id,manifest_revision) VALUES (?,?,?)`, deviceID, manifest.ID, manifest.Revision); err != nil {
		return config.Device{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO registry_device_manifest_binding_audit(id,device_id,manifest_id,manifest_revision,actor,created_at_ns) VALUES(lower(hex(randomblob(16))),?,?,?,?,?)`, deviceID, manifest.ID, manifest.Revision, actor, s.now().UnixNano()); err != nil {
		return config.Device{}, err
	}
	if err = tx.Commit(); err != nil {
		return config.Device{}, err
	}
	return device, nil
}

func deviceHasTopic(device config.Device, kind string) bool {
	switch kind {
	case "telemetry":
		return device.Topics.Telemetry != ""
	case "state":
		return device.Topics.State != ""
	case "event":
		return device.Topics.Event != ""
	case "command":
		return device.Topics.Command != ""
	case "command-result":
		return device.Topics.CommandResult != ""
	}
	return false
}

func (s *Store) seedDefaultManifests(ctx context.Context) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, document := range devicemanifest.DefaultDocuments {
		parsed, canonical, err := devicemanifest.Parse(document)
		if err != nil {
			return fmt.Errorf("validate default device manifest: %w", err)
		}
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO registry_device_manifests
			(id, display_name, published_revision, created_at_ns, updated_at_ns) VALUES (?, ?, 1, ?, ?)`,
			parsed.ID, parsed.DisplayName, s.now().UnixNano(), s.now().UnixNano())
		if err != nil {
			return fmt.Errorf("seed device manifest %q: %w", parsed.ID, err)
		}
		created, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if created == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO registry_device_manifest_revisions
			(manifest_id, revision, state, document_json, created_by, created_at_ns)
			VALUES (?, 1, 'published', ?, ?, ?)`, parsed.ID, canonical, manifestSeedActor, s.now().UnixNano()); err != nil {
			return fmt.Errorf("seed device manifest revision %q: %w", parsed.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO registry_device_manifest_audit
			(id, manifest_id, revision, action, actor, document_json, created_at_ns)
			VALUES (lower(hex(randomblob(16))), ?, 1, 'seed', ?, ?, ?)`,
			parsed.ID, manifestSeedActor, canonical, s.now().UnixNano()); err != nil {
			return fmt.Errorf("audit seed device manifest %q: %w", parsed.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit device manifest seeds: %w", err)
	}
	return nil
}

type manifestScanner interface {
	Scan(dest ...any) error
}

func scanDeviceManifest(scanner manifestScanner) (DeviceManifest, error) {
	var item DeviceManifest
	var revision uint64
	var createdAtNS int64
	if err := scanner.Scan(&item.ID, &item.DisplayName, &revision, &item.Document, &item.CreatedBy, &createdAtNS); err != nil {
		return DeviceManifest{}, err
	}
	item.Revision = revision
	item.CreatedAt = time.Unix(0, createdAtNS).UTC()
	return item, nil
}

func insertManifestRevision(ctx context.Context, tx *sql.Tx, manifestID string, revision uint64, state, document, actor string, createdAtNS int64) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO registry_device_manifest_revisions
		(manifest_id, revision, state, document_json, created_by, created_at_ns)
		VALUES (?, ?, ?, ?, ?, ?)`, manifestID, revision, state, document, actor, createdAtNS); err != nil {
		return fmt.Errorf("insert device manifest revision: %w", err)
	}
	return nil
}

func insertManifestAudit(ctx context.Context, tx *sql.Tx, manifestID string, revision uint64, action, actor, document string, previous *uint64, createdAtNS int64) error {
	var previousValue any
	if previous != nil {
		previousValue = *previous
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO registry_device_manifest_audit
		(id, manifest_id, revision, action, actor, document_json, created_at_ns, previous_revision)
		VALUES (lower(hex(randomblob(16))), ?, ?, ?, ?, ?, ?, ?)`,
		manifestID, revision, action, actor, document, createdAtNS, previousValue); err != nil {
		return fmt.Errorf("audit device manifest revision: %w", err)
	}
	return nil
}

func validateManifestActor(actor string) (string, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" || len(actor) > 120 || strings.ContainsAny(actor, "\r\n") {
		return "", errors.New("manifest actor must be between 1 and 120 printable characters")
	}
	return actor, nil
}
