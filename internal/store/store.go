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
	"github.com/zekihan/go-commons/sqlitestore"

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

// migrations are applied in order by sqlitestore; PRAGMA user_version then
// counts how many have run. The base schema deliberately creates actions
// without the execution flags so the alter migration below can add them to
// databases created before those columns existed.
var migrations = []string{schema, alterActions, actionDataFixups}

func Open(ctx context.Context, path string) (*sqlx.DB, error) {
	err := normalizeLegacyVersion(ctx, path)
	if err != nil {
		return nil, err
	}

	st, err := sqlitestore.Open(ctx, path, migrations)
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}

	return st.DB(), nil
}

// normalizeLegacyVersion maps a database written by the pre-sqlitestore
// schema onto the migration numbering above. The old code recorded
// user_version 1 only after adding the action execution flags and applying
// the data fixups, which is exactly the state migrations 2 and 3 produce, so
// such databases are stamped with the full migration count. A version-1
// database cannot occur in the new numbering: sqlitestore applies all
// pending migrations in one transaction.
func normalizeLegacyVersion(ctx context.Context, path string) error {
	if path == ":memory:" {
		return nil
	}

	_, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return fmt.Errorf("stat SQLite database: %w", err)
	}

	db, err := sqlx.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("open legacy SQLite database: %w", err)
	}

	var version int

	err = db.GetContext(ctx, &version, "PRAGMA user_version")
	if err == nil && version == 1 {
		_, err = db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", len(migrations)))
	}

	closeErr := db.Close()
	if err != nil {
		return fmt.Errorf("normalize legacy SQLite database: %w", err)
	}

	if closeErr != nil {
		return fmt.Errorf("close legacy SQLite database: %w", closeErr)
	}

	return nil
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

// schema is the base schema. Connection pragmas (WAL, busy timeout, foreign
// keys) come from the sqlitestore DSN.
// The oidc_transactions table carries return_to for the local sign-in flow
// in internal/auth.
const schema = `
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

const alterActions = `
ALTER TABLE actions ADD COLUMN cancel_requested INTEGER NOT NULL DEFAULT 0;
ALTER TABLE actions ADD COLUMN execution_started INTEGER NOT NULL DEFAULT 0;
`

// Legacy in-flight claims have an unknown execution state, so they are
// marked as started; legacy stalls get a fresh deadline on recovery.
const actionDataFixups = `
UPDATE actions SET execution_started=1 WHERE claimed=1;
UPDATE actions SET expires_at=0 WHERE status='stalled';
`
