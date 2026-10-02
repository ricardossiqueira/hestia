package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ricardossiqueira/iot-gateway/internal/devicev2"
)

var (
	ErrV2DeviceAlreadyExists = errors.New("v2 device already exists")
	ErrV2DeviceNotFound      = errors.New("v2 device not found")
)

type V2Manifest struct {
	ManifestID        string
	Revision          uint64
	SHA256            string
	Document          string
	IssuerFingerprint string
	State             string
	CreatedAt         time.Time
}
type V2Device struct {
	DeviceID          string
	DeviceUID         string
	ManifestID        string
	ManifestRevision  uint64
	ManifestSHA256    string
	FirmwareVersion   string
	IdentityPublicKey string
	DesiredState      string
	ActiveState       string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// AcceptV2Manifest validates and stores the exact firmware-provided contract.
// A known hash is reused; a changed hash creates the next immutable revision.
func (s *Store) AcceptV2Manifest(ctx context.Context, raw, issuerFingerprint string) (V2Manifest, error) {
	if s == nil || s.db == nil {
		return V2Manifest{}, errors.New("registry store is closed")
	}
	parsed, canonical, hash, err := devicev2.Parse(raw)
	if err != nil {
		return V2Manifest{}, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return V2Manifest{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var existing V2Manifest
	err = tx.QueryRowContext(ctx, `SELECT manifest_id,revision,manifest_sha256,document_json,issuer_fingerprint,state,created_at_ns FROM registry_v2_manifests WHERE manifest_sha256=?`, hash).Scan(&existing.ManifestID, &existing.Revision, &existing.SHA256, &existing.Document, &existing.IssuerFingerprint, &existing.State, new(int64))
	if err == nil {
		// Need a second scan for time because Scan cannot target a temporary in a stable struct.
		var created int64
		_ = tx.QueryRowContext(ctx, `SELECT created_at_ns FROM registry_v2_manifests WHERE manifest_sha256=?`, hash).Scan(&created)
		existing.CreatedAt = time.Unix(0, created).UTC()
		if err := tx.Commit(); err != nil {
			return V2Manifest{}, err
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return V2Manifest{}, fmt.Errorf("lookup v2 manifest: %w", err)
	}
	var current uint64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0) FROM registry_v2_manifests WHERE manifest_id=?`, parsed.ManifestID).Scan(&current); err != nil {
		return V2Manifest{}, err
	}
	revision := current + 1
	if current > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE registry_v2_manifests SET state='archived' WHERE manifest_id=? AND state='accepted'`, parsed.ManifestID); err != nil {
			return V2Manifest{}, err
		}
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO registry_v2_manifests(manifest_id,revision,manifest_sha256,document_json,issuer_fingerprint,state,created_at_ns) VALUES(?,?,?,?,?,'accepted',?)`, parsed.ManifestID, revision, hash, canonical, strings.TrimSpace(issuerFingerprint), now.UnixNano()); err != nil {
		return V2Manifest{}, fmt.Errorf("insert v2 manifest: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return V2Manifest{}, err
	}
	return V2Manifest{ManifestID: parsed.ManifestID, Revision: revision, SHA256: hash, Document: canonical, IssuerFingerprint: strings.TrimSpace(issuerFingerprint), State: "accepted", CreatedAt: now}, nil
}

// RegisterV2Device creates a pending binding to an accepted manifest. Calling
// it again for an identical UID is rejected while that binding is live:
// operator intent is explicit. A binding the coordinator already gave up on
// (desired_state='removed', left behind by a failed registration - see
// V2RegistrationCoordinator.Register's cleanup paths, none of which delete
// the row) does not block a fresh attempt; it is cleared here instead,
// since nothing else in this package ever removes that row.
func (s *Store) RegisterV2Device(ctx context.Context, device V2Device) (V2Device, error) {
	if s == nil || s.db == nil {
		return V2Device{}, errors.New("registry store is closed")
	}
	if err := validateV2Device(device); err != nil {
		return V2Device{}, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return V2Device{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var found int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM registry_v2_devices WHERE (device_id=? OR device_uid=?) AND desired_state!='removed'`, device.DeviceID, device.DeviceUID).Scan(&found); err == nil {
		return V2Device{}, fmt.Errorf("%w: %s", ErrV2DeviceAlreadyExists, device.DeviceID)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return V2Device{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM registry_v2_devices WHERE (device_id=? OR device_uid=?) AND desired_state='removed'`, device.DeviceID, device.DeviceUID); err != nil {
		return V2Device{}, fmt.Errorf("clear removed device binding: %w", err)
	}
	var hash string
	if err := tx.QueryRowContext(ctx, `SELECT manifest_sha256 FROM registry_v2_manifests WHERE manifest_id=? AND revision=? AND state='accepted'`, device.ManifestID, device.ManifestRevision).Scan(&hash); errors.Is(err, sql.ErrNoRows) {
		return V2Device{}, errors.New("accepted v2 manifest revision not found")
	} else if err != nil {
		return V2Device{}, err
	}
	if hash != device.ManifestSHA256 {
		return V2Device{}, errors.New("manifest hash does not match accepted revision")
	}
	now := s.now()
	device.DesiredState = "pending"
	device.ActiveState = "unprovisioned"
	device.CreatedAt = now
	device.UpdatedAt = now
	_, err = tx.ExecContext(ctx, `INSERT INTO registry_v2_devices(device_id,device_uid,manifest_id,manifest_revision,manifest_sha256,firmware_version,identity_public_key,desired_state,active_state,created_at_ns,updated_at_ns) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, device.DeviceID, device.DeviceUID, device.ManifestID, device.ManifestRevision, device.ManifestSHA256, device.FirmwareVersion, device.IdentityPublicKey, device.DesiredState, device.ActiveState, now.UnixNano(), now.UnixNano())
	if err != nil {
		return V2Device{}, fmt.Errorf("insert v2 device: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return V2Device{}, err
	}
	return device, nil
}

func (s *Store) SetV2DeviceState(ctx context.Context, deviceID, desired, active string) (V2Device, error) {
	if desired != "pending" && desired != "active" && desired != "disabled" && desired != "removed" {
		return V2Device{}, errors.New("invalid desired state")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE registry_v2_devices SET desired_state=?,active_state=?,updated_at_ns=? WHERE device_id=?`, desired, strings.TrimSpace(active), s.now().UnixNano(), deviceID)
	if err != nil {
		return V2Device{}, err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return V2Device{}, fmt.Errorf("%w: %s", ErrV2DeviceNotFound, deviceID)
	}
	return s.GetV2Device(ctx, deviceID)
}
func (s *Store) GetV2Device(ctx context.Context, id string) (V2Device, error) {
	return scanV2Device(s.db.QueryRowContext(ctx, `SELECT device_id,device_uid,manifest_id,manifest_revision,manifest_sha256,firmware_version,identity_public_key,desired_state,active_state,created_at_ns,updated_at_ns FROM registry_v2_devices WHERE device_id=?`, id))
}
func (s *Store) ListV2Devices(ctx context.Context) ([]V2Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT device_id,device_uid,manifest_id,manifest_revision,manifest_sha256,firmware_version,identity_public_key,desired_state,active_state,created_at_ns,updated_at_ns FROM registry_v2_devices ORDER BY device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []V2Device{}
	for rows.Next() {
		item, err := scanV2Device(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
func (s *Store) ResolveV2Manifest(ctx context.Context, id string) (devicev2.Manifest, bool, error) {
	var document string
	err := s.db.QueryRowContext(ctx, `SELECT m.document_json FROM registry_v2_devices d JOIN registry_v2_manifests m ON m.manifest_id=d.manifest_id AND m.revision=d.manifest_revision WHERE d.device_id=?`, id).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return devicev2.Manifest{}, false, nil
	}
	if err != nil {
		return devicev2.Manifest{}, false, err
	}
	value, _, _, err := devicev2.Parse(document)
	return value, err == nil, err
}

type scanner interface{ Scan(...any) error }

func scanV2Device(row scanner) (V2Device, error) {
	var d V2Device
	var created, updated int64
	err := row.Scan(&d.DeviceID, &d.DeviceUID, &d.ManifestID, &d.ManifestRevision, &d.ManifestSHA256, &d.FirmwareVersion, &d.IdentityPublicKey, &d.DesiredState, &d.ActiveState, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return V2Device{}, fmt.Errorf("%w", ErrV2DeviceNotFound)
	}
	if err != nil {
		return V2Device{}, err
	}
	d.CreatedAt = time.Unix(0, created).UTC()
	d.UpdatedAt = time.Unix(0, updated).UTC()
	return d, nil
}
func validateV2Device(d V2Device) error {
	if !v2ID(d.DeviceID) || strings.TrimSpace(d.DeviceUID) == "" || strings.TrimSpace(d.ManifestID) == "" || d.ManifestRevision == 0 || len(d.ManifestSHA256) != 64 || strings.TrimSpace(d.FirmwareVersion) == "" || strings.TrimSpace(d.IdentityPublicKey) == "" {
		return errors.New("device_id, device_uid, manifest binding, firmware_version and identity_public_key are required")
	}
	return nil
}
func v2ID(value string) bool {
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return value != ""
}
