package mail

import (
	"bytes"
	"errors"
	"io"
	"mime/quotedprintable"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"

	_ "github.com/emersion/go-message/charset" // Register legacy mail charsets.
)

const mimeDepth = 32

type Attachment struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

type Message struct {
	HTMLBody    string       `json:"-"`
	From        []string     `json:"from"`
	To          []string     `json:"to"`
	Cc          []string     `json:"cc"`
	Subject     string       `json:"subject"`
	Body        string       `json:"body"`
	Attachments []Attachment `json:"attachments"`
	Truncated   bool         `json:"truncated"`
}

type Envelope struct {
	From       string
	Recipients []string
}

func Inspect(raw []byte, limit int) (Message, error) {
	entity, err := message.Read(bytes.NewReader(raw))
	if !readableEntity(entity, err) {
		return Message{}, ErrInput
	}

	header := mail.Header{Header: entity.Header}

	subject, err := header.Subject()
	if err != nil {
		subject = header.Get("Subject")
	}

	msg := Message{
		From: displayAddresses(&header, "From"), To: displayAddresses(&header, "To"),
		Cc: displayAddresses(&header, "Cc"), Subject: subject,
		Attachments: []Attachment{},
	}

	body, err := readPart(entity, &msg.Attachments, 0)
	if err != nil {
		return Message{}, err
	}

	if limit == 0 && body.plain && body.html != "" {
		msg.HTMLBody = body.html
	}
	msg.Body, msg.Truncated = capBody(strings.ToValidUTF8(body.text, "\uFFFD"), limit)

	return msg, nil
}

func readableEntity(entity *message.Entity, err error) bool {
	return entity != nil &&
		(err == nil || message.IsUnknownCharset(err) || message.IsUnknownEncoding(err))
}

func displayAddresses(header *mail.Header, name string) []string {
	addrs, err := header.AddressList(name)
	if err != nil {
		return []string{header.Get(name)}
	}

	result := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		result = append(result, addr.String())
	}

	return result
}

type bodyPart struct {
	html  string
	text  string
	plain bool
}

func readPart(entity *message.Entity, attachments *[]Attachment, depth int) (bodyPart, error) {
	if depth > mimeDepth {
		return bodyPart{}, ErrUnsafe
	}

	mediaType, params, err := entity.Header.ContentType()
	if err != nil {
		return bodyPart{}, ErrInput
	}

	disposition, filenameParams, err := entity.Header.ContentDisposition()
	if err != nil && entity.Header.Has("Content-Disposition") {
		return bodyPart{}, ErrInput
	}

	name := filenameParams["filename"]
	if name == "" {
		name = params["name"]
	}

	if disposition == "attachment" || name != "" {
		return readAttachment(entity, attachments, name, mediaType)
	}

	if parts := entity.MultipartReader(); parts != nil {
		return readMultipart(parts, mediaType == "multipart/alternative", attachments, depth)
	}

	if mediaType != "text/plain" && mediaType != "text/html" {
		return readAttachment(entity, attachments, name, mediaType)
	}

	content, err := io.ReadAll(entity.Body)
	if err != nil {
		return bodyPart{}, ErrInput
	}

	if mediaType == "text/plain" {
		return bodyPart{text: string(content), plain: true}, nil
	}

	text := visibleHTML(content)

	return bodyPart{text: text, html: text}, nil
}

func readAttachment(entity *message.Entity, attachments *[]Attachment, name, mediaType string,
) (bodyPart, error) {
	size, err := io.Copy(io.Discard, entity.Body)
	if err != nil {
		return bodyPart{}, ErrInput
	}

	*attachments = append(*attachments, Attachment{Name: name, Type: mediaType, Size: size})

	return bodyPart{}, nil
}

func readMultipart(parts message.MultipartReader, alternative bool, attachments *[]Attachment,
	depth int,
) (bodyPart, error) {
	var (
		result           bodyPart
		texts, htmlTexts []string
	)

	for {
		part, err := parts.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}

		if !readableEntity(part, err) {
			return bodyPart{}, ErrInput
		}

		body, err := readPart(part, attachments, depth+1)
		if err != nil {
			return bodyPart{}, err
		}

		if body.html != "" {
			htmlTexts = append(htmlTexts, body.html)
		}
		if body.text == "" {
			continue
		}

		if alternative {
			if result.text == "" || (body.plain && !result.plain) {
				result = body
			}
		} else {
			texts = append(texts, body.text)
			result.plain = result.plain || body.plain
		}
	}

	if !alternative {
		result.text = strings.Join(texts, "\n\n")
	}

	result.html = strings.Join(htmlTexts, "\n\n")

	return result, nil
}

func capBody(body string, limit int) (string, bool) {
	if limit <= 0 || len(body) <= limit {
		return body, false
	}

	for !utf8.RuneStart(body[limit]) {
		limit--
	}

	return body[:limit], true
}

// SMTPEnvelope rejects ambiguous header sets and payloads SMTP DATA would rewrite.
func SMTPEnvelope(raw []byte) (Envelope, error) {
	entity, err := message.Read(bytes.NewReader(raw))
	if err != nil || !canonicalSMTP(raw) {
		return Envelope{}, ErrInput
	}

	header := mail.Header{Header: entity.Header}
	err = validateSendHeaders(&header)
	if err != nil {
		return Envelope{}, err
	}

	from, err := header.AddressList("From")
	if err != nil || len(from) != 1 || !safeAddress(from[0].Address) {
		return Envelope{}, ErrInput
	}

	result := Envelope{From: from[0].Address}

	for _, name := range []string{"To", "Cc"} {
		if strings.TrimSpace(header.Get(name)) == "" {
			continue
		}

		recipients, err := header.AddressList(name)
		if err != nil {
			return Envelope{}, ErrInput
		}

		for _, addr := range recipients {
			if !safeAddress(addr.Address) {
				return Envelope{}, ErrInput
			}

			result.Recipients = append(result.Recipients, addr.Address)
		}
	}

	if len(result.Recipients) == 0 {
		return Envelope{}, ErrInput
	}

	return result, nil
}

func validateSendHeaders(header *mail.Header) error {
	fields := header.Fields()
	seen := make(map[string]bool)

	for fields.Next() {
		name := strings.ToLower(fields.Key())
		switch {
		case name == "bcc", strings.HasPrefix(name, "resent-"):
			return ErrUnsafe
		case name == "from", name == "to", name == "cc", name == "subject",
			name == "sender", name == "content-type", name == "content-transfer-encoding":
			if seen[name] {
				return ErrUnsafe
			}

			seen[name] = true
		}
	}

	return nil
}

func canonicalSMTP(raw []byte) bool {
	if !bytes.HasSuffix(raw, []byte("\r\n")) || bytes.IndexByte(raw, 0) >= 0 {
		return false
	}

	for i, char := range raw {
		if char == '\r' && (i+1 == len(raw) || raw[i+1] != '\n') {
			return false
		}

		if char == '\n' && (i == 0 || raw[i-1] != '\r') {
			return false
		}
	}

	return true
}

func safeAddress(addr string) bool {
	return !strings.ContainsAny(addr, "<>") && !strings.ContainsFunc(addr, unicode.IsControl)
}

type Compose struct {
	Subject  string   `json:"subject"`
	BodyText string   `json:"body_text"`
	To       []string `json:"to"`
	Cc       []string `json:"cc,omitempty"`
}

func Build(from string, input Compose, reply []byte) ([]byte, []string, error) {
	if strings.ContainsFunc(input.Subject, unicode.IsControl) ||
		len(input.BodyText) > maxMessageBytes {
		return nil, nil, ErrInput
	}

	fromAddr, err := parseAddress(from)
	if err != nil {
		return nil, nil, err
	}

	var header mail.Header
	header.SetAddressList("From", []*mail.Address{fromAddr})
	header.SetSubject(input.Subject)
	header.SetDate(time.Now())

	err = header.GenerateMessageIDWithHostname(
		fromAddr.Address[strings.LastIndexByte(fromAddr.Address, '@')+1:],
	)
	if err != nil {
		return nil, nil, ErrInput
	}

	if len(reply) > 0 {
		err = setReplyHeaders(&header, reply)
		if err != nil {
			return nil, nil, err
		}
	}

	recipients, err := setRecipients(&header, input.To, input.Cc)
	if err != nil {
		return nil, nil, err
	}

	raw, err := writePlainMessage(&header, input.BodyText)

	return raw, recipients, err
}

func parseAddress(raw string) (*mail.Address, error) {
	if strings.ContainsFunc(raw, unicode.IsControl) {
		return nil, ErrInput
	}

	addr, err := mail.ParseAddress(raw)
	if err != nil || !safeAddress(addr.Address) {
		return nil, ErrInput
	}

	return addr, nil
}

func setRecipients(header *mail.Header, to, cc []string) ([]string, error) {
	var recipients []string

	for _, group := range []struct {
		name  string
		addrs []string
	}{{"To", to}, {"Cc", cc}} {
		var addrs []*mail.Address
		for _, raw := range group.addrs {
			addr, err := parseAddress(raw)
			if err != nil {
				return nil, err
			}

			addrs = append(addrs, addr)
			recipients = append(recipients, addr.Address)
		}

		if len(addrs) != 0 {
			header.SetAddressList(group.name, addrs)
		}
	}

	return recipients, nil
}

func setReplyHeaders(header *mail.Header, raw []byte) error {
	entity, err := message.Read(bytes.NewReader(raw))
	if !readableEntity(entity, err) {
		return ErrInput
	}

	parent := mail.Header{Header: entity.Header}

	id, err := parent.MessageID()
	if err != nil || id == "" || strings.ContainsFunc(id, unicode.IsControl) {
		return ErrInput
	}

	refs, err := parent.MsgIDList("References")
	if err != nil {
		return ErrInput
	}
	if len(refs) == 0 {
		refs, err = parent.MsgIDList("In-Reply-To")
		if err != nil {
			return ErrInput
		}
	}

	if !slices.Contains(refs, id) {
		refs = append(refs, id)
	}

	header.SetMsgIDList("In-Reply-To", []string{id})
	header.SetMsgIDList("References", refs)

	return nil
}

func writePlainMessage(header *mail.Header, body string) ([]byte, error) {
	header.Set("MIME-Version", "1.0")
	header.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
	header.Set("Content-Transfer-Encoding", "quoted-printable")

	var raw bytes.Buffer
	err := textproto.WriteHeader(&raw, header.Header.Header)
	if err != nil {
		return nil, ErrInput
	}

	body = strings.ReplaceAll(body, "\r\n", "\n")

	body = strings.ReplaceAll(body, "\r", "\n")
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}

	writer := quotedprintable.NewWriter(&raw)
	_, err = io.WriteString(writer, body)
	if err != nil {
		return nil, ErrInput
	}

	if writer.Close() != nil {
		return nil, ErrInput
	}

	if raw.Len() > maxMessageBytes {
		return nil, ErrTooLarge
	}

	return raw.Bytes(), nil
}
