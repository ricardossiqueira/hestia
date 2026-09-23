package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ricardossiqueira/iot-gateway/internal/devicemanifest"
)

const manifestSeedActor = "system:bootstrap"

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
