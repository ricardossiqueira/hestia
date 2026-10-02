// Package registry owns the gateway's durable SQLite-backed state. It used
// to also own V1's devices/routes/inconsistencies/manifests policy - retired
// along with the rest of V1 (see docs/decisions.md's V1-removal ADR). What
// remains here is pure infrastructure (opening the database, running
// migrations) plus whatever device_v2.go/automation_v2.go need from it.
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

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

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
	return &Store{db: db, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) begin(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin registry transaction: %w", err)
	}
	return tx, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
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
