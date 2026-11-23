package actions

import (
	"context"
	"fmt"
	"time"

	"github.com/zekihan/proton-mcp/internal/store"
)

func (svc *Service) cancel(ctx context.Context, id string) error {
	tx, err := svc.DB.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin cancellation: %w", err)
	}
	defer rollback(tx)

	row, err := getRecord(ctx, tx, id)
	if err != nil {
		return err
	}
	now := time.Now().UnixNano()
	if row.ExecutionStarted {
		_, err = tx.ExecContext(ctx, "UPDATE actions SET cancel_requested=1 WHERE id=?", id)
		if err != nil {
			return fmt.Errorf("record late cancellation: %w", err)
		}
		err = svc.commit(ctx, tx, &row, row.event("cancel_too_late"))
		if err != nil {
			return err
		}

		operation := "sending"
		if row.Kind == Delete {
			operation = "deletion"
		}

		return fmt.Errorf("%w: %s has started", ErrTooLate, operation)
	}
	if row.Status != Queued && (row.Status != Pending || row.Expires <= now) {
		return ErrConflict
	}
	_, err = tx.ExecContext(ctx, `UPDATE actions SET cancel_requested=1, status=?, run_at=0,
		claimed=0 WHERE id=?`, Cancelled, id)
	if err != nil {
		return fmt.Errorf("cancel action: %w", err)
	}
	row.Status = Cancelled

	return svc.commit(ctx, tx, &row, row.event(Cancelled))
}

// beginExecution is the cancellation boundary. The Bridge calls it after its
// connection and checks are ready, immediately before the first write command.
func (svc *Service) beginExecution(ctx context.Context, row *record, event store.Event) error {
	tx, err := svc.DB.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin execution: %w", err)
	}
	defer rollback(tx)
	result, err := tx.ExecContext(ctx, `UPDATE actions SET execution_started=1
		WHERE id=? AND status=? AND claimed=1 AND cancel_requested=0 AND execution_started=0`,
		row.ID, Queued)
	if err != nil {
		return fmt.Errorf("start execution: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count execution starts: %w", err)
	}
	if count != 1 {
		return ErrConflict
	}

	return svc.commit(ctx, tx, row, event)
}
