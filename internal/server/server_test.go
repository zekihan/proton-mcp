package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/zekihan/proton-mcp/internal/actions"
	"github.com/zekihan/proton-mcp/internal/mail"
)

const htmlEmail = "From: sender@example.com\r\nTo: owner@example.com\r\n" +
	"Subject: Example HTML\r\nMessage-ID: <fixture@example.com>\r\n" +
	"Content-Type: text/html; charset=UTF-8\r\n\r\n" +
	"<head>HIDDEN-HEAD<title>HIDDEN-TITLE</title><style>HIDDEN-STYLE</style></head>" +
	"<body>Hello<!-- HIDDEN-COMMENT --><script>HIDDEN-SCRIPT</script>" +
	"<p hidden>HIDDEN-ATTR</p><p style='d\\69splay: N/**/ONE !important'>HIDDEN-DISPLAY</p>" +
	"<p style='visibility:hidden'>HIDDEN-VISIBILITY</p><p style='font-size:0px'>HIDDEN-SIZE</p>" +
	"<p style='opacity:0.00'>HIDDEN-OPACITY</p><p>Visible abcdefghijklmnopqrstuvwxyz " +
	"abcdefghijklmnopqrstuvwxyz</p></body>\r\n"

const multipartEmail = "From: sender@example.com\r\nTo: owner@example.com\r\n" +
	"Subject: Example multipart\r\nContent-Type: multipart/mixed; boundary=fixture\r\n\r\n" +
	"--fixture\r\nContent-Type: text/plain\r\n\r\n" + secretBody +
	"\r\n--fixture\r\nContent-Type: application/octet-stream\r\n" +
	"Content-Disposition: attachment; filename=example.bin\r\nContent-Transfer-Encoding: base64\r\n" +
	"\r\nYWJj\r\n--fixture--\r\n"

func testDraft(t *testing.T, fx *fixture, subject string) actions.SavedDraft {
	t.Helper()
	input, err := json.Marshal(mail.Compose{
		To: []string{recipient}, Subject: subject,
		BodyText: secretBody + "\n.dot\n",
	})
	requireNoError(t, err)
	var draft actions.SavedDraft
	requireNoError(t, fx.tool(t, "save_draft", string(input), &draft))
	if draft.UID == 0 {
		t.Fatal("missing draft UID")
	}

	return draft
}

func TestReadMarkMoveAndSearch(t *testing.T) {
	fx := newFixture(t)
	uid := fx.append(t, inbox, htmlEmail)
	var read struct {
		mail.Message

		Note string `json:"note"`
	}
	requireNoError(
		t,
		fx.tool(t, "read_message", fmt.Sprintf(`{"folder":"INBOX","uid":%d}`, uid), &read),
	)
	assertReadPreview(t, read.Body, read.Note, read.Truncated, fx.service.MaxBody)

	var page mail.Page
	requireNoError(t, fx.tool(t, "list_messages", `{"folder":"INBOX","unread_only":true}`, &page))
	if len(page.Messages) != 1 || page.Messages[0].UID != uid || page.Messages[0].Read {
		t.Fatal("PEEK changed the flag or unread query failed")
	}
	requireNoError(
		t,
		fx.tool(
			t,
			"mark_messages",
			fmt.Sprintf(`{"folder":"INBOX","uids":[%d],"read":true}`, uid),
			nil,
		),
	)
	requireNoError(
		t,
		fx.tool(
			t,
			"search_messages",
			`{"folder":"INBOX","query":{"subject":"Example HTML"}}`,
			&page,
		),
	)
	if len(page.Messages) != 1 || !page.Messages[0].Read {
		t.Fatal("mark or search did not work")
	}
	requireNoError(
		t,
		fx.tool(
			t,
			"move_messages",
			fmt.Sprintf(`{"folder":"INBOX","uids":[%d],"to_folder":%q}`, uid, trash),
			nil,
		),
	)
	requireNoError(t, fx.tool(t, "list_messages", `{"folder":"INBOX"}`, &page))
	if len(page.Messages) != 0 {
		t.Fatal("source still contains moved message")
	}
	requireNoError(t, fx.tool(t, "list_messages", `{"folder":"Deleted Items"}`, &page))
	if len(page.Messages) != 1 || page.Messages[0].Subject != "Example HTML" {
		t.Fatal("destination is missing the moved message")
	}
}

func assertReadPreview(t *testing.T, wrapped, note string, truncated bool, maxBody int) {
	t.Helper()
	_, body, _ := strings.Cut(wrapped, ">")
	body = strings.TrimSuffix(body, "</untrusted_email>")

	if !strings.HasPrefix(wrapped, "<untrusted_email from=") || strings.Contains(body, "HIDDEN") ||
		!strings.Contains(body, "Hello") || !strings.Contains(body, "Visible") ||
		len(body) > maxBody ||
		!truncated ||
		note == "" {
		t.Fatalf("unexpected sanitized body %q, truncated %v", body, truncated)
	}
}

func TestDraftReplacement(t *testing.T) {
	fx := newFixture(t)
	external := fx.append(t, drafts, htmlEmail, imap.FlagDraft, imap.FlagDeleted)
	first := testDraft(t, fx, "First draft")
	input := `{"to":["recipient@example.com"],"subject":"Replacement","body_text":"example","replace_draft_uid":%d}`
	err := fx.tool(t, "save_draft", fmt.Sprintf(input, external), nil)
	if err == nil {
		t.Fatal("accepted removal of an external draft")
	}
	var second actions.SavedDraft
	requireNoError(t, fx.tool(t, "save_draft", fmt.Sprintf(input, first.UID), &second))
	if second.UID == first.UID || second.Detail != "" {
		t.Fatalf("replacement failed: %+v", second)
	}
	fx.assertMissing(t, drafts, first.UID)
	_, raw, err := fx.service.Bridge.Raw(t.Context(), drafts, external)
	requireNoError(t, err)
	if string(raw) != htmlEmail {
		t.Fatal("untargeted pre-deleted message was changed")
	}
}

func (fx *fixture) requestSend(t *testing.T, uid uint32) actions.Action {
	t.Helper()
	var action actions.Action
	requireNoError(t, fx.tool(t, actions.SendDraft, fmt.Sprintf(`{"draft_uid":%d}`, uid), &action))
	if action.Status != actions.Pending || action.ID == "" {
		t.Fatalf("request returned %+v", action)
	}

	return action
}

func (fx *fixture) approve(t *testing.T, id string, cookie *http.Cookie) {
	t.Helper()
	form, _ := fx.form(t, id, cookie)
	form.Set("decision", "approve")
	expectCode(t, fx.http(t, http.MethodPost, "/approve/"+id, cookie, form), http.StatusSeeOther)
	fx.assertStatus(t, id, actions.Queued)
}

func (fx *fixture) assertStatus(t *testing.T, id, want string) {
	t.Helper()
	var action actions.Action
	requireNoError(t, fx.tool(t, "get_action", fmt.Sprintf(`{"action_id":%q}`, id), &action))
	if action.Status != want {
		t.Fatalf("status = %s, want %s", action.Status, want)
	}
}

func (fx *fixture) assertMissing(t *testing.T, folder string, uid uint32) {
	t.Helper()
	_, _, err := fx.service.Bridge.Raw(t.Context(), folder, uid)
	if !errors.Is(err, mail.ErrNotFound) {
		t.Fatalf("message %s/%d is still available: %v", folder, uid, err)
	}
}

func (fx *fixture) due(t *testing.T, id string) {
	t.Helper()
	_, err := fx.service.DB.ExecContext(t.Context(), "UPDATE actions SET run_at=? WHERE id=?",
		time.Now().Add(-time.Second).UnixNano(), id)
	requireNoError(t, err)
}

func (fx *fixture) tick(t *testing.T) {
	t.Helper()
	requireNoError(t, fx.service.Tick(t.Context()))
}

func (fx *fixture) assertNoDelivery(t *testing.T) {
	t.Helper()
	select {
	case <-fx.delivered:
		t.Fatal("unexpected SMTP delivery")
	default:
	}
}

func TestSendDraftCancelAndDeliver(t *testing.T) {
	fx := newFixture(t)
	draft := testDraft(t, fx, "Approve me")
	action := fx.requestSend(t, draft.UID)
	approvalPath := "/approve/" + action.ID
	expectCode(t, fx.http(t, http.MethodGet, approvalPath, nil, nil), http.StatusSeeOther)
	cookie := fx.ownerCookie(t, approvalPath)
	_, page := fx.form(t, action.ID, cookie)
	if !strings.Contains(page, recipient) || !strings.Contains(page, secretBody) {
		t.Fatal("approval page omitted the snapshotted body or recipient")
	}

	fx.tick(t)
	fx.approve(t, action.ID, cookie)
	fx.tick(t)
	queued, err := fx.service.Get(t.Context(), action.ID)
	requireNoError(t, err)
	if queued.RunAt == nil || time.Until(*queued.RunAt) < fx.service.Delay-time.Second {
		t.Fatal("approval did not schedule a full delay")
	}
	fx.assertNoDelivery(t)
	form, _ := fx.form(t, action.ID, cookie)
	expectCode(
		t,
		fx.http(t, http.MethodPost, "/queued/"+action.ID+"/cancel", cookie, form),
		http.StatusSeeOther,
	)
	fx.assertStatus(t, action.ID, actions.Cancelled)
	_, before, err := fx.service.Bridge.Raw(t.Context(), drafts, draft.UID)
	requireNoError(t, err)

	action = fx.requestSend(t, draft.UID)
	fx.tick(t)
	fx.approve(t, action.ID, cookie)
	fx.tick(t)
	fx.due(t, action.ID)
	fx.tick(t)
	fx.assertStatus(t, action.ID, actions.Sent)
	select {
	case raw := <-fx.delivered:
		if !bytes.Equal(raw, before) {
			t.Fatal("SMTP DATA differs from the snapshotted RFC822 bytes")
		}
	default:
		t.Fatal("SMTP did not receive the message")
	}
	fx.assertMissing(t, drafts, draft.UID)
	fx.tick(t)
	fx.assertNoDelivery(t)

	list, err := fx.service.Bridge.List(t.Context(), sent, 0, 0, false, mail.Query{})
	requireNoError(t, err)
	if len(list.Messages) != 0 {
		t.Fatal("service appended a Sent copy with the default flag")
	}
	fx.assertNotifications(
		t,
		"Approval requested",
		"Approved, sending at",
		"Approval requested",
		"Approved, sending at",
		"Sent.",
	)
	var count int
	requireNoError(t, fx.service.DB.GetContext(t.Context(), &count,
		"SELECT COUNT(*) FROM audit WHERE event LIKE ?", "%"+secretBody+"%"))
	if count != 0 {
		t.Fatal("audit contains a message body")
	}
}

func (fx *fixture) assertNotifications(t *testing.T, prefixes ...string) {
	t.Helper()
	for _, prefix := range prefixes {
		select {
		case text := <-fx.notifications:
			if !strings.HasPrefix(text, prefix) || strings.Contains(text, secretBody) {
				t.Fatalf("unexpected notification %q, wanted %q", text, prefix)
			}
		default:
			t.Fatalf("missing notification %q", prefix)
		}
	}
}

func TestRejectCSRFAndWrongSubject(t *testing.T) {
	fx := newFixture(t)
	draft := testDraft(t, fx, "Reject me")
	action := fx.requestSend(t, draft.UID)
	path := "/approve/" + action.ID
	expectCode(t, fx.login(t, "another-example-owner", path), http.StatusForbidden)
	cookie := fx.ownerCookie(t, path)
	form, _ := fx.form(t, action.ID, cookie)
	expectCode(t, fx.http(t, http.MethodPost, path, nil, form), http.StatusUnauthorized)
	expectCode(t, fx.http(
		t,
		http.MethodPost,
		path,
		cookie,
		url.Values{
			"decision": {"approve"},
			"snapshot": {form.Get("snapshot")},
		},
	), http.StatusForbidden)
	fx.assertStatus(t, action.ID, actions.Pending)
	form.Set("decision", "reject")
	expectCode(t, fx.http(t, http.MethodPost, path, cookie, form), http.StatusSeeOther)
	fx.assertStatus(t, action.ID, actions.Rejected)
	fx.assertNoDelivery(t)
}

func TestRestartStallsOverdueAndNeverReplaysClaimed(t *testing.T) {
	fx := newFixture(t)
	draft := testDraft(t, fx, "Wait for me")
	action := fx.requestSend(t, draft.UID)
	other := fx.requestSend(t, draft.UID)
	future := fx.requestSend(t, draft.UID)
	cookie := fx.ownerCookie(t, "")
	fx.approve(t, action.ID, cookie)
	fx.approve(t, other.ID, cookie)
	fx.approve(t, future.ID, cookie)
	fx.due(t, action.ID)
	_, err := fx.service.DB.ExecContext(
		t.Context(),
		"UPDATE actions SET claimed=1 WHERE id=?",
		other.ID,
	)
	requireNoError(t, err)
	fx.reopen(t)
	fx.assertStatus(t, action.ID, actions.Stalled)
	fx.assertStatus(t, other.ID, actions.Failed)
	fx.assertStatus(t, future.ID, actions.Queued)
	for range 8 {
		fx.tick(t)
	}
	fx.assertNotifications(t, "Approval requested", "Approval requested", "Approval requested",
		"Approved, sending at", "Approved, sending at", "Approved, sending at",
		"Action failed", "Action stalled")
	fx.assertNoDelivery(t)
	fx.approve(t, action.ID, cookie)
	queued, err := fx.service.Get(t.Context(), action.ID)
	requireNoError(t, err)
	if queued.RunAt == nil || time.Until(*queued.RunAt) < fx.service.Delay-time.Second {
		t.Fatal("re-approval did not start a fresh delay")
	}
}

func TestSnapshotTamperExpiryAndBcc(t *testing.T) {
	fx := newFixture(t)
	draft := testDraft(t, fx, "Inspect me")
	action := fx.requestSend(t, draft.UID)
	expiring := fx.requestSend(t, draft.UID)
	cookie := fx.ownerCookie(t, "")
	fx.approve(t, action.ID, cookie)
	_, err := fx.service.DB.ExecContext(t.Context(), "UPDATE actions SET snapshot=? WHERE id=?",
		[]byte(`{"location":{"folder":"Drafts"}}`), action.ID)
	requireNoError(t, err)
	fx.due(t, action.ID)
	_, err = fx.service.DB.ExecContext(
		t.Context(),
		"UPDATE actions SET expires_at=0 WHERE id=?",
		expiring.ID,
	)
	requireNoError(t, err)
	fx.tick(t)
	fx.assertStatus(t, action.ID, actions.Failed)
	fx.assertStatus(t, expiring.ID, actions.Expired)
	fx.assertNoDelivery(t)

	bcc := fx.append(t, drafts, "Bcc: hidden@example.com\r\n"+htmlEmail, imap.FlagDraft)
	err = fx.tool(t, actions.SendDraft, fmt.Sprintf(`{"draft_uid":%d}`, bcc), nil)
	if err == nil {
		t.Fatal("accepted a draft with Bcc")
	}
}

func TestForwardAndTrashDelete(t *testing.T) {
	fx := newFixture(t)
	uid := fx.append(t, inbox, multipartEmail)
	args := fmt.Sprintf(`{"folder":"INBOX","uids":[%d]}`, uid)
	if fx.tool(t, actions.Delete, args, nil) == nil {
		t.Fatal("accepted permanent deletion from INBOX")
	}
	wrongTrash := fx.append(t, "Trash", multipartEmail)
	if fx.tool(
		t,
		actions.Delete,
		fmt.Sprintf(`{"folder":"Trash","uids":[%d]}`, wrongTrash),
		nil,
	) == nil {
		t.Fatal("accepted a name fallback despite an authoritative SPECIAL-USE match")
	}
	var msg mail.Message
	requireNoError(
		t,
		fx.tool(t, "read_message", fmt.Sprintf(`{"folder":"INBOX","uid":%d}`, uid), &msg),
	)
	if len(msg.Attachments) != 1 || msg.Attachments[0].Name != "example.bin" ||
		msg.Attachments[0].Size != 3 {
		t.Fatalf("unexpected attachment metadata: %+v", msg.Attachments)
	}
	var action actions.Action
	requireNoError(t, fx.tool(
		t,
		actions.Forward,
		fmt.Sprintf(
			`{"folder":"INBOX","uid":%d,"to":["recipient@example.com"],"note_text":"Read this."}`,
			uid,
		),
		&action,
	))
	cookie := fx.ownerCookie(t, "")
	fx.approve(t, action.ID, cookie)
	fx.due(t, action.ID)
	fx.tick(t)
	fx.assertStatus(t, action.ID, actions.Sent)
	select {
	case raw := <-fx.delivered:
		msg, err := mail.Inspect(raw, 0)
		requireNoError(t, err)
		if len(msg.Attachments) != 0 || !strings.Contains(msg.Body, secretBody) ||
			!strings.Contains(
				msg.Body,
				"Read this.",
			) || bytes.Contains(raw, []byte("example.bin")) {
			t.Fatal("forward did not contain only the note and original body")
		}
	default:
		t.Fatal("forward did not reach SMTP")
	}

	for _, folder := range []string{trash, spam} {
		uid := fx.append(t, folder, multipartEmail)
		other := fx.append(t, folder, multipartEmail, imap.FlagDeleted)
		requireNoError(
			t,
			fx.tool(
				t,
				actions.Delete,
				fmt.Sprintf(`{"folder":%q,"uids":[%d]}`, folder, uid),
				&action,
			),
		)
		fx.approve(t, action.ID, cookie)
		fx.due(t, action.ID)
		fx.tick(t)
		fx.assertStatus(t, action.ID, actions.Done)
		fx.assertMissing(t, folder, uid)
		_, _, err := fx.service.Bridge.Raw(t.Context(), folder, other)
		requireNoError(t, err)
	}
}

func TestHealthAndMCPChallenge(t *testing.T) {
	fx := newFixture(t)
	health := fx.http(t, http.MethodGet, "/healthz", nil, nil)
	expectCode(t, health, http.StatusOK)
	if health.Body.String() != "{\"db\":true,\"imap\":true,\"smtp\":true}\n" {
		t.Fatalf("unexpected health data %s", health.Body)
	}
	bad := *fx.service.Bridge
	bad.Password = "incorrect-example-credential"
	fx.service.Bridge = &bad
	expectCode(t, fx.http(t, http.MethodGet, "/healthz", nil, nil), http.StatusOK)
	fx.reopen(t)
	badHealth := fx.http(t, http.MethodGet, "/healthz", nil, nil)
	expectCode(t, badHealth, http.StatusServiceUnavailable)
	probe := new(healthResult)
	requireNoError(t, json.Unmarshal(badHealth.Body.Bytes(), probe))

	if !probe.DB || probe.IMAP || probe.SMTP {
		t.Fatalf("failed authentication was not reflected: %+v", probe)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+fx.bearer(t, "another-example-owner"))
	response := httptest.NewRecorder()
	fx.handler.ServeHTTP(response, req)
	expectCode(t, response, http.StatusUnauthorized)
	missing := fx.http(t, http.MethodPost, "/mcp", nil, nil)
	expectCode(t, missing, http.StatusUnauthorized)
	if !strings.Contains(missing.Header().Get("WWW-Authenticate"),
		publicURL+"/.well-known/oauth-protected-resource") {
		t.Fatal("missing OAuth discovery challenge")
	}
	metadata := fx.http(t, http.MethodGet, "/.well-known/oauth-protected-resource", nil, nil)
	expectCode(t, metadata, http.StatusOK)
	if !strings.Contains(metadata.Body.String(), fx.provider.Issuer()) {
		t.Fatal("protected resource metadata omitted OIDC issuer")
	}
}

func TestExecutionClaimIsExclusive(t *testing.T) {
	fx := newFixture(t)
	draft := testDraft(t, fx, "One SMTP acceptance")
	action := fx.requestSend(t, draft.UID)
	fx.approve(t, action.ID, fx.ownerCookie(t, ""))
	fx.due(t, action.ID)
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			err := fx.service.Tick(t.Context())
			if err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	fx.assertStatus(t, action.ID, actions.Sent)
	if len(fx.delivered) != 1 {
		t.Fatalf("SMTP delivery count = %d, want 1", len(fx.delivered))
	}
}

func TestSMTPRejectionLeavesDraft(t *testing.T) {
	fx := newFixture(t)
	fx.rejectSMTP.Store(true)
	draft := testDraft(t, fx, "Reject SMTP DATA")
	action := fx.requestSend(t, draft.UID)
	fx.tick(t)
	fx.approve(t, action.ID, fx.ownerCookie(t, ""))
	fx.tick(t)
	fx.due(t, action.ID)
	fx.tick(t)
	fx.assertStatus(t, action.ID, actions.Failed)
	fx.assertNoDelivery(t)
	_, _, err := fx.service.Bridge.Raw(t.Context(), drafts, draft.UID)
	requireNoError(t, err)
	fx.assertNotifications(t, "Approval requested", "Approved, sending at", "Action failed")
}

func TestPermanentDeleteChecksUIDValidity(t *testing.T) {
	fx := newFixture(t)
	original := fx.append(t, spam, multipartEmail)
	var action actions.Action
	requireNoError(t, fx.tool(t, actions.Delete,
		fmt.Sprintf(`{"folder":"Spam","uids":[%d]}`, original), &action))
	fx.approve(t, action.ID, fx.ownerCookie(t, ""))
	requireNoError(t, fx.user.Delete(spam))
	requireNoError(t, fx.user.Create(spam, nil))
	replacement := fx.append(t, spam, htmlEmail)
	if replacement != original {
		t.Fatal("fixture did not reuse UID")
	}
	fx.due(t, action.ID)
	fx.tick(t)
	fx.assertStatus(t, action.ID, actions.Failed)
	_, raw, err := fx.service.Bridge.Raw(t.Context(), spam, replacement)
	requireNoError(t, err)
	if string(raw) != htmlEmail {
		t.Fatal("replacement message was changed")
	}
}

func TestLivezChecksOnlySQLite(t *testing.T) {
	fx := newFixture(t)
	fx.service.Bridge = nil
	resp := fx.http(t, http.MethodGet, "/livez", nil, nil)
	expectCode(t, resp, http.StatusOK)
	if resp.Body.String() != "{\"db\":true}\n" {
		t.Fatalf("unexpected live response %s", resp.Body)
	}
	requireNoError(t, fx.service.DB.Close())
	expectCode(t, fx.http(t, http.MethodGet, "/livez", nil, nil), http.StatusServiceUnavailable)
}

func TestHealthCoalescesConcurrentProbes(t *testing.T) {
	fx := newFixture(t)
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
			resp := httptest.NewRecorder()
			fx.handler.ServeHTTP(resp, req)
			if resp.Code != http.StatusOK {
				t.Errorf("health returned %d: %s", resp.Code, resp.Body)
			}
		})
	}
	workers.Wait()
	if imapCount, smtpCount := fx.imapProbes.Load(), fx.smtpProbes.Load(); imapCount != 1 ||
		smtpCount != 1 {
		t.Fatalf("health connected %d times to IMAP and %d times to SMTP", imapCount, smtpCount)
	}
}
