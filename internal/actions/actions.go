// Package actions owns mail operations that require an owner's approval.
package actions

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/zekihan/proton-mcp/internal/mail"
	"github.com/zekihan/proton-mcp/internal/store"
)

const (
	SendDraft = "send_draft"
	Forward   = "forward_message"
	Delete    = "delete_permanently"

	Pending   = "pending"
	Queued    = "approved_queued"
	Cancelled = "cancelled"
	Rejected  = "rejected"
	Expired   = "expired"
	Stalled   = "stalled"
	Sent      = "sent"
	Failed    = "failed"
	Done      = "done"
)

var (
	ErrConflict  = errors.New("action is no longer available for this operation")
	ErrTooLate   = errors.New("too late to cancel")
	ErrIntegrity = errors.New("action snapshot failed its integrity check")
)

type Service struct {
	DB           *sqlx.DB
	Bridge       *mail.Bridge
	Telegram     *Telegram
	Delay        time.Duration
	TTL          time.Duration
	MaxBody      int
	draftMu      sync.Mutex
	SaveSentCopy bool
}

type Snapshot struct {
	Location   mail.Location  `json:"location"`
	Messages   []mail.Summary `json:"messages,omitempty"`
	Raw        []byte         `json:"raw,omitempty"`
	RawSHA256  string         `json:"rfc822_sha256,omitempty"`
	Subject    string         `json:"subject,omitempty"`
	Recipients []string       `json:"recipients,omitempty"`
}

type Action struct {
	ID     string     `json:"action_id"`
	Kind   string     `json:"kind"`
	Status string     `json:"status"`
	RunAt  *time.Time `json:"run_at,omitempty"`
	Detail string     `json:"detail,omitempty"`
}

type record struct {
	ID               string `db:"id"`
	Kind             string `db:"kind"`
	Status           string `db:"status"`
	Digest           string `db:"digest"`
	Detail           string `db:"detail"`
	Snapshot         []byte `db:"snapshot"`
	Expires          int64  `db:"expires_at"`
	RunAt            int64  `db:"run_at"`
	Claimed          bool   `db:"claimed"`
	ExecutionStarted bool   `db:"execution_started"`
}

func (row *record) action() Action {
	action := Action{ID: row.ID, Kind: row.Kind, Status: row.Status, Detail: row.Detail}
	if row.RunAt != 0 && row.Status == Queued && !row.ExecutionStarted {
		runAt := time.Unix(0, row.RunAt).UTC()
		action.RunAt = &runAt
	}

	if row.ExecutionStarted && row.Status == Queued {
		action.Detail = "Execution has started."
	}

	return action
}

func digest(raw []byte) string {
	hash := sha256.Sum256(raw)

	return hex.EncodeToString(hash[:])
}

func (row *record) snapshotDigest() string {
	hash := sha256.New()
	hash.Write([]byte(row.ID + "\x00" + row.Kind + "\x00"))
	hash.Write(row.Snapshot)

	return hex.EncodeToString(hash.Sum(nil))
}

func (row *record) snapshot() (Snapshot, error) {
	var snapshot Snapshot
	if row.snapshotDigest() != row.Digest || json.Unmarshal(row.Snapshot, &snapshot) != nil ||
		(len(snapshot.Raw) > 0 && digest(snapshot.Raw) != snapshot.RawSHA256) {
		return Snapshot{}, ErrIntegrity
	}

	return snapshot, nil
}

func (row *record) event(status string) store.Event {
	event := store.Event{Action: row.Kind, ID: row.ID, Status: status}

	snapshot, err := row.snapshot()
	if err != nil {
		return event
	}

	return row.snapshotEvent(status, &snapshot)
}

func (row *record) snapshotEvent(status string, snapshot *Snapshot) store.Event {
	event := store.Event{Action: row.Kind, ID: row.ID, Status: status}
	event.Folder, event.Recipients = snapshot.Location.Folder, snapshot.Recipients
	if snapshot.Location.UID != 0 {
		event.UIDs = []uint32{snapshot.Location.UID}
	}

	for _, msg := range snapshot.Messages {
		event.UIDs = append(event.UIDs, msg.UID)
	}

	return event
}

func (svc *Service) Get(ctx context.Context, id string) (Action, error) {
	row, err := svc.lookup(ctx, id)
	if err != nil {
		return Action{}, err
	}

	return row.action(), nil
}

func (svc *Service) Review(ctx context.Context, id string) (Action, Snapshot, string, error) {
	row, err := svc.lookup(ctx, id)
	if err != nil {
		return Action{}, Snapshot{}, "", err
	}

	if row.Status != Pending && row.Status != Stalled && row.Status != Queued {
		return row.action(), Snapshot{}, "", nil
	}

	snapshot, err := row.snapshot()

	return row.action(), snapshot, row.Digest, err
}

func (svc *Service) Decide(ctx context.Context, id, decision, reviewedDigest string) error {
	if decision == "cancel" {
		return svc.cancel(ctx, id)
	}
	row, err := svc.lookup(ctx, id)
	if err != nil {
		return err
	}

	next, err := reviewDecision(&row, decision, reviewedDigest)
	if err != nil {
		return err
	}

	return svc.transition(ctx, &row, next)
}

func (svc *Service) create(ctx context.Context, kind string, snapshot *Snapshot) (Action, error) {
	if len(snapshot.Raw) > 0 {
		snapshot.RawSHA256 = digest(snapshot.Raw)
	}

	raw, err := json.Marshal(snapshot)
	if err != nil {
		return Action{}, fmt.Errorf("encode action snapshot: %w", err)
	}

	row := record{
		ID: uuid.NewString(), Kind: kind, Status: Pending, Snapshot: raw,
		Expires: time.Now().Add(svc.TTL).UnixNano(),
	}
	row.Digest = row.snapshotDigest()
	event := row.event(Pending)

	tx, err := svc.DB.BeginTxx(ctx, nil)
	if err != nil {
		return Action{}, fmt.Errorf("begin action: %w", err)
	}
	defer rollback(tx)

	_, err = tx.ExecContext(
		ctx,
		`INSERT INTO actions(id, kind, status, expires_at, snapshot, digest)
		VALUES (?, ?, ?, ?, ?, ?)`,
		row.ID,
		row.Kind,
		row.Status,
		row.Expires,
		raw,
		row.Digest,
	)
	if err != nil {
		return Action{}, fmt.Errorf("create action: %w", err)
	}

	err = svc.commit(ctx, tx, &row, event)
	if err != nil {
		return Action{}, err
	}

	return row.action(), nil
}

func (svc *Service) lookup(ctx context.Context, id string) (record, error) {
	row, err := svc.get(ctx, id)
	if err != nil {
		return record{}, err
	}

	now := time.Now().UnixNano()
	if awaitingApproval(row.Status) && row.Expires <= now {
		err = svc.transition(ctx, &row, mutation{
			status: Expired, condition: "expires_at <= ?", args: []any{now},
		})
		if err != nil && !errors.Is(err, ErrConflict) {
			return record{}, err
		}

		return svc.get(ctx, id)
	}

	return row, nil
}

func (svc *Service) get(ctx context.Context, id string) (record, error) {
	return getRecord(ctx, svc.DB, id)
}

func getRecord(ctx context.Context, queryer sqlx.QueryerContext, id string) (record, error) {
	var row record
	err := sqlx.GetContext(
		ctx,
		queryer,
		&row,
		`SELECT id,kind,status,expires_at,run_at,claimed,snapshot,digest,detail,execution_started FROM actions WHERE id=?`,
		id,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return row, mail.ErrNotFound
	}

	if err != nil {
		return row, fmt.Errorf("get action: %w", err)
	}

	return row, nil
}

func awaitingApproval(status string) bool {
	return status == Pending || status == Stalled
}

func reviewDecision(row *record, decision, reviewedDigest string) (mutation, error) {
	if !awaitingApproval(row.Status) {
		return mutation{}, ErrConflict
	}
	var next mutation

	switch decision {
	case "approve":
		_, err := row.snapshot()
		if err != nil {
			return mutation{}, err
		}

		if reviewedDigest != row.Digest {
			return mutation{}, ErrConflict
		}

		next.status = Queued
	case "reject":
		next.status = Rejected
	default:
		return mutation{}, ErrConflict
	}

	return next, nil
}

type mutation struct {
	status    string
	detail    string
	condition string
	args      []any
	runAt     int64
	claim     bool
}

func (svc *Service) transition(ctx context.Context, row *record, next mutation) error {
	tx, err := svc.DB.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin action transition: %w", err)
	}
	defer rollback(tx)

	now := time.Now()
	var expiry any
	if next.status == Stalled {
		expiry = now.Add(svc.TTL).UnixNano()
	}
	if awaitingApproval(row.Status) && (next.status == Queued || next.status == Rejected) {
		next.condition, next.args = "expires_at > ?", []any{now.UnixNano()}
		if next.status == Queued {
			next.runAt = now.Add(svc.Delay).UnixNano()
		}
	}
	const transitionParameters = 10
	args := make([]any, 0, transitionParameters+len(next.args))
	args = append(args,
		next.status, next.runAt, next.claim, next.detail, expiry, row.ID, row.Status,
		row.Claimed, row.Digest, row.RunAt,
	)
	args = append(args, next.args...)
	// The suffix is supplied by internal state transitions and contains no input.
	result, err := tx.ExecContext(
		ctx,
		`UPDATE actions SET status=?, run_at=?, claimed=?, detail=?, expires_at=COALESCE(?,expires_at)
		WHERE id=? AND status=? AND claimed=? AND digest=? AND run_at=? AND `+next.condition,
		args...)
	if err != nil {
		return fmt.Errorf("transition action: %w", err)
	}

	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count action transitions: %w", err)
	}

	if count != 1 {
		return ErrConflict
	}

	status := next.status
	if next.claim {
		status = "execution_claimed"
	}

	updated := *row
	updated.Status, updated.RunAt, updated.Claimed, updated.Detail =
		next.status, next.runAt, next.claim, next.detail

	return svc.commit(ctx, tx, &updated, updated.event(status))
}

func (svc *Service) commit(ctx context.Context, tx *sqlx.Tx, row *record, event store.Event) error {
	err := store.Record(ctx, tx, event)
	if err != nil {
		return err
	}

	if svc.Telegram != nil && row.Status == event.Status {
		err = svc.Telegram.enqueue(ctx, tx, row)
		if err != nil {
			return err
		}
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("commit action: %w", err)
	}

	store.Log(ctx, event)

	return nil
}

func rollback(tx *sqlx.Tx) {
	err := tx.Rollback()
	if err != nil && !errors.Is(err, sql.ErrTxDone) {
		slog.Error("Could not roll back action transaction")
	}
}
