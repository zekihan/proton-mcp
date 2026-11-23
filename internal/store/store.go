// Package store owns the SQLite schema and audit records.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jmoiron/sqlx"

	_ "modernc.org/sqlite" // Register the SQLite driver.
)

type Event struct {
	Action     string   `json:"action"`
	ID         string   `json:"id,omitempty"`
	Folder     string   `json:"folder,omitempty"`
	Status     string   `json:"status"`
	UIDs       []uint32 `json:"uids,omitempty"`
	Recipients []string `json:"recipients,omitempty"`
}

func Open(ctx context.Context, path string) (*sqlx.DB, error) {
	db, err := sqlx.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}

	db.SetMaxOpenConns(1)

	_, err = db.ExecContext(ctx, schema)
	if err != nil {
		return nil, fmt.Errorf("initialize SQLite: %w", closeAfterError(db, err))
	}

	return db, nil
}

func closeAfterError(db *sqlx.DB, cause error) error {
	err := db.Close()
	if err != nil {
		slog.Error("Could not close SQLite")
	}

	return cause
}

func Record(ctx context.Context, db sqlx.ExecerContext, event Event) error {
	detail, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode audit record: %w", err)
	}

	_, err = db.ExecContext(ctx, "INSERT INTO audit(at, event) VALUES (?, ?)",
		time.Now().UnixNano(), string(detail))
	if err != nil {
		return fmt.Errorf("store audit record: %w", err)
	}

	return nil
}

var auditLogger = slog.New(slog.NewJSONHandler(os.Stdout, nil))

func Log(ctx context.Context, event Event) {
	auditLogger.InfoContext(ctx, "Audit",
		slog.String("action", event.Action),
		slog.String("id", event.ID),
		slog.String("folder", event.Folder),
		slog.Any("uids", event.UIDs),
		slog.Any("recipients", event.Recipients),
		slog.String("status", event.Status),
	)
}

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA synchronous = FULL;
PRAGMA busy_timeout = 5000;

CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY, issuer TEXT NOT NULL, subject TEXT NOT NULL,
	name TEXT, email TEXT, created_at TEXT NOT NULL, renewed_at TEXT NOT NULL, expires_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);
CREATE TABLE IF NOT EXISTS oidc_transactions (
	id_hash TEXT PRIMARY KEY, state TEXT NOT NULL, nonce TEXT NOT NULL,
	pkce_verifier TEXT NOT NULL, expires_at TEXT NOT NULL, return_to TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS drafts (
	folder TEXT NOT NULL, uid_validity INTEGER NOT NULL, uid INTEGER NOT NULL,
	PRIMARY KEY(folder, uid_validity, uid)
);
CREATE TABLE IF NOT EXISTS actions (
	id TEXT PRIMARY KEY, kind TEXT NOT NULL, status TEXT NOT NULL,
	expires_at INTEGER NOT NULL, run_at INTEGER NOT NULL DEFAULT 0,
	claimed INTEGER NOT NULL DEFAULT 0, snapshot BLOB NOT NULL, digest TEXT NOT NULL,
	detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_actions_due ON actions(status, claimed, run_at, expires_at);
CREATE TABLE IF NOT EXISTS telegram_outbox (
 id INTEGER PRIMARY KEY, text TEXT NOT NULL, try_after INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS audit (
	id INTEGER PRIMARY KEY, at INTEGER NOT NULL, event TEXT NOT NULL
);
`
