package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zekihan/proton-mcp/internal/actions"
)

func TestCancelAfterClaimBeforeSMTP(t *testing.T) {
	fx := newFixture(t)
	draft := testDraft(t, fx, "Cancel during authentication")
	action := fx.requestSend(t, draft.UID)
	cookie := fx.ownerCookie(t, "")
	fx.approve(t, action.ID, cookie)
	form, _ := fx.form(t, action.ID, cookie)
	fx.due(t, action.ID)

	gate := newCallGate(t)
	fx.smtpAuthGate.Store(gate)
	finish := fx.startTick(t)
	gate.wait(t)
	var claimed bool
	requireNoError(
		t,
		fx.service.DB.GetContext(
			t.Context(),
			&claimed,
			"SELECT claimed FROM actions WHERE id=?",
			action.ID,
		),
	)
	if !claimed {
		t.Fatal("scheduler has not claimed the action")
	}
	expectCode(
		t,
		fx.http(t, http.MethodPost, "/queued/"+action.ID+"/cancel", cookie, form),
		http.StatusSeeOther,
	)
	gate.open()
	requireNoError(t, <-finish)
	fx.assertStatus(t, action.ID, actions.Cancelled)
	fx.assertNoDelivery(t)
	_, _, err := fx.service.Bridge.Raw(t.Context(), drafts, draft.UID)
	requireNoError(t, err)
}

func TestCancelReportsTooLateOnceSendingStarts(t *testing.T) {
	fx := newFixture(t)
	draft := testDraft(t, fx, "Cancel after SMTP MAIL")
	action := fx.requestSend(t, draft.UID)
	cookie := fx.ownerCookie(t, "")
	fx.approve(t, action.ID, cookie)
	form, _ := fx.form(t, action.ID, cookie)
	fx.due(t, action.ID)
	gate := newCallGate(t)
	fx.smtpMailGate.Store(gate)
	finish := fx.startTick(t)
	gate.wait(t)

	resp := fx.http(t, http.MethodPost, "/queued/"+action.ID+"/cancel", cookie, form)
	expectCode(t, resp, http.StatusConflict)
	if !strings.Contains(resp.Body.String(), "too late to cancel: sending has started") {
		t.Fatalf("unexpected cancellation response %s", resp.Body)
	}
	err := fx.tool(t, "cancel_send", fmt.Sprintf(`{"action_id":%q}`, action.ID), nil)
	if err == nil || !strings.Contains(err.Error(), "sending has started") {
		t.Fatalf("unexpected MCP cancellation result %v", err)
	}
	var requested bool
	requireNoError(
		t,
		fx.service.DB.GetContext(
			t.Context(),
			&requested,
			"SELECT cancel_requested FROM actions WHERE id=?",
			action.ID,
		),
	)
	if !requested {
		t.Fatal("late cancel was not recorded")
	}

	gate.open()
	requireNoError(t, <-finish)
	fx.assertStatus(t, action.ID, actions.Sent)
	if len(fx.delivered) != 1 {
		t.Fatal("SMTP did not receive exactly one message")
	}
}

func (fx *fixture) startTick(t *testing.T) <-chan error {
	t.Helper()
	result := make(chan error, 1)
	go func() { result <- fx.service.Tick(t.Context()) }()

	return result
}

func TestUIDLimitAcrossMCPTools(t *testing.T) {
	fx := newFixture(t)
	uids := strings.Repeat("1,", 100) + "1"
	for tool, extra := range map[string]string{
		"mark_messages":      `,"read":true`,
		"move_messages":      `,"to_folder":"INBOX"`,
		"delete_permanently": "",
	} {
		err := fx.tool(t, tool, fmt.Sprintf(`{"folder":"Spam","uids":[%s]%s}`, uids, extra), nil)
		if err == nil || !strings.Contains(err.Error(), "at most 100") {
			t.Errorf("%s did not report the UID limit: %v", tool, err)
		}
	}
	if fx.imapProbes.Load() != 0 {
		t.Fatal("oversized requests reached IMAP")
	}
}

func TestSessionRenewalRotatesTokenAndCSRF(t *testing.T) {
	fx := newFixture(t)
	draft := testDraft(t, fx, "Review after renewal")
	action := fx.requestSend(t, draft.UID)
	path := "/approve/" + action.ID
	old := fx.ownerCookie(t, path)
	_, err := fx.service.DB.ExecContext(t.Context(), "UPDATE sessions SET renewed_at='2000'")
	requireNoError(t, err)

	page := fx.http(t, http.MethodGet, path, old, nil)
	expectCode(t, page, http.StatusOK)
	cookies := page.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value == old.Value {
		t.Fatal("renewal did not issue a replacement token")
	}
	rotated := cookies[0]
	expectCode(t, fx.http(t, http.MethodGet, path, old, nil), http.StatusSeeOther)

	form := url.Values{"decision": {"approve"}}
	for _, match := range formField.FindAllStringSubmatch(page.Body.String(), -1) {
		form.Set(match[1], match[2])
	}
	expectCode(t, fx.http(t, http.MethodPost, path, rotated, form), http.StatusSeeOther)
	fx.assertStatus(t, action.ID, actions.Queued)
}

func TestStalledApprovalGetsFreshDeadlineAndExpires(t *testing.T) {
	fx := newFixture(t)
	draft := testDraft(t, fx, "Stalled review")
	action := fx.requestSend(t, draft.UID)
	cookie := fx.ownerCookie(t, "")
	fx.approve(t, action.ID, cookie)
	fx.due(t, action.ID)
	_, err := fx.service.DB.ExecContext(
		t.Context(),
		"UPDATE actions SET expires_at=0, claimed=1 WHERE id=?",
		action.ID,
	)
	requireNoError(t, err)
	before := time.Now()
	fx.reopen(t)
	fx.assertStatus(t, action.ID, actions.Stalled)

	var expiry int64
	requireNoError(
		t,
		fx.service.DB.GetContext(
			t.Context(),
			&expiry,
			"SELECT expires_at FROM actions WHERE id=?",
			action.ID,
		),
	)
	if expiry < before.Add(fx.service.TTL).UnixNano() ||
		expiry > time.Now().Add(fx.service.TTL).UnixNano() {
		t.Fatal("stall did not reset the approval deadline")
	}
	form, _ := fx.form(t, action.ID, cookie)
	form.Set("decision", "approve")
	_, err = fx.service.DB.ExecContext(
		t.Context(),
		"UPDATE actions SET expires_at=0 WHERE id=?",
		action.ID,
	)
	requireNoError(t, err)
	fx.tick(t)
	fx.assertStatus(t, action.ID, actions.Expired)
	expectCode(
		t,
		fx.http(t, http.MethodPost, "/approve/"+action.ID, cookie, form),
		http.StatusConflict,
	)
	fx.assertNoDelivery(t)
}

func TestHealthWaiterCanLeaveSharedProbe(t *testing.T) {
	fx := newFixture(t)
	gate := newCallGate(t)
	fx.smtpAuthGate.Store(gate)
	probe := &health{svc: fx.service, ttl: healthTTL}
	first := make(chan healthResult, 1)
	go func() { first <- probe.probe(t.Context()) }()
	gate.wait(t)

	cancelled, stop := context.WithCancel(t.Context())
	stop()
	waiter := make(chan healthResult, 1)
	go func() { waiter <- probe.probe(cancelled) }()
	select {
	case result := <-waiter:
		if result != (healthResult{}) {
			t.Fatal("cancelled waiter received an unfinished result")
		}
	case <-time.After(time.Second):
		t.Fatal("waiter was blocked by the in-flight probe")
	}
	gate.open()
	select {
	case result := <-first:
		if !result.DB || !result.IMAP || !result.SMTP {
			t.Fatalf("shared check failed: %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shared probe did not finish")
	}
	if fx.smtpProbes.Load() != 1 || fx.imapProbes.Load() != 1 {
		t.Fatal("concurrent calls ran more than one set of Bridge probes")
	}
}

func TestHealthProbesShareTimeoutBudget(t *testing.T) {
	fx := newFixture(t)
	imapGate, smtpGate := newCallGate(t), newCallGate(t)
	fx.imapLoginGate.Store(imapGate)
	fx.smtpAuthGate.Store(smtpGate)
	probe := &health{svc: fx.service, ttl: healthTTL}
	finish := make(chan healthResult, 1)
	go func() { finish <- probe.probe(t.Context()) }()
	imapGate.wait(t)
	smtpGate.wait(t)
	select {
	case result := <-finish:
		if !result.DB || result.IMAP || result.SMTP {
			t.Fatalf("unexpected result from blocked Bridge logins: %+v", result)
		}
	case <-time.After(healthBudget):
		t.Fatal("health probe exceeded its shared timeout budget")
	}
}

func TestCancelAfterClaimBeforeIMAPDelete(t *testing.T) {
	fx := newFixture(t)
	uid := fx.append(t, spam, multipartEmail)
	var action actions.Action
	requireNoError(
		t,
		fx.tool(t, actions.Delete, fmt.Sprintf(`{"folder":"Spam","uids":[%d]}`, uid), &action),
	)
	cookie := fx.ownerCookie(t, "")
	fx.approve(t, action.ID, cookie)
	form, _ := fx.form(t, action.ID, cookie)
	fx.due(t, action.ID)

	gate := newCallGate(t)
	fx.imapLoginGate.Store(gate)
	finish := fx.startTick(t)
	gate.wait(t)
	expectCode(
		t,
		fx.http(t, http.MethodPost, "/queued/"+action.ID+"/cancel", cookie, form),
		http.StatusSeeOther,
	)
	gate.open()
	requireNoError(t, <-finish)
	fx.assertStatus(t, action.ID, actions.Cancelled)
	_, _, err := fx.service.Bridge.Raw(t.Context(), spam, uid)
	requireNoError(t, err)
}

func TestRecoveryRestoresLegacyStallDeadlineFromAudit(t *testing.T) {
	fx := newFixture(t)
	draft := testDraft(t, fx, "Legacy stall")
	action := fx.requestSend(t, draft.UID)
	fx.approve(t, action.ID, fx.ownerCookie(t, ""))
	fx.due(t, action.ID)
	fx.reopen(t)
	fx.assertStatus(t, action.ID, actions.Stalled)

	var stalledAt int64
	requireNoError(t, fx.service.DB.GetContext(
		t.Context(),
		&stalledAt,
		`SELECT at FROM audit
    WHERE json_extract(event,'$.id')=? AND json_extract(event,'$.status')=?
    AND json_extract(event,'$.action')='send_draft'`,
		action.ID,
		actions.Stalled,
	))
	_, err := fx.service.DB.ExecContext(
		t.Context(),
		"UPDATE actions SET expires_at=0 WHERE id=?",
		action.ID,
	)
	requireNoError(t, err)
	fx.reopen(t)
	var deadline int64
	requireNoError(
		t,
		fx.service.DB.GetContext(
			t.Context(),
			&deadline,
			"SELECT expires_at FROM actions WHERE id=?",
			action.ID,
		),
	)
	if deadline != stalledAt+fx.service.TTL.Nanoseconds() {
		t.Fatal("recovery reset an existing stall's original deadline")
	}
}
