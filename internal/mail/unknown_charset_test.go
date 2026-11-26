package mail

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestInspectToleratesUnknownCharsetAndEncoding(t *testing.T) {
	const unknownSubject = "=?unknown-8bit?Q?Example?="

	tests := []struct {
		name        string
		raw         string
		body        string
		subject     string
		attachments []Attachment
	}{
		{
			name: "unknown-8bit body charset",
			raw:  "Content-Type: text/plain; charset=unknown-8bit\r\n\r\nHello \xffworld",
			body: "Hello \uFFFDworld",
		},
		{
			name: "x-uuencode attachment",
			raw: "Content-Type: multipart/mixed; boundary=example\r\n\r\n" +
				"--example\r\n" +
				"Content-Type: application/octet-stream\r\n" +
				"Content-Disposition: attachment; filename=example.bin\r\n" +
				"Content-Transfer-Encoding: x-uuencode\r\n\r\n" +
				"example\r\n" +
				"--example\r\n" +
				"Content-Type: text/plain\r\n\r\n" +
				"Hello world\r\n" +
				"--example--\r\n",
			body: "Hello world",
			attachments: []Attachment{
				{Name: "example.bin", Type: "application/octet-stream", Size: 7},
			},
		},
		{
			name:    "unknown charset in subject encoded-word",
			raw:     "Subject: " + unknownSubject + "\r\n\r\nHello world",
			body:    "Hello world",
			subject: unknownSubject,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Inspect([]byte(tt.raw), 0)
			if err != nil {
				t.Fatalf("Inspect() error = %v", err)
			}
			if got.Body != tt.body || got.Subject != tt.subject {
				t.Errorf(
					"Inspect() body = %q, subject = %q, want %q and %q",
					got.Body,
					got.Subject,
					tt.body,
					tt.subject,
				)
			}
			if !slices.Equal(got.Attachments, tt.attachments) {
				t.Errorf("Inspect() attachments = %+v, want %+v", got.Attachments, tt.attachments)
			}
		})
	}
}

func TestInspectRejectsMalformedMIME(t *testing.T) {
	for name, raw := range map[string]string{
		"malformed header": "Invalid header\r\n\r\nBody",
		"missing boundary": "Content-Type: multipart/mixed\r\n\r\nBody",
		"malformed part header": "Content-Type: multipart/mixed; boundary=example\r\n\r\n" +
			"--example\r\nInvalid header\r\n\r\nBody\r\n--example--\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Inspect([]byte(raw), 0)
			if !errors.Is(err, ErrInput) {
				t.Fatalf("Inspect() error = %v, want %v", err, ErrInput)
			}
		})
	}
}

func TestBuildRepliesToUnknownCharset(t *testing.T) {
	parent := []byte("From: sender@example.com\r\n" +
		"To: recipient@example.com\r\n" +
		"Message-ID: <parent@example.com>\r\n" +
		"Content-Type: text/plain; charset=unknown-8bit\r\n\r\nHello world\r\n")

	raw, _, err := Build("recipient@example.com", Compose{
		To:       []string{"sender@example.com"},
		Subject:  "Example reply",
		BodyText: "Hello",
	}, parent)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	for _, field := range []string{"In-Reply-To", "References"} {
		if !strings.Contains(string(raw), field+": <parent@example.com>\r\n") {
			t.Errorf("Build() missing %s for the parent message", field)
		}
	}
	_, err = SMTPEnvelope(parent)
	if !errors.Is(err, ErrInput) {
		t.Errorf("SMTPEnvelope() error = %v, want %v", err, ErrInput)
	}
}
