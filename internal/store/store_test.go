package store

import (
	"path/filepath"
	"testing"

	"github.com/jmoiron/sqlx"
)

func TestMigrationPreservesLegacyClaimsAndStalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	old, err := sqlx.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.ExecContext(t.Context(), schema+`
		INSERT INTO actions(id,kind,status,expires_at,claimed,snapshot,digest)
		VALUES ('claimed','send_draft','approved_queued',123,1,x'01','digest'),
		       ('stalled','send_draft','stalled',456,0,x'02','digest');
	`)
	if err != nil {
		t.Fatal(err)
	}
	err = old.Close()
	if err != nil {
		t.Fatal(err)
	}

	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var started bool
	err = db.GetContext(
		t.Context(),
		&started,
		"SELECT execution_started FROM actions WHERE id='claimed'",
	)
	if err != nil || !started {
		t.Fatalf(
			"legacy in-flight claim was not marked ambiguous: started=%v, err=%v",
			started,
			err,
		)
	}
	var expires int64
	err = db.GetContext(t.Context(), &expires, "SELECT expires_at FROM actions WHERE id='stalled'")
	if err != nil || expires != 0 {
		t.Fatalf(
			"legacy stall was not marked for deadline restoration: expiry=%d, err=%v",
			expires,
			err,
		)
	}
}

// A database already migrated by the pre-sqlitestore code has the execution
// flag columns and PRAGMA user_version = 1. Open must adopt it without
// re-applying migrations and without losing sessions.
func TestOpenAdoptsLegacyVersionOneDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	old, err := sqlx.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.ExecContext(t.Context(), schema+`
		ALTER TABLE actions ADD COLUMN cancel_requested INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE actions ADD COLUMN execution_started INTEGER NOT NULL DEFAULT 0;
		INSERT INTO sessions(token_hash, issuer, subject, created_at, renewed_at, expires_at)
		VALUES ('hash', 'issuer', 'owner', 't', 't', '9999');
		PRAGMA user_version = 1;
	`)
	if err != nil {
		t.Fatal(err)
	}
	err = old.Close()
	if err != nil {
		t.Fatal(err)
	}

	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var version int
	err = db.GetContext(t.Context(), &version, "PRAGMA user_version")
	if err != nil {
		t.Fatal(err)
	}
	if version != len(migrations) {
		t.Fatalf("version = %d, want %d", version, len(migrations))
	}
	var subject string
	err = db.GetContext(t.Context(), &subject,
		"SELECT subject FROM sessions WHERE token_hash='hash'")
	if err != nil || subject != "owner" {
		t.Fatalf("session lost: subject=%q err=%v", subject, err)
	}
}
