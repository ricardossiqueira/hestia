// Package outbox stores validated device messages that are pending delivery to
// an external uplink. It deliberately has no MQTT dependency.
package outbox

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Kind determines an outbox message's retention priority. When bounded
// storage is full, telemetry is discarded before state and state before event.
type Kind string

const (
	Telemetry Kind = "telemetry"
	State     Kind = "state"
	Event     Kind = "event"
)

// Message is an already validated payload to be sent to the future uplink.
// EnqueuedAt is assigned by Store, rather than trusting a device timestamp.
type Message struct {
	MessageID string
	DeviceID  string
	Topic     string
	Kind      Kind
	Payload   []byte
}

// Options bounds durable work retained by a Store.
type Options struct {
	MaxMessages int
	MaxBytes    int64
	MaxAge      time.Duration
	Now         func() time.Time
}

// Stats contains the current logical outbox usage. PayloadBytes counts the
// stored MQTT payload bytes, not SQLite's internal file overhead.
type Stats struct {
	Messages     int
	PayloadBytes int64
}

// Snapshot is a read-only view of the pending logical outbox. Expired rows
// are excluded from its values but are deliberately not deleted: diagnostics
// must never add writes to an otherwise idle SQLite database.
type Snapshot struct {
	Stats
	OldestEnqueuedAt *time.Time
}

// LeaseRequest bounds a batch temporarily reserved for one uplink attempt.
// MaxBytes applies to payload bytes, matching the storage accounting rules.
type LeaseRequest struct {
	MaxMessages int
	MaxBytes    int64
	Duration    time.Duration
}

// LeasedMessage contains a pending record and immutable delivery metadata.
// Sequence is the local insertion order, not a cross-gateway ordering key.
type LeasedMessage struct {
	Message
	Sequence   int64
	EnqueuedAt time.Time
	Attempts   int
}

// Lease is an exclusive, time-bounded reservation of outbox messages. A lease
// does not remove messages; only Acknowledge can do that.
type Lease struct {
	Token     string
	ExpiresAt time.Time
	Messages  []LeasedMessage
}

// AcknowledgeResult reports how many records were durably removed. A count of
// zero is valid when a stale or unknown lease is acknowledged.
type AcknowledgeResult struct {
	Deleted int
}

// EnqueueResult describes whether the message became pending work. A duplicate
// is considered stored because it was already durably queued.
type EnqueueResult struct {
	Stored        bool
	Duplicate     bool
	Evicted       int
	Expired       int
	DiscardReason string
}

// Store is a SQLite-backed durable outbox.
type Store struct {
	db      *sql.DB
	options Options
}

// Open opens the SQLite file, applies every embedded migration exactly once,
// and validates the retention bounds before accepting messages.
func Open(ctx context.Context, sqlitePath string, options Options) (*Store, error) {
	if err := validateOptions(options); err != nil {
		return nil, err
	}
	if strings.TrimSpace(sqlitePath) == "" {
		return nil, errors.New("SQLite path is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", sqlitePath)
	if err != nil {
		return nil, fmt.Errorf("open outbox database: %w", err)
	}
	// SQLite is reliable with multiple connections, but a single writer avoids
	// needless lock contention on this small edge device.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open outbox database: %w", err)
	}
	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("configure outbox database: %w", err)
		}
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, options: normalizeOptions(options)}, nil
}

// Close releases the SQLite database handle. It is safe to call on nil.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Enqueue persists a message before any future uplink attempt. It expires old
// rows by their local enqueued time, deduplicates message IDs, and enforces
// count/byte limits without letting lower-priority messages evict higher ones.
func (s *Store) Enqueue(ctx context.Context, message Message) (EnqueueResult, error) {
	if s == nil || s.db == nil {
		return EnqueueResult{}, errors.New("outbox store is closed")
	}
	if err := ctx.Err(); err != nil {
		return EnqueueResult{}, err
	}
	if err := validateMessage(message); err != nil {
		return EnqueueResult{}, err
	}
	if int64(len(message.Payload)) > s.options.MaxBytes {
		return EnqueueResult{DiscardReason: "message exceeds max_outbox_bytes"}, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EnqueueResult{}, fmt.Errorf("begin outbox enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result := EnqueueResult{}
	cutoff := s.now().Add(-s.options.MaxAge).UnixNano()
	deleted, err := tx.ExecContext(ctx, `DELETE FROM outbox_messages WHERE enqueued_at_ns < ?`, cutoff)
	if err != nil {
		return EnqueueResult{}, fmt.Errorf("expire outbox messages: %w", err)
	}
	expired, err := deleted.RowsAffected()
	if err != nil {
		return EnqueueResult{}, fmt.Errorf("count expired outbox messages: %w", err)
	}
	result.Expired = int(expired)

	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM outbox_messages WHERE message_id = ?`, message.MessageID).Scan(&exists)
	if err == nil {
		result.Stored = true
		result.Duplicate = true
		if err := tx.Commit(); err != nil {
			return EnqueueResult{}, fmt.Errorf("commit outbox duplicate: %w", err)
		}
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return EnqueueResult{}, fmt.Errorf("check outbox duplicate: %w", err)
	}

	stats, err := statsTx(ctx, tx)
	if err != nil {
		return EnqueueResult{}, err
	}
	victims, err := evictionVictims(ctx, tx, message.Kind, stats, int64(len(message.Payload)), s.options)
	if err != nil {
		return EnqueueResult{}, err
	}
	if victims == nil {
		result.DiscardReason = "outbox limits preserve higher-priority messages"
		if err := tx.Commit(); err != nil {
			return EnqueueResult{}, fmt.Errorf("commit outbox discard: %w", err)
		}
		return result, nil
	}
	for _, victim := range victims {
		if _, err := tx.ExecContext(ctx, `DELETE FROM outbox_messages WHERE id = ?`, victim); err != nil {
			return EnqueueResult{}, fmt.Errorf("evict outbox message: %w", err)
		}
	}
	result.Evicted = len(victims)
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_messages
		(message_id, device_id, topic, kind, payload, payload_bytes, enqueued_at_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		message.MessageID, message.DeviceID, message.Topic, message.Kind, message.Payload, len(message.Payload), s.now().UnixNano()); err != nil {
		return EnqueueResult{}, fmt.Errorf("insert outbox message: %w", err)
	}
	result.Stored = true
	if err := tx.Commit(); err != nil {
		return EnqueueResult{}, fmt.Errorf("commit outbox enqueue: %w", err)
	}
	return result, nil
}

// Stats returns the logical capacity consumed by pending messages. It also
// removes expired rows so idle outboxes eventually release their storage.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	if s == nil || s.db == nil {
		return Stats{}, errors.New("outbox store is closed")
	}
	if err := ctx.Err(); err != nil {
		return Stats{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Stats{}, fmt.Errorf("begin outbox stats: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM outbox_messages WHERE enqueued_at_ns < ?`, s.now().Add(-s.options.MaxAge).UnixNano()); err != nil {
		return Stats{}, fmt.Errorf("expire outbox messages: %w", err)
	}
	stats, err := statsTx(ctx, tx)
	if err != nil {
		return Stats{}, err
	}
	if err := tx.Commit(); err != nil {
		return Stats{}, fmt.Errorf("commit outbox stats: %w", err)
	}
	return stats, nil
}

// Snapshot reads the logical capacity consumed by pending, non-expired
// messages. Unlike Stats, it performs no DELETE, transaction, or migration.
func (s *Store) Snapshot(ctx context.Context) (Snapshot, error) {
	if s == nil || s.db == nil {
		return Snapshot{}, errors.New("outbox store is closed")
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	var snapshot Snapshot
	var oldest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(payload_bytes), 0), MIN(enqueued_at_ns)
		FROM outbox_messages WHERE enqueued_at_ns >= ?`, s.now().Add(-s.options.MaxAge).UnixNano()).Scan(
		&snapshot.Messages, &snapshot.PayloadBytes, &oldest,
	); err != nil {
		return Snapshot{}, fmt.Errorf("read outbox snapshot: %w", err)
	}
	if oldest.Valid {
		value := time.Unix(0, oldest.Int64).UTC()
		snapshot.OldestEnqueuedAt = &value
	}
	return snapshot, nil
}

// Lease reserves the oldest eligible records for an external delivery attempt.
// Expired leases become eligible again. The operation is transactional, so two
// callers cannot lease the same record before the first lease expires.
func (s *Store) Lease(ctx context.Context, request LeaseRequest) (Lease, error) {
	if s == nil || s.db == nil {
		return Lease{}, errors.New("outbox store is closed")
	}
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	if err := validateLeaseRequest(request); err != nil {
		return Lease{}, err
	}

	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Lease{}, fmt.Errorf("begin outbox lease: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM outbox_messages WHERE enqueued_at_ns < ?`, now.Add(-s.options.MaxAge).UnixNano()); err != nil {
		return Lease{}, fmt.Errorf("expire outbox messages: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `SELECT id, message_id, device_id, topic, kind, payload, payload_bytes, enqueued_at_ns, attempt_count
		FROM outbox_messages
		WHERE lease_until_ns <= ? AND next_attempt_ns <= ?
		ORDER BY id`, now.UnixNano(), now.UnixNano())
	if err != nil {
		return Lease{}, fmt.Errorf("list leasable outbox messages: %w", err)
	}
	defer rows.Close()

	lease := Lease{Messages: make([]LeasedMessage, 0, request.MaxMessages)}
	var payloadBytes int64
	for rows.Next() {
		var message LeasedMessage
		var enqueuedAtNS int64
		var storedBytes int64
		if err := rows.Scan(&message.Sequence, &message.MessageID, &message.DeviceID, &message.Topic, &message.Kind, &message.Payload, &storedBytes, &enqueuedAtNS, &message.Attempts); err != nil {
			return Lease{}, fmt.Errorf("read leasable outbox message: %w", err)
		}
		if len(lease.Messages) == 0 && storedBytes > request.MaxBytes {
			return Lease{}, fmt.Errorf("next outbox message exceeds lease max bytes")
		}
		if payloadBytes+storedBytes > request.MaxBytes || len(lease.Messages) == request.MaxMessages {
			break
		}
		message.EnqueuedAt = time.Unix(0, enqueuedAtNS).UTC()
		lease.Messages = append(lease.Messages, message)
		payloadBytes += storedBytes
	}
	if err := rows.Err(); err != nil {
		return Lease{}, fmt.Errorf("list leasable outbox messages: %w", err)
	}
	if len(lease.Messages) == 0 {
		if err := tx.Commit(); err != nil {
			return Lease{}, fmt.Errorf("commit empty outbox lease: %w", err)
		}
		return lease, nil
	}

	lease.Token, err = newLeaseToken()
	if err != nil {
		return Lease{}, err
	}
	lease.ExpiresAt = now.Add(request.Duration)
	for index := range lease.Messages {
		message := &lease.Messages[index]
		result, err := tx.ExecContext(ctx, `UPDATE outbox_messages
			SET lease_token = ?, lease_until_ns = ?, attempt_count = attempt_count + 1
			WHERE id = ? AND lease_until_ns <= ? AND next_attempt_ns <= ?`,
			lease.Token, lease.ExpiresAt.UnixNano(), message.Sequence, now.UnixNano(), now.UnixNano())
		if err != nil {
			return Lease{}, fmt.Errorf("reserve outbox message: %w", err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return Lease{}, fmt.Errorf("count reserved outbox message: %w", err)
		}
		if updated != 1 {
			return Lease{}, errors.New("outbox lease lost before reservation")
		}
		message.Attempts++
	}
	if err := tx.Commit(); err != nil {
		return Lease{}, fmt.Errorf("commit outbox lease: %w", err)
	}
	return lease, nil
}

// Acknowledge permanently removes only records reserved by token. Callers may
// acknowledge a subset, which is necessary when a future protocol returns an
// explicit list of accepted message IDs.
func (s *Store) Acknowledge(ctx context.Context, token string, messageIDs []string) (AcknowledgeResult, error) {
	if s == nil || s.db == nil {
		return AcknowledgeResult{}, errors.New("outbox store is closed")
	}
	if err := ctx.Err(); err != nil {
		return AcknowledgeResult{}, err
	}
	if strings.TrimSpace(token) == "" {
		return AcknowledgeResult{}, errors.New("outbox lease token is required")
	}
	if len(messageIDs) == 0 {
		return AcknowledgeResult{}, errors.New("at least one outbox message ID is required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AcknowledgeResult{}, fmt.Errorf("begin outbox acknowledgement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result := AcknowledgeResult{}
	seen := make(map[string]struct{}, len(messageIDs))
	for _, messageID := range messageIDs {
		if strings.TrimSpace(messageID) == "" {
			return AcknowledgeResult{}, errors.New("outbox message ID is required")
		}
		if _, exists := seen[messageID]; exists {
			return AcknowledgeResult{}, fmt.Errorf("duplicate outbox message ID %q in acknowledgement", messageID)
		}
		seen[messageID] = struct{}{}
		deleted, err := tx.ExecContext(ctx, `DELETE FROM outbox_messages WHERE message_id = ? AND lease_token = ?`, messageID, token)
		if err != nil {
			return AcknowledgeResult{}, fmt.Errorf("acknowledge outbox message: %w", err)
		}
		count, err := deleted.RowsAffected()
		if err != nil {
			return AcknowledgeResult{}, fmt.Errorf("count acknowledged outbox message: %w", err)
		}
		result.Deleted += int(count)
	}
	if err := tx.Commit(); err != nil {
		return AcknowledgeResult{}, fmt.Errorf("commit outbox acknowledgement: %w", err)
	}
	return result, nil
}

// Release ends a lease without deleting its records and schedules their next
// attempt. errorCode is metadata for diagnostics and must not contain a
// payload, credential, or server response body.
func (s *Store) Release(ctx context.Context, token string, nextAttempt time.Time, errorCode string) (int, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("outbox store is closed")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if strings.TrimSpace(token) == "" {
		return 0, errors.New("outbox lease token is required")
	}
	if nextAttempt.IsZero() {
		return 0, errors.New("next outbox attempt time is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE outbox_messages
		SET lease_token = '', lease_until_ns = 0, next_attempt_ns = ?, last_error_code = ?
		WHERE lease_token = ?`, nextAttempt.UTC().UnixNano(), errorCode, token)
	if err != nil {
		return 0, fmt.Errorf("release outbox lease: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count released outbox lease: %w", err)
	}
	return int(count), nil
}

type storedMessage struct {
	id           int64
	kind         Kind
	payloadBytes int64
}

func evictionVictims(ctx context.Context, tx *sql.Tx, incoming Kind, stats Stats, incomingBytes int64, options Options) ([]int64, error) {
	if stats.Messages+1 <= options.MaxMessages && stats.PayloadBytes+incomingBytes <= options.MaxBytes {
		return []int64{}, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, payload_bytes FROM outbox_messages
		ORDER BY CASE kind WHEN 'telemetry' THEN 0 WHEN 'state' THEN 1 WHEN 'event' THEN 2 END, enqueued_at_ns, id`)
	if err != nil {
		return nil, fmt.Errorf("list outbox eviction candidates: %w", err)
	}
	defer rows.Close()
	var candidates []storedMessage
	for rows.Next() {
		var candidate storedMessage
		if err := rows.Scan(&candidate.id, &candidate.kind, &candidate.payloadBytes); err != nil {
			return nil, fmt.Errorf("read outbox eviction candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list outbox eviction candidates: %w", err)
	}

	count, bytes := stats.Messages, stats.PayloadBytes
	victims := make([]int64, 0)
	for _, candidate := range candidates {
		// A new message may replace an equally important old message or any
		// lower priority one, but must never push a higher priority record out.
		if priority(candidate.kind) > priority(incoming) {
			continue
		}
		victims = append(victims, candidate.id)
		count--
		bytes -= candidate.payloadBytes
		if count+1 <= options.MaxMessages && bytes+incomingBytes <= options.MaxBytes {
			return victims, nil
		}
	}
	return nil, nil
}

func statsTx(ctx context.Context, tx *sql.Tx) (Stats, error) {
	var stats Stats
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(payload_bytes), 0) FROM outbox_messages`).Scan(&stats.Messages, &stats.PayloadBytes); err != nil {
		return Stats{}, fmt.Errorf("read outbox stats: %w", err)
	}
	return stats, nil
}

func validateOptions(options Options) error {
	if options.MaxMessages <= 0 {
		return errors.New("max outbox messages must be greater than zero")
	}
	if options.MaxBytes <= 0 {
		return errors.New("max outbox bytes must be greater than zero")
	}
	if options.MaxAge <= 0 {
		return errors.New("max outbox age must be greater than zero")
	}
	return nil
}

func validateLeaseRequest(request LeaseRequest) error {
	if request.MaxMessages <= 0 {
		return errors.New("lease max messages must be greater than zero")
	}
	if request.MaxBytes <= 0 {
		return errors.New("lease max bytes must be greater than zero")
	}
	if request.Duration <= 0 {
		return errors.New("lease duration must be greater than zero")
	}
	return nil
}

func normalizeOptions(options Options) Options {
	if options.Now == nil {
		options.Now = time.Now
	}
	return options
}

func validateMessage(message Message) error {
	if strings.TrimSpace(message.MessageID) == "" {
		return errors.New("outbox message ID is required")
	}
	if strings.TrimSpace(message.DeviceID) == "" {
		return errors.New("outbox device ID is required")
	}
	if strings.TrimSpace(message.Topic) == "" {
		return errors.New("outbox topic is required")
	}
	if priority(message.Kind) < 0 {
		return fmt.Errorf("unsupported outbox message kind %q", message.Kind)
	}
	return nil
}

func priority(kind Kind) int {
	switch kind {
	case Telemetry:
		return 0
	case State:
		return 1
	case Event:
		return 2
	default:
		return -1
	}
}

func (s *Store) now() time.Time { return s.options.Now().UTC() }

func newLeaseToken() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("create outbox lease token: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY)`); err != nil {
		return fmt.Errorf("create outbox migration table: %w", err)
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("read embedded outbox migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		name := entry.Name()
		var applied string
		err := db.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE name = ?`, name).Scan(&applied)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check outbox migration %q: %w", name, err)
		}
		sqlBytes, err := migrationFiles.ReadFile(path.Join("migrations", name))
		if err != nil {
			return fmt.Errorf("read outbox migration %q: %w", name, err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin outbox migration %q: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply outbox migration %q: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(name) VALUES (?)`, name); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record outbox migration %q: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit outbox migration %q: %w", name, err)
		}
	}
	return nil
}
