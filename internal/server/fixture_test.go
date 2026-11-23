package server

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
	"github.com/golang-jwt/jwt/v5"
	"github.com/oauth2-proxy/mockoidc"

	"github.com/zekihan/proton-mcp/internal/actions"
	"github.com/zekihan/proton-mcp/internal/auth"
	"github.com/zekihan/proton-mcp/internal/mail"
	"github.com/zekihan/proton-mcp/internal/mcp"
	"github.com/zekihan/proton-mcp/internal/store"
)

const (
	publicURL  = "https://mail.example.com"
	owner      = "example-owner"
	username   = "owner@example.com"
	password   = "example-bridge-credential"
	drafts     = "Drafts"
	trash      = "Deleted Items"
	spam       = "Spam"
	sent       = "Sent"
	inbox      = "INBOX"
	recipient  = "recipient@example.com"
	secretBody = "private fixture body"
)

type fixture struct {
	handler       http.Handler
	service       *actions.Service
	user          *imapmemserver.User
	provider      *mockoidc.MockOIDC
	delivered     chan []byte
	notifications chan string
	dbPath        string
	rejectSMTP    atomic.Bool
}

var errTestRPC = errors.New("test MCP call failed")

func newFixture(t *testing.T) *fixture {
	t.Helper()
	fx := &fixture{
		delivered: make(chan []byte, 16), notifications: make(chan string, 32),
		dbPath: filepath.Join(t.TempDir(), "proton-mcp.db"),
	}
	fx.startMail(t)
	provider, err := mockoidc.Run()
	requireNoError(t, err)
	t.Cleanup(func() { requireNoError(t, provider.Shutdown()) })
	fx.provider = provider
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Text string `json:"text"`
		}
		if r.URL.Path != "/botexample-token/sendMessage" ||
			json.NewDecoder(r.Body).Decode(&payload) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)

			return
		}
		fx.notifications <- payload.Text
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(telegram.Close)
	fx.service.Telegram = &actions.Telegram{
		PublicURL: publicURL, APIURL: telegram.URL, Token: "example-token", ChatID: "example-chat",
	}
	fx.reopen(t)
	t.Cleanup(func() { requireNoError(t, fx.service.DB.Close()) })

	return fx
}

func (fx *fixture) reopen(t *testing.T) {
	t.Helper()
	if fx.service.DB != nil {
		requireNoError(t, fx.service.DB.Close())
	}
	db, err := store.Open(t.Context(), fx.dbPath)
	requireNoError(t, err)
	old := fx.service
	fx.service = &actions.Service{
		DB: db, Bridge: old.Bridge, Delay: old.Delay, TTL: old.TTL,
		MaxBody: old.MaxBody, Telegram: old.Telegram,
	}
	requireNoError(t, fx.service.Recover(t.Context(), time.Now()))
	identity, err := auth.New(t.Context(), &auth.Config{
		PublicURL: publicURL, Issuer: fx.provider.Issuer(),
		ClientID: fx.provider.ClientID, ClientSecret: fx.provider.ClientSecret,
		OwnerSubject: owner, MCPClientID: "proton-mcp-mcp", MCPAudience: publicURL + "/mcp",
	}, db)
	requireNoError(t, err)
	fx.handler = New(fx.service, identity, mcp.NewHandler(fx.service, "test"))
}

func (fx *fixture) startMail(t *testing.T) {
	t.Helper()
	serverTLS, clientTLS := mailTLS(t)
	fx.user = imapmemserver.NewUser(username, password)
	for _, name := range testFolders {
		requireNoError(t, fx.user.Create(name, nil))
	}
	memory := imapmemserver.New()
	memory.AddUser(fx.user)
	imapServer := imapserver.New(&imapserver.Options{
		NewSession: func(_ *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			session, ok := memory.NewSession().(imapserver.SessionIMAP4rev2)
			if !ok {
				return nil, nil, errTestRPC
			}

			return &testIMAPSession{
				SessionIMAP4rev2: session,
			}, nil, nil
		},
		TLSConfig: serverTLS,
		Caps: imap.CapSet{ //nolint:exhaustive // Advertise only the fixture capabilities.
			imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}, imap.CapSpecialUse: {},
		},
		Logger: quietIMAP{},
	})
	imapListener := listen(t)
	go func() { _ = imapServer.Serve(imapListener) }()
	t.Cleanup(func() { _ = imapServer.Close() })

	smtpServer := smtp.NewServer(smtp.BackendFunc(func(_ *smtp.Conn) (smtp.Session, error) {
		return &smtpSession{delivered: fx.delivered, reject: &fx.rejectSMTP}, nil
	}))
	smtpServer.Domain = "example.com"
	smtpServer.TLSConfig = serverTLS
	smtpListener := listen(t)
	go func() { _ = smtpServer.Serve(smtpListener) }()
	t.Cleanup(func() { _ = smtpServer.Close() })

	fx.service = &actions.Service{
		Bridge: &mail.Bridge{
			IMAPAddr: imapListener.Addr().String(), SMTPAddr: smtpListener.Addr().String(),
			Username: username, Password: password, TLSConfig: clientTLS,
		},
		Delay: 3 * time.Minute, TTL: time.Hour, MaxBody: 40,
	}
}

var testFolders = []string{inbox, drafts, trash, spam, sent, "Trash"}

type testIMAPSession struct {
	imapserver.SessionIMAP4rev2
}

// The in-memory backend's CREATE does not apply requested SPECIAL-USE flags.
func (session *testIMAPSession) List(writer *imapserver.ListWriter, ref string, patterns []string,
	options *imap.ListOptions,
) error {
	for _, name := range testFolders {
		for _, pattern := range patterns {
			if !imapserver.MatchList(name, '/', ref, pattern) {
				continue
			}
			data := &imap.ListData{Mailbox: name, Delim: '/'}
			if name == trash && options.ReturnSpecialUse {
				data.Attrs = []imap.MailboxAttr{imap.MailboxAttrTrash}
			}
			err := writer.WriteList(data)
			if err != nil {
				return fmt.Errorf("write fixture folder: %w", err)
			}

			break
		}
	}

	return nil
}

type quietIMAP struct{}

func (quietIMAP) Printf(string, ...any) {}

func listen(t *testing.T) net.Listener {
	t.Helper()
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	requireNoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	return listener
}

func mailTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	requireNoError(t, err)
	info := &x509.Certificate{
		SerialNumber: big.NewInt(1), IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, info, info, &key.PublicKey, key)
	requireNoError(t, err)
	cert, err := x509.ParseCertificate(der)
	requireNoError(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(cert)

	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	}, &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
}

type smtpSession struct {
	reject    *atomic.Bool
	delivered chan []byte
}

func (*smtpSession) Reset()                               {}
func (*smtpSession) Logout() error                        { return nil }
func (*smtpSession) Mail(string, *smtp.MailOptions) error { return nil }
func (*smtpSession) Rcpt(string, *smtp.RcptOptions) error { return nil }
func (*smtpSession) AuthMechanisms() []string             { return []string{sasl.Plain} }

//nolint:ireturn // Method required by go-smtp.
func (*smtpSession) Auth(mechanism string) (sasl.Server, error) {
	if mechanism != sasl.Plain {
		return nil, smtp.ErrAuthUnknownMechanism
	}

	return sasl.NewPlainServer(func(_, user, pass string) error {
		if user != username || pass != password {
			return smtp.ErrAuthFailed
		}

		return nil
	}), nil
}

func (session *smtpSession) Data(reader io.Reader) error {
	raw, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("read fixture SMTP DATA: %w", err)
	}
	if session.reject.Load() {
		return &smtp.SMTPError{Code: 554, Message: "Fixture rejected message"}
	}
	session.delivered <- raw

	return nil
}

type literal struct {
	*bytes.Reader
}

func (data literal) Size() int64 { return int64(data.Len()) }

func (fx *fixture) append(t *testing.T, folder, raw string, flags ...imap.Flag) uint32 {
	t.Helper()
	data, err := fx.user.Append(folder,
		literal{Reader: bytes.NewReader([]byte(raw))}, &imap.AppendOptions{Flags: flags})
	requireNoError(t, err)

	return uint32(data.UID)
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (fx *fixture) http(t *testing.T, method, target string, cookie *http.Cookie, form url.Values,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(
		t.Context(),
		method,
		target,
		strings.NewReader(form.Encode()),
	)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp := httptest.NewRecorder()
	fx.handler.ServeHTTP(resp, req)

	return resp
}

func expectCode(t *testing.T, resp *httptest.ResponseRecorder, code int) {
	t.Helper()
	if resp.Code != code {
		t.Fatalf("status = %d, want %d (%s)", resp.Code, code, resp.Body)
	}
}

func (fx *fixture) login(t *testing.T, subject, next string) *httptest.ResponseRecorder {
	t.Helper()
	user := mockoidc.DefaultUser()
	user.Subject = subject
	fx.provider.QueueUser(user)
	start := fx.http(t, http.MethodGet, "/auth/login?next="+url.QueryEscape(next), nil, nil)
	expectCode(t, start, http.StatusSeeOther)
	location, err := url.Parse(start.Header().Get("Location"))
	requireNoError(t, err)
	if location.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("missing PKCE")
	}
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}, Timeout: 3 * time.Second}
	req, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		location.String(),
		http.NoBody,
	)
	requireNoError(t, err)
	response, err := client.Do(req)
	requireNoError(t, err)
	defer response.Body.Close()
	callback, err := response.Location()
	requireNoError(t, err)

	return fx.http(t, http.MethodGet, callback.RequestURI(), start.Result().Cookies()[0], nil)
}

func (fx *fixture) ownerCookie(t *testing.T, next string) *http.Cookie {
	t.Helper()
	resp := fx.login(t, owner, next)
	expectCode(t, resp, http.StatusSeeOther)
	if next != "" && resp.Header().Get("Location") != next {
		t.Fatal("approval page was not restored after sign-in")
	}
	for _, cookie := range resp.Result().Cookies() {
		if cookie.Path == "/" && cookie.Value != "" {
			if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
				t.Fatal("insecure session cookie")
			}

			return cookie
		}
	}
	t.Fatal("missing session cookie")

	return nil
}

var formField = regexp.MustCompile(`name="(csrf|snapshot)" value="([^"]+)"`)

func (fx *fixture) form(t *testing.T, id string, cookie *http.Cookie) (url.Values, string) {
	t.Helper()
	page := fx.http(t, http.MethodGet, "/approve/"+id, cookie, nil)
	expectCode(t, page, http.StatusOK)
	values := make(url.Values)
	for _, match := range formField.FindAllStringSubmatch(page.Body.String(), -1) {
		values.Set(match[1], match[2])
	}

	return values, page.Body.String()
}

func (fx *fixture) bearer(t *testing.T, subject string) string {
	t.Helper()
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": fx.provider.Issuer(), "sub": subject, "aud": publicURL + "/mcp",
		"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "client_id": "proton-mcp-mcp",
	})
	kid, err := fx.provider.Keypair.KeyID()
	requireNoError(t, err)
	token.Header["kid"] = kid
	token.Header["typ"] = "at+jwt"
	raw, err := token.SignedString(fx.provider.Keypair.PrivateKey)
	requireNoError(t, err)

	return raw
}

func (fx *fixture) tool(t *testing.T, name, args string, into any) error {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`,
	))
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+fx.bearer(t, owner))
	resp := httptest.NewRecorder()
	fx.handler.ServeHTTP(resp, req)
	expectCode(t, resp, http.StatusOK)
	//nolint:tagliatelle // Decode MCP wire fields.
	var body struct {
		Result struct {
			Structured json.RawMessage `json:"structuredContent"`
			Content    []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	requireNoError(t, json.Unmarshal(resp.Body.Bytes(), &body))
	if len(body.Error) != 0 {
		return fmt.Errorf("%w: %s", errTestRPC, body.Error)
	}
	if body.Result.IsError {
		return fmt.Errorf("%w: %s", errTestRPC, body.Result.Content[0].Text)
	}
	if into != nil {
		err := json.Unmarshal(body.Result.Structured, into)
		if err != nil {
			return fmt.Errorf("decode tool output: %w", err)
		}
	}

	return nil
}
