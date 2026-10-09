// Package operatorauth owns local operator accounts and browser sessions.
// It uses the gateway's SQLite file, but its tables and HTTP boundary are
// separate from device policy so an external identity provider can replace it.
package operatorauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

var (
	ErrAlreadyRegistered  = errors.New("an operator is already registered")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrInvalidAccount     = errors.New("invalid operator account")
)

const sessionLifetime = 30 * 24 * time.Hour

type Store struct {
	db        *sql.DB
	now       func() time.Time
	dummyHash []byte
}

type Session struct {
	Username  string
	CSRFToken string
	ExpiresAt time.Time
}

func Open(ctx context.Context, sqlitePath string) (*Store, error) {
	if strings.TrimSpace(sqlitePath) == "" {
		return nil, errors.New("operator auth SQLite path is required")
	}
	db, err := sql.Open("sqlite", sqlitePath)
	if err != nil {
		return nil, fmt.Errorf("open operator auth database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, statement := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		`CREATE TABLE IF NOT EXISTS operator_auth_users (
			username TEXT PRIMARY KEY,
			password_hash TEXT NOT NULL,
			created_at_ns INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS operator_auth_sessions (
			token_hash TEXT PRIMARY KEY,
			username TEXT NOT NULL REFERENCES operator_auth_users(username) ON DELETE CASCADE,
			csrf_token TEXT NOT NULL,
			expires_at_ns INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS operator_auth_sessions_expiry ON operator_auth_sessions(expires_at_ns)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("initialize operator auth database: %w", err)
		}
	}
	dummyHash, err := bcrypt.GenerateFromPassword([]byte("unused-password-for-equal-work"), 12)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, now: func() time.Time { return time.Now().UTC() }, dummyHash: dummyHash}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) NeedsRegistration(ctx context.Context) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM operator_auth_users`).Scan(&count); err != nil {
		return false, err
	}
	return count == 0, nil
}

// RegisterFirst accepts exactly one account. The conditional INSERT closes the
// race between two browsers completing first-run registration together.
func (s *Store) RegisterFirst(ctx context.Context, username, password string) error {
	if err := validateCredentials(username, password); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return fmt.Errorf("hash operator password: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO operator_auth_users(username,password_hash,created_at_ns)
		SELECT ?,?,? WHERE NOT EXISTS(SELECT 1 FROM operator_auth_users)`, username, string(hash), s.now().UnixNano())
	if err != nil {
		return fmt.Errorf("register operator: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrAlreadyRegistered
	}
	return nil
}

func validateCredentials(username, password string) error {
	if len(username) < 3 || len(username) > 64 {
		return fmt.Errorf("%w: username must contain 3 to 64 characters", ErrInvalidAccount)
	}
	for _, r := range username {
		if r < 'a' || r > 'z' {
			if r < 'A' || r > 'Z' {
				if r < '0' || r > '9' {
					if r != '_' && r != '-' && r != '.' {
						return fmt.Errorf("%w: username may contain only letters, numbers, underscore, dash or dot", ErrInvalidAccount)
					}
				}
			}
		}
	}
	if len(password) < 12 || len(password) > 72 {
		return fmt.Errorf("%w: password must contain 12 to 72 bytes", ErrInvalidAccount)
	}
	return nil
}

func (s *Store) VerifyPassword(ctx context.Context, username, password string) error {
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM operator_auth_users WHERE username=?`, username).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(s.dummyHash, []byte(password))
		return ErrInvalidCredentials
	}
	if err != nil {
		return fmt.Errorf("load operator account: %w", err)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return ErrInvalidCredentials
	}
	return nil
}

func randomToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Store) NewSession(ctx context.Context, username string) (string, Session, error) {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM operator_auth_sessions WHERE expires_at_ns<=?`, s.now().UnixNano()); err != nil {
		return "", Session{}, fmt.Errorf("expire operator sessions: %w", err)
	}
	token, err := randomToken()
	if err != nil {
		return "", Session{}, err
	}
	csrf, err := randomToken()
	if err != nil {
		return "", Session{}, err
	}
	session := Session{Username: username, CSRFToken: csrf, ExpiresAt: s.now().Add(sessionLifetime)}
	_, err = s.db.ExecContext(ctx, `INSERT INTO operator_auth_sessions(token_hash,username,csrf_token,expires_at_ns) VALUES(?,?,?,?)`, tokenHash(token), username, csrf, session.ExpiresAt.UnixNano())
	if err != nil {
		return "", Session{}, fmt.Errorf("create operator session: %w", err)
	}
	return token, session, nil
}

func (s *Store) Session(ctx context.Context, token string) (Session, bool, error) {
	if len(token) != 64 {
		return Session{}, false, nil
	}
	var session Session
	var expires int64
	err := s.db.QueryRowContext(ctx, `SELECT username,csrf_token,expires_at_ns FROM operator_auth_sessions WHERE token_hash=? AND expires_at_ns>?`, tokenHash(token), s.now().UnixNano()).Scan(&session.Username, &session.CSRFToken, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("load operator session: %w", err)
	}
	session.ExpiresAt = time.Unix(0, expires).UTC()
	return session, true, nil
}

func (s *Store) RevokeSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM operator_auth_sessions WHERE token_hash=?`, tokenHash(token))
	return err
}
