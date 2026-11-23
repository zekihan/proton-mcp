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
