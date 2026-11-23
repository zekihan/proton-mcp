package actions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/zekihan/proton-mcp/internal/mail"
)

const (
	tickInterval     = 250 * time.Millisecond
	executionTimeout = 90 * time.Second
	finalizeTimeout  = 5 * time.Second
	batchSize        = 32
)

const interrupted = "Execution was interrupted. Verify the mailbox before making another request."

// Recover runs once before serving HTTP or starting the scheduler.
func (svc *Service) Recover(ctx context.Context, started time.Time) error {
	err := svc.reconcile(ctx, Queued, mutation{
		status: Failed, detail: interrupted, condition: "claimed = 1",
	})
	if err != nil {
		return err
	}

	return svc.reconcile(ctx, Queued, mutation{
		status: Stalled, condition: "claimed = 0 AND run_at < ?",
		args: []any{started.UnixNano()},
	})
}

func (svc *Service) reconcile(ctx context.Context, current string, change mutation) error {
	for {
		ids, err := svc.matching(ctx, current, change.condition, change.args...)
		if err != nil || len(ids) == 0 {
			return err
		}

		for _, id := range ids {
			row, err := svc.get(ctx, id)
			if err != nil {
				return err
			}

			if row.Status != current {
				continue
			}

			err = svc.transition(ctx, &row, change)
			if err != nil && !errors.Is(err, ErrConflict) {
				return err
			}
		}
	}
}

func (svc *Service) matching(ctx context.Context, status, condition string,
	args ...any,
) ([]string, error) {
	var ids []string

	arguments := append([]any{status}, args...)
	arguments = append(arguments, batchSize)

	err := svc.DB.SelectContext(ctx, &ids,
		"SELECT id FROM actions WHERE status=? AND "+condition+" ORDER BY run_at, id LIMIT ?",
		arguments...)
	if err != nil {
		return nil, fmt.Errorf("select scheduled actions: %w", err)
	}

	return ids, nil
}

func (svc *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for ctx.Err() == nil {
		err := svc.Tick(ctx)
		if err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "Could not process scheduled actions")
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (svc *Service) Tick(ctx context.Context) error {
	now := time.Now().UnixNano()
	err := svc.reconcile(ctx, Pending, mutation{
		status: Expired, condition: "expires_at <= ?", args: []any{now},
	})
	if err != nil {
		return err
	}

	ids, err := svc.matching(ctx, Queued, "claimed=0 AND run_at<=?", now)
	if err != nil {
		return err
	}

	for _, id := range ids {
		err = svc.runAction(ctx, id, now)
		if err != nil {
			return err
		}
	}

	if svc.Telegram != nil {
		return svc.Telegram.deliver(ctx, svc.DB)
	}

	return nil
}

func (svc *Service) runAction(ctx context.Context, id string, dueAt int64) error {
	row, err := svc.get(ctx, id)
	if err != nil {
		return err
	}

	if row.Status != Queued || row.Claimed {
		return nil
	}

	err = svc.transition(ctx, &row, mutation{
		status: Queued, runAt: row.RunAt, claim: true,
		condition: "claimed=0 AND run_at<=?", args: []any{dueAt},
	})
	if errors.Is(err, ErrConflict) {
		return nil
	}

	if err != nil {
		return err
	}

	row.Claimed = true
	opCtx, stop := context.WithTimeout(ctx, executionTimeout)
	status, detail := svc.execute(opCtx, &row)

	stop()

	finalizeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizeTimeout)
	defer cancel()

	return svc.transition(finalizeCtx, &row, mutation{
		status: status, detail: detail, condition: "claimed=1",
	})
}

func (svc *Service) execute(ctx context.Context, row *record) (string, string) {
	snapshot, err := row.snapshot()
	if err != nil {
		return Failed, ErrIntegrity.Error()
	}

	switch row.Kind {
	case SendDraft, Forward:
		return svc.send(ctx, row.Kind, &snapshot)
	case Delete:
		uids := make([]uint32, 0, len(snapshot.Messages))
		for _, msg := range snapshot.Messages {
			uids = append(uids, msg.UID)
		}

		err = svc.Bridge.Expunge(ctx, snapshot.Location.Folder, snapshot.Location.Validity,
			uids, true)
		if err != nil {
			return Failed, "Permanent deletion could not be confirmed."
		}

		return Done, ""
	default:
		return Failed, "Unknown action kind."
	}
}

func (svc *Service) send(ctx context.Context, kind string, snapshot *Snapshot) (string, string) {
	err := svc.Bridge.Send(ctx, snapshot.Raw)
	if err != nil {
		if errors.Is(err, mail.ErrDeliveryUnknown) {
			return Failed, mail.ErrDeliveryUnknown.Error() + ". Verify Sent before requesting again."
		}

		return Failed, "SMTP submission failed."
	}

	var detail string
	if kind == SendDraft && svc.cleanupSentDraft(ctx, snapshot) != nil {
		detail = "Draft cleanup could not be confirmed."
	}

	if svc.SaveSentCopy && svc.Bridge.AppendSent(ctx, snapshot.Raw) != nil {
		if detail != "" {
			detail += " "
		}

		detail += "Saving a Sent copy could not be confirmed."
	}

	return Sent, detail
}
