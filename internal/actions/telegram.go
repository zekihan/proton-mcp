package actions

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"
)

const (
	telegramTimeout  = 3 * time.Second
	telegramRetry    = 30 * time.Second
	maxTelegramField = 500
)

type Telegram struct {
	PublicURL string
	APIURL    string
	Token     string
	ChatID    string
}

func (telegram *Telegram) enqueue(ctx context.Context, tx *sqlx.Tx, row *record) error {
	link := telegram.PublicURL + "/approve/" + row.ID

	var text string

	switch row.Status {
	case Pending:
		text = "Approval requested for " + row.Kind + ".\n" + link
	case Queued:
		text = "Approved, sending at " + time.Unix(0, row.RunAt).UTC().Format("15:04") +
			" UTC.\nReview or cancel: " + link
		if row.Kind == Delete {
			text = "Approved, deleting at " + time.Unix(0, row.RunAt).UTC().Format("15:04") +
				" UTC.\nReview or cancel: " + link
		}
	case Stalled:
		text = "Action stalled. Review and confirm again.\n" + link
	case Sent:
		text = "Sent."
	case Failed:
		text = "Action failed.\n" + link
	default:
		return nil
	}

	if row.Detail != "" {
		text += "\n" + row.Detail
	}

	snapshot, err := row.snapshot()
	if err == nil && len(snapshot.Recipients) > 0 {
		text += "\nSubject: " + telegramField(snapshot.Subject) +
			"\nRecipients: " + telegramField(strings.Join(snapshot.Recipients, ", "))
	}

	_, err = tx.ExecContext(
		ctx,
		"INSERT INTO telegram_outbox(text) VALUES (?)",
		text,
	)
	if err != nil {
		return fmt.Errorf("enqueue Telegram notification: %w", err)
	}

	return nil
}

func telegramField(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= maxTelegramField {
		return value
	}

	end := maxTelegramField
	for !utf8.RuneStart(value[end]) {
		end--
	}

	return value[:end] + "…"
}

func (telegram *Telegram) deliver(ctx context.Context, db *sqlx.DB) error {
	var (
		id   int64
		text string
	)

	err := db.QueryRowxContext(ctx, `SELECT id,text FROM telegram_outbox
		WHERE try_after<=? ORDER BY id LIMIT 1`, time.Now().UnixNano()).Scan(&id, &text)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("read Telegram outbox: %w", err)
	}

	if telegram.send(ctx, text) {
		_, err = db.ExecContext(ctx, "DELETE FROM telegram_outbox WHERE id=?", id)
	} else {
		slog.WarnContext(ctx, "Telegram notification could not be delivered")
		_, err = db.ExecContext(ctx, "UPDATE telegram_outbox SET try_after=? WHERE id=?",
			time.Now().Add(telegramRetry).UnixNano(), id)
	}

	if err != nil {
		return fmt.Errorf("update Telegram outbox: %w", err)
	}

	return nil
}

func (telegram *Telegram) send(ctx context.Context, text string) bool {
	payload, err := json.Marshal(map[string]any{
		"chat_id": telegram.ChatID, "text": text, "link_preview_options": map[string]bool{
			"is_disabled": true,
		},
	})
	if err != nil {
		return false
	}

	endpoint := strings.TrimRight(telegram.APIURL, "/") + "/bot" + telegram.Token + "/sendMessage"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return false
	}

	req.Header.Set("Content-Type", "application/json")

	client := http.Client{
		Timeout: telegramTimeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return false
	}

	defer func() {
		err := resp.Body.Close()
		if err != nil {
			slog.DebugContext(ctx, "Could not close Telegram response")
		}
	}()

	var result struct {
		OK bool `json:"ok"`
	}

	return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&result) == nil &&
		result.OK
}
