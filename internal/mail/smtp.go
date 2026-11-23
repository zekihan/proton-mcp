package mail

import (
	"bytes"
	"context"
	"errors"

	"github.com/emersion/go-smtp"
)

var ErrDeliveryUnknown = errors.New("SMTP acceptance could not be confirmed")

func (bridge *Bridge) Send(ctx context.Context, raw []byte, beforeWrite func() error) error {
	envelope, err := SMTPEnvelope(raw)
	if err != nil {
		return err
	}

	client, closeBridge, err := bridge.smtp(ctx)
	if err != nil {
		return err
	}
	defer closeBridge()

	err = beforeWrite()
	if err != nil {
		return err
	}
	err = client.SendMail(
		envelope.From,
		envelope.Recipients,
		bytes.NewReader(raw),
	)
	if err != nil {
		if _, ok := errors.AsType[*smtp.SMTPError](err); ok {
			return ErrBridge
		}

		return ErrDeliveryUnknown
	}

	return nil
}
