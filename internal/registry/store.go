// Package registry owns mutable gateway policy: devices, topics, routes and
// the revision that lets long-lived consumers converge on it. It deliberately
// never stores MQTT passwords or other plaintext secrets.
package registry

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/ricardossiqueira/iot-gateway/internal/config"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

var (
	ErrDeviceAlreadyExists      = errors.New("registry device already exists")
	ErrDeviceNotFound           = errors.New("registry device not found")
	ErrRouteAlreadyExists       = errors.New("registry route already exists")
	ErrRouteNotFound            = errors.New("registry route not found")
	ErrInconsistencyNotFound    = errors.New("registry inconsistency not found")
	ErrManifestNotFound         = errors.New("registry device manifest not found")
	ErrManifestAlreadyExists    = errors.New("registry device manifest already exists")
	ErrManifestRevisionNotFound = errors.New("registry device manifest revision not found")
	ErrManifestRevisionNotDraft = errors.New("registry device manifest revision is not a draft")
)

// Snapshot is one coherent, revisioned routing policy.
type Snapshot struct {
	Revision uint64
	Devices  []config.Device
	Routes   []config.Route
}

// Inconsistency records a best-effort compensation in internal/admin that
// itself failed, leaving the registry and the Mosquitto broker disagreeing
// about a device - see RecordInconsistency's doc comment.
type Inconsistency struct {
	ID                string
	Kind              string
	DeviceID          string
	Cause             string
	CompensationError string
	CreatedAt         time.Time
}

// DeviceManifest is one published, immutable revision of a device-family
// definition. Document is canonical JSON and deliberately excludes secrets.
type DeviceManifest struct {
	ID          string
	DisplayName string
	Revision    uint64
	Document    string
	CreatedBy   string
	CreatedAt   time.Time
}

// DeviceManifestBinding records which exact definition was used for a device
// instance. It is written by the generic provisioner in the next milestone;
// keeping the relation now prevents a published edit from changing old
// instances implicitly.
type DeviceManifestBinding struct {
	DeviceID         string
	ManifestID       string
	ManifestRevision uint64
}

// Store serializes writes on the edge device and provides durable snapshots.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

func Open(ctx context.Context, sqlitePath string) (*Store, error) {
	if strings.TrimSpace(sqlitePath) == "" {
		return nil, errors.New("registry SQLite path is required")
	}
	db, err := sql.Open("sqlite", sqlitePath)
	if err != nil {
		return nil, fmt.Errorf("open registry database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open registry database: %w", err)
	}
	for _, pragma := range []string{"PRAGMA foreign_keys = ON", "PRAGMA journal_mode = WAL", "PRAGMA synchronous = FULL", "PRAGMA busy_timeout = 5000"} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("configure registry database: %w", err)
		}
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	store := &Store{db: db, now: func() time.Time { return time.Now().UTC() }}
	if err := store.seedDefaultManifests(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Seed imports legacy YAML exactly once, and only into an empty registry.
// Once populated, SQLite is the only mutable source of device policy.
func (s *Store) Seed(ctx context.Context, devices []config.Device, routes []config.Route) (bool, error) {
	if len(devices) == 0 && len(routes) == 0 {
		return false, nil
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM registry_devices`).Scan(&count); err != nil {
		return false, fmt.Errorf("count registry devices: %w", err)
	}
	if count != 0 {
		return false, tx.Commit()
	}
	revision, err := nextRevision(ctx, tx)
	if err != nil {
		return false, err
	}
	for _, device := range devices {
		if err := validateDevice(device); err != nil {
			return false, err
		}
		if err := insertDevice(ctx, tx, device, revision, s.now()); err != nil {
			return false, err
		}
	}
	for _, route := range routes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO registry_routes
			(id, source_topic, destination_topic, transform_type, command_type, qos, retain, revision)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, route.ID, route.SourceTopic, route.DestinationTopic,
			route.Transform.Type, route.Transform.CommandType, route.QoS, boolInt(route.Retain), revision); err != nil {
			return false, fmt.Errorf("seed route %q: %w", route.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit registry seed: %w", err)
	}
	return true, nil
}

// AddDevice durably records a new desired device policy. idempotencyKey must
// be unique per caller operation; a repeated key reports the original success.
func (s *Store) AddDevice(ctx context.Context, device config.Device, idempotencyKey string) (Snapshot, error) {
	if err := validateDevice(device); err != nil {
		return Snapshot{}, err
	}
	if strings.TrimSpace(idempotencyKey) == "" {
		return Snapshot{}, errors.New("idempotency key is required")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var existingDevice string
	if err := tx.QueryRowContext(ctx, `SELECT device_id FROM registry_operations WHERE idempotency_key = ?`, idempotencyKey).Scan(&existingDevice); err == nil {
		if existingDevice != device.ID {
			return Snapshot{}, errors.New("idempotency key belongs to a different device")
		}
		if err := tx.Commit(); err != nil {
			return Snapshot{}, err
		}
		return s.Snapshot(ctx)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, fmt.Errorf("read idempotency operation: %w", err)
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM registry_devices WHERE id = ?`, device.ID).Scan(&exists); err == nil {
		return Snapshot{}, fmt.Errorf("%w: %q", ErrDeviceAlreadyExists, device.ID)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, fmt.Errorf("check registry device: %w", err)
	}
	revision, err := nextRevision(ctx, tx)
	if err != nil {
		return Snapshot{}, err
	}
	if err := insertDevice(ctx, tx, device, revision, s.now()); err != nil {
		return Snapshot{}, err
	}
	if err := insertOperation(ctx, tx, idempotencyKey, "provision", device.ID, revision, s.now()); err != nil {
		return Snapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return Snapshot{}, fmt.Errorf("commit registry device: %w", err)
	}
	return s.Snapshot(ctx)
}

func (s *Store) SetDeviceEnabled(ctx context.Context, id string, enabled bool, idempotencyKey string) (config.Device, error) {
	if strings.TrimSpace(idempotencyKey) == "" {
		return config.Device{}, errors.New("idempotency key is required")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return config.Device{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM registry_devices WHERE id = ?`, id).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return config.Device{}, fmt.Errorf("%w: %q", ErrDeviceNotFound, id)
	} else if err != nil {
		return config.Device{}, fmt.Errorf("check registry device: %w", err)
	}
	revision, err := nextRevision(ctx, tx)
	if err != nil {
		return config.Device{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE registry_devices SET enabled = ?, revision = ?, updated_at_ns = ? WHERE id = ?`, boolInt(enabled), revision, s.now().UnixNano(), id); err != nil {
		return config.Device{}, fmt.Errorf("update registry device: %w", err)
	}
	if err := insertOperation(ctx, tx, idempotencyKey, "set_enabled", id, revision, s.now()); err != nil {
		return config.Device{}, err
	}
	if err := tx.Commit(); err != nil {
		return config.Device{}, fmt.Errorf("commit registry device: %w", err)
	}
	snapshot, err := s.Snapshot(ctx)
	if err != nil {
		return config.Device{}, err
	}
	for _, device := range snapshot.Devices {
		if device.ID == id {
			return device, nil
		}
	}
	return config.Device{}, fmt.Errorf("%w: %q", ErrDeviceNotFound, id)
}

func (s *Store) RemoveDevice(ctx context.Context, id, idempotencyKey string) error {
	if strings.TrimSpace(idempotencyKey) == "" {
		return errors.New("idempotency key is required")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	revision, err := nextRevision(ctx, tx)
	if err != nil {
		return err
	}
	// A route cannot remain after either endpoint goes away. Removing it in
	// the same revision prevents the long-lived gateway from ever observing
	// a dangling source or destination topic.
	if _, err := tx.ExecContext(ctx, `DELETE FROM registry_routes
		WHERE source_topic IN (
			SELECT topic FROM registry_device_topics
			WHERE device_id = ? AND kind IN ('telemetry', 'state', 'event', 'command_result')
		) OR destination_topic IN (
			SELECT topic FROM registry_device_topics
			WHERE device_id = ? AND kind = 'command'
		)`, id, id); err != nil {
		return fmt.Errorf("remove routes for registry device: %w", err)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM registry_devices WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("remove registry device: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %q", ErrDeviceNotFound, id)
	}
	if err := insertOperation(ctx, tx, idempotencyKey, "remove", id, revision, s.now()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit registry removal: %w", err)
	}
	return nil
}

// AddRoute persists one local MQTT route and advances the policy revision.
// Its endpoints must be enabled device topics at the instant it is created;
// the gateway's registry watcher applies the resulting snapshot without a
// process restart.
func (s *Store) AddRoute(ctx context.Context, route config.Route) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := validateRoute(ctx, tx, route); err != nil {
		return err
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM registry_routes WHERE id = ?`, route.ID).Scan(&exists); err == nil {
		return fmt.Errorf("%w: %q", ErrRouteAlreadyExists, route.ID)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check registry route: %w", err)
	}
	revision, err := nextRevision(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO registry_routes
		(id, source_topic, destination_topic, transform_type, command_type, qos, retain, revision)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, route.ID, route.SourceTopic, route.DestinationTopic,
		route.Transform.Type, route.Transform.CommandType, route.QoS, boolInt(route.Retain), revision); err != nil {
		return fmt.Errorf("insert registry route %q: %w", route.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit registry route: %w", err)
	}
	return nil
}

// RemoveRoute deletes a local route and advances the policy revision.
func (s *Store) RemoveRoute(ctx context.Context, id string) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = nextRevision(ctx, tx)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM registry_routes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("remove registry route: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("%w: %q", ErrRouteNotFound, id)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit registry route removal: %w", err)
	}
	return nil
}

// RecordInconsistency durably records that a best-effort compensation in
// internal/admin (undoing a partially-applied provisioning step) itself
// failed - the registry and the Mosquitto broker now disagree about
// deviceID. cause is what triggered the original rollback attempt;
// compensationError is why the rollback itself failed. This is deliberately
// not part of a transaction with whatever registry write preceded it: it
// runs after that write already committed (or after it was attempted and
// failed), as an independent durable note for an operator to act on -
// see ListInconsistencies/ResolveInconsistency.
func (s *Store) RecordInconsistency(ctx context.Context, kind, deviceID, cause, compensationError string) error {
	if s == nil || s.db == nil {
		return errors.New("registry store is closed")
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO registry_inconsistencies
		(id, kind, device_id, cause, compensation_error, created_at_ns)
		VALUES (lower(hex(randomblob(16))), ?, ?, ?, ?, ?)`,
		kind, deviceID, cause, compensationError, s.now().UnixNano()); err != nil {
		return fmt.Errorf("record registry inconsistency: %w", err)
	}
	return nil
}

// ListInconsistencies returns unresolved inconsistencies, most recent first.
func (s *Store) ListInconsistencies(ctx context.Context) ([]Inconsistency, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("registry store is closed")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, kind, device_id, cause, compensation_error, created_at_ns
		FROM registry_inconsistencies WHERE resolved_at_ns IS NULL ORDER BY created_at_ns DESC`)
	if err != nil {
		return nil, fmt.Errorf("list registry inconsistencies: %w", err)
	}
	defer rows.Close()
	inconsistencies := make([]Inconsistency, 0)
	for rows.Next() {
		var item Inconsistency
		var createdAtNS int64
		if err := rows.Scan(&item.ID, &item.Kind, &item.DeviceID, &item.Cause, &item.CompensationError, &createdAtNS); err != nil {
			return nil, fmt.Errorf("read registry inconsistency: %w", err)
		}
		item.CreatedAt = time.Unix(0, createdAtNS).UTC()
		inconsistencies = append(inconsistencies, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list registry inconsistencies: %w", err)
	}
	return inconsistencies, nil
}

// ResolveInconsistency marks an inconsistency resolved without deleting it,
// preserving the audit trail. It does not itself change anything in the
// registry or the broker - the operator has already fixed the underlying
// state by hand (or independently confirmed it needs no fix).
func (s *Store) ResolveInconsistency(ctx context.Context, id string) error {
	if s == nil || s.db == nil {
		return errors.New("registry store is closed")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE registry_inconsistencies SET resolved_at_ns = ?
		WHERE id = ? AND resolved_at_ns IS NULL`, s.now().UnixNano(), id)
	if err != nil {
		return fmt.Errorf("resolve registry inconsistency: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count resolved registry inconsistency: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("%w: %q", ErrInconsistencyNotFound, id)
	}
	return nil
}

func (s *Store) Snapshot(ctx context.Context) (Snapshot, error) {
	if s == nil || s.db == nil {
		return Snapshot{}, errors.New("registry store is closed")
	}
	var revision uint64
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM registry_meta WHERE key = 'revision'`).Scan(&revision); err != nil {
		return Snapshot{}, fmt.Errorf("read registry revision: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT d.id, d.type, d.enabled,
		COALESCE(t.telemetry, ''), COALESCE(t.state, ''), COALESCE(t.event, ''), COALESCE(t.command, ''), COALESCE(t.command_result, ''),
		f.telemetry_to_vps, f.state_to_vps, f.events_to_vps, f.commands_from_vps
		FROM registry_devices d JOIN registry_device_forwarding f ON f.device_id = d.id
		LEFT JOIN (SELECT device_id,
		MAX(CASE WHEN kind = 'telemetry' THEN topic END) telemetry,
		MAX(CASE WHEN kind = 'state' THEN topic END) state,
		MAX(CASE WHEN kind = 'event' THEN topic END) event,
		MAX(CASE WHEN kind = 'command' THEN topic END) command,
		MAX(CASE WHEN kind = 'command_result' THEN topic END) command_result
		FROM registry_device_topics GROUP BY device_id) t ON t.device_id = d.id ORDER BY d.id`)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list registry devices: %w", err)
	}
	defer rows.Close()
	snapshot := Snapshot{Revision: revision}
	for rows.Next() {
		var d config.Device
		var enabled, telemetry, state, event, command int
		if err := rows.Scan(&d.ID, &d.Type, &enabled, &d.Topics.Telemetry, &d.Topics.State, &d.Topics.Event, &d.Topics.Command, &d.Topics.CommandResult, &telemetry, &state, &event, &command); err != nil {
			return Snapshot{}, fmt.Errorf("scan registry device: %w", err)
		}
		value := enabled != 0
		d.Enabled = &value
		d.Forwarding = config.Forwarding{TelemetryToVPS: telemetry != 0, StateToVPS: state != 0, EventsToVPS: event != 0, CommandsFromVPS: command != 0}
		snapshot.Devices = append(snapshot.Devices, d)
	}
	if err := rows.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("iterate registry devices: %w", err)
	}
	routes, err := s.db.QueryContext(ctx, `SELECT id, source_topic, destination_topic, transform_type, command_type, qos, retain FROM registry_routes ORDER BY id`)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list registry routes: %w", err)
	}
	defer routes.Close()
	for routes.Next() {
		var route config.Route
		var retain int
		if err := routes.Scan(&route.ID, &route.SourceTopic, &route.DestinationTopic, &route.Transform.Type, &route.Transform.CommandType, &route.QoS, &retain); err != nil {
			return Snapshot{}, fmt.Errorf("scan registry route: %w", err)
		}
		route.Retain = retain != 0
		snapshot.Routes = append(snapshot.Routes, route)
	}
	if err := routes.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("iterate registry routes: %w", err)
	}
	return snapshot, nil
}

func (s *Store) begin(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin registry transaction: %w", err)
	}
	return tx, nil
}
func nextRevision(ctx context.Context, tx *sql.Tx) (uint64, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE registry_meta SET value = value + 1 WHERE key = 'revision'`); err != nil {
		return 0, fmt.Errorf("advance registry revision: %w", err)
	}
	var revision uint64
	if err := tx.QueryRowContext(ctx, `SELECT value FROM registry_meta WHERE key = 'revision'`).Scan(&revision); err != nil {
		return 0, fmt.Errorf("read next registry revision: %w", err)
	}
	return revision, nil
}
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func insertDevice(ctx context.Context, tx *sql.Tx, d config.Device, revision uint64, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO registry_devices(id, type, enabled, revision, created_at_ns, updated_at_ns) VALUES (?, ?, ?, ?, ?, ?)`, d.ID, d.Type, boolInt(*d.Enabled), revision, now.UnixNano(), now.UnixNano()); err != nil {
		return fmt.Errorf("insert registry device %q: %w", d.ID, err)
	}
	for _, item := range []struct{ kind, topic string }{{"telemetry", d.Topics.Telemetry}, {"state", d.Topics.State}, {"event", d.Topics.Event}, {"command", d.Topics.Command}, {"command_result", d.Topics.CommandResult}} {
		if item.topic == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO registry_device_topics(device_id, kind, topic) VALUES (?, ?, ?)`, d.ID, item.kind, item.topic); err != nil {
			return fmt.Errorf("insert registry topic %q: %w", item.topic, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO registry_device_forwarding(device_id, telemetry_to_vps, state_to_vps, events_to_vps, commands_from_vps) VALUES (?, ?, ?, ?, ?)`, d.ID, boolInt(d.Forwarding.TelemetryToVPS), boolInt(d.Forwarding.StateToVPS), boolInt(d.Forwarding.EventsToVPS), boolInt(d.Forwarding.CommandsFromVPS)); err != nil {
		return fmt.Errorf("insert registry forwarding %q: %w", d.ID, err)
	}
	return nil
}
func insertOperation(ctx context.Context, tx *sql.Tx, key, kind, id string, revision uint64, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO registry_operations(id, idempotency_key, kind, device_id, state, revision, created_at_ns, updated_at_ns) VALUES (lower(hex(randomblob(16))), ?, ?, ?, 'applied', ?, ?, ?)`, key, kind, id, revision, now.UnixNano(), now.UnixNano()); err != nil {
		return fmt.Errorf("record registry operation: %w", err)
	}
	return nil
}
func validateDevice(d config.Device) error {
	if err := config.ValidateDeviceID("device.id", d.ID); err != nil {
		return err
	}
	if strings.TrimSpace(d.Type) == "" {
		return errors.New("device.type is required")
	}
	if d.Enabled == nil {
		return errors.New("device.enabled is required")
	}
	topics := []struct{ suffix, topic string }{{"telemetry", d.Topics.Telemetry}, {"state", d.Topics.State}, {"event", d.Topics.Event}, {"command", d.Topics.Command}, {"command-result", d.Topics.CommandResult}}
	count := 0
	for _, item := range topics {
		if item.topic == "" {
			continue
		}
		count++
		if item.topic != "devices/"+d.ID+"/"+item.suffix {
			return fmt.Errorf("device topic %q must be devices/%s/%s", item.topic, d.ID, item.suffix)
		}
	}
	if count == 0 {
		return errors.New("device must define at least one topic")
	}
	return nil
}

func validateRoute(ctx context.Context, tx *sql.Tx, route config.Route) error {
	if err := config.ValidateDeviceID("route.id", route.ID); err != nil {
		return err
	}
	if route.QoS > 2 {
		return errors.New("route.qos must be between 0 and 2")
	}
	if route.Transform.Type != "json_command" {
		return errors.New("route.transform.type must be json_command")
	}
	if strings.TrimSpace(route.Transform.CommandType) == "" {
		return errors.New("route.transform.command_type is required")
	}
	var sourceCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM registry_device_topics t
		JOIN registry_devices d ON d.id = t.device_id
		WHERE d.enabled = 1 AND t.topic = ? AND t.kind IN ('telemetry', 'state', 'event', 'command_result')`, route.SourceTopic).Scan(&sourceCount); err != nil {
		return fmt.Errorf("validate route source: %w", err)
	}
	if sourceCount == 0 {
		return errors.New("route.source_topic must reference an enabled inbound device topic")
	}
	var destinationCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM registry_device_topics t
		JOIN registry_devices d ON d.id = t.device_id
		WHERE d.enabled = 1 AND t.topic = ? AND t.kind = 'command'`, route.DestinationTopic).Scan(&destinationCount); err != nil {
		return fmt.Errorf("validate route destination: %w", err)
	}
	if destinationCount == 0 {
		return errors.New("route.destination_topic must reference an enabled command topic")
	}
	return nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS registry_schema_migrations (name TEXT PRIMARY KEY)`); err != nil {
		return fmt.Errorf("create registry migration table: %w", err)
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		var applied string
		err := db.QueryRowContext(ctx, `SELECT name FROM registry_schema_migrations WHERE name = ?`, entry.Name()).Scan(&applied)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		sqlBytes, err := migrationFiles.ReadFile(path.Join("migrations", entry.Name()))
		if err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply registry migration %q: %w", entry.Name(), err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO registry_schema_migrations(name) VALUES (?)`, entry.Name()); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
