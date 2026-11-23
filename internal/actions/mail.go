package actions

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zekihan/proton-mcp/internal/mail"
)

type SaveDraftInput struct {
	mail.Compose

	ReplyFolder string `json:"in_reply_to_folder,omitempty"`
	ReplyUID    uint32 `json:"in_reply_to_uid,omitempty"`
	ReplaceUID  uint32 `json:"replace_draft_uid,omitempty"`
}

type SavedDraft struct {
	mail.Location

	Recipients []string `json:"-"`
	Detail     string   `json:"detail,omitempty"`
}

func (svc *Service) SaveDraft(ctx context.Context, input SaveDraftInput) (SavedDraft, error) {
	if (input.ReplyFolder == "") != (input.ReplyUID == 0) {
		return SavedDraft{}, mail.ErrInput
	}

	folder, err := svc.Bridge.Drafts(ctx)
	if err != nil {
		return SavedDraft{}, err
	}

	var reply []byte
	if input.ReplyUID != 0 {
		_, reply, err = svc.Bridge.Raw(ctx, input.ReplyFolder, input.ReplyUID)
		if err != nil {
			return SavedDraft{}, err
		}
	}

	raw, recipients, err := mail.Build(svc.Bridge.Username, input.Compose, reply)
	if err != nil {
		return SavedDraft{}, err
	}

	svc.draftMu.Lock()
	defer svc.draftMu.Unlock()

	var old mail.Location
	if input.ReplaceUID != 0 {
		old, err = svc.serverDraft(ctx, folder, input.ReplaceUID)
		if err != nil {
			return SavedDraft{}, err
		}
	}

	loc, err := svc.Bridge.AppendDraft(ctx, folder, raw)
	if err != nil {
		return SavedDraft{}, err
	}

	result := SavedDraft{Location: loc, Recipients: recipients}

	_, err = svc.DB.ExecContext(
		ctx,
		"INSERT INTO drafts(folder, uid_validity, uid) VALUES (?, ?, ?)",
		loc.Folder,
		loc.Validity,
		loc.UID,
	)
	if err != nil {
		result.Detail = "Draft was appended, but ownership could not be recorded."

		return result, nil
	}

	if old.UID != 0 && svc.removeOldDraft(ctx, old, loc) != nil {
		result.Detail = "Draft was saved, but replacement cleanup could not be confirmed."
	}

	return result, nil
}

func (svc *Service) serverDraft(
	ctx context.Context,
	folder string,
	uid uint32,
) (mail.Location, error) {
	loc, _, err := svc.Bridge.Raw(ctx, folder, uid)
	if err != nil {
		return loc, err
	}

	var tracked int

	err = svc.DB.GetContext(ctx, &tracked,
		"SELECT 1 FROM drafts WHERE folder=? AND uid_validity=? AND uid=?",
		loc.Folder, loc.Validity, loc.UID)
	if errors.Is(err, sql.ErrNoRows) {
		return loc, mail.ErrUnsafe
	}

	if err != nil {
		return loc, fmt.Errorf("check draft ownership: %w", err)
	}

	return loc, nil
}

func (svc *Service) removeOldDraft(ctx context.Context, old, latest mail.Location) error {
	if old.Validity != latest.Validity || old.UID == latest.UID {
		return mail.ErrUnsafe
	}

	return svc.expungeDraft(ctx, old)
}

func (svc *Service) expungeDraft(ctx context.Context, loc mail.Location) error {
	err := svc.Bridge.Expunge(ctx, loc.Folder, loc.Validity, []uint32{loc.UID}, false)
	if err != nil {
		return err
	}

	_, err = svc.DB.ExecContext(
		ctx,
		"DELETE FROM drafts WHERE folder=? AND uid_validity=? AND uid=?",
		loc.Folder,
		loc.Validity,
		loc.UID,
	)
	if err != nil {
		return fmt.Errorf("remove draft tracking: %w", err)
	}

	return nil
}

func (svc *Service) RequestSend(ctx context.Context, uid uint32) (Action, error) {
	folder, err := svc.Bridge.Drafts(ctx)
	if err != nil {
		return Action{}, err
	}

	loc, raw, err := svc.Bridge.Raw(ctx, folder, uid)
	if err != nil {
		return Action{}, err
	}

	return svc.requestMail(ctx, SendDraft, loc, raw)
}

func (svc *Service) RequestForward(ctx context.Context, folder string, uid uint32, to []string,
	note string,
) (Action, error) {
	loc, raw, err := svc.Bridge.Raw(ctx, folder, uid)
	if err != nil {
		return Action{}, err
	}

	msg, err := mail.Inspect(raw, 0)
	if err != nil {
		return Action{}, err
	}

	note += "\n\n---------- Forwarded message ----------\n" + msg.Body

	raw, _, err = mail.Build(svc.Bridge.Username, mail.Compose{
		To: to, Subject: "Fwd: " + strings.Join(strings.Fields(msg.Subject), " "), BodyText: note,
	}, nil)
	if err != nil {
		return Action{}, err
	}

	return svc.requestMail(ctx, Forward, loc, raw)
}

func (svc *Service) requestMail(ctx context.Context, kind string, loc mail.Location,
	raw []byte,
) (Action, error) {
	envelope, err := mail.SMTPEnvelope(raw)
	if err != nil {
		return Action{}, err
	}

	msg, err := mail.Inspect(raw, 0)
	if err != nil {
		return Action{}, err
	}

	return svc.create(ctx, kind, &Snapshot{
		Location: loc, Raw: raw, Subject: msg.Subject, Recipients: envelope.Recipients,
	})
}

func (svc *Service) RequestDelete(
	ctx context.Context,
	folder string,
	uids []uint32,
) (Action, error) {
	validity, messages, err := svc.Bridge.DeletePreview(ctx, folder, uids)
	if err != nil {
		return Action{}, err
	}

	return svc.create(ctx, Delete, &Snapshot{
		Location: mail.Location{Folder: folder, Validity: validity}, Messages: messages,
	})
}

func (svc *Service) cleanupSentDraft(ctx context.Context, snapshot *Snapshot) error {
	svc.draftMu.Lock()
	defer svc.draftMu.Unlock()

	loc, raw, err := svc.Bridge.Raw(ctx, snapshot.Location.Folder, snapshot.Location.UID)
	if errors.Is(err, mail.ErrNotFound) {
		return nil
	}

	if err != nil {
		return err
	}

	if loc != snapshot.Location || !bytes.Equal(raw, snapshot.Raw) {
		return mail.ErrUnsafe
	}

	return svc.expungeDraft(ctx, loc)
}
