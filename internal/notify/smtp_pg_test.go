// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify_test

import (
	"bytes"
	"context"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

func mailChannelJSON(f *fakeSMTP, extra string) string {
	return `{"id":"mail","type":"smtp","host":"127.0.0.1","port":` + strconv.Itoa(f.port()) + `,"from":"wardyn@example.com"` + extra + `}`
}

const ownerRoute = `[{"tiers":[{"after":"0s","channels":["mail"],"notify":["run_owner"]}]}]`

func (h *harness) addPerson(t *testing.T, principal, email string) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO people (principal, email, created_by) VALUES ($1, $2, 'admin')`, principal, email); err != nil {
		t.Fatal(err)
	}
}

// TestSMTPWorker_SecretAndSandboxTextAreAbsentFromTheMessage: a registered secret in the approval's
// requested_scope and reason, and the run's task text, never reach the mail; the owner's address does.
func TestSMTPWorker_SecretAndSandboxTextAreAbsentFromTheMessage(t *testing.T) {
	const secret = "ghp_PLANTEDSECRETVALUE0123456789abcdef"
	f := newFakeSMTP(t, true)
	h := routedHarness(t, mailChannelJSON(f, `,"username":"relay-user","password":"`+smtpPassword+`"`), ownerRoute)
	h.addPerson(t, "alice", "alice@example.com")
	runID := h.run(t)
	masks := secretmask.NewRegistry()
	masks.Add(runID, []byte(secret))
	a := h.raise(t, runID, `{"host":"evil.example.com","argv":"`+secret+`"}`, "approve me, key "+secret)
	if n, err := h.worker(f.trust, masks).Tick(context.Background()); err != nil || n != 1 {
		t.Fatalf("tick: %d, %v", n, err)
	}
	if r := h.row(t, a.ID); r.State != "sent" {
		t.Fatalf("row = %+v, want sent", r)
	}
	for _, leak := range []string{secret, "evil.example.com", "approve me", "do not leak this task title", smtpPassword} {
		if strings.Contains(f.data, leak) {
			t.Fatalf("message carries %q:\n%s", leak, f.data)
		}
	}
	if !strings.Contains(f.data, "\nTo: alice@example.com\n") || !strings.Contains(f.data, "\nSubject: [Wardyn] Approval waiting: Network access\n") {
		t.Fatalf("message:\n%s", f.data)
	}
}

// TestSMTPWorker_NoRecipientIsADeadRowWithNoSend: an owner with no email and no static address leaves
// nothing to send to: dead after one attempt, class no_recipient, the relay never contacted.
func TestSMTPWorker_NoRecipientIsADeadRowWithNoSend(t *testing.T) {
	f := newFakeSMTP(t, true)
	h := routedHarness(t, mailChannelJSON(f, ""), ownerRoute)
	a := h.raise(t, h.run(t), `{"host":"x.example"}`, "")
	if n, err := h.worker(f.trust, nil).Tick(context.Background()); err != nil || n != 1 {
		t.Fatalf("tick: %d, %v", n, err)
	}
	r := h.row(t, a.ID)
	if r.State != "dead" || r.LastError != "no_recipient" || r.Attempts != 1 {
		t.Fatalf("row = %+v, want dead/no_recipient after one attempt", r)
	}
	if got := f.seen(); len(got) != 0 {
		t.Fatalf("the relay was contacted: %v", got)
	}
	if evs := h.rec.byAction("approval.notify.failed"); len(evs) != 1 || !strings.Contains(string(evs[0].Data), `"no_recipient"`) {
		t.Fatalf("audit rows = %+v", evs)
	}
}

// TestSMTPWorker_AHostileStoredEmailIsSkipped: a people.email that carries a CR LF, a comma pair or a
// display name is never written to a header or the envelope, so the row ends as no_recipient; beside a
// valid static address the message goes to that address alone.
func TestSMTPWorker_AHostileStoredEmailIsSkipped(t *testing.T) {
	for _, email := range []string{
		"victim@example.com\r\nBcc: attacker@example.com",
		"a@example.com, b@example.com",
		"Alice <alice@example.com>",
	} {
		f := newFakeSMTP(t, true)
		h := routedHarness(t, mailChannelJSON(f, ""), ownerRoute)
		h.addPerson(t, "alice", email)
		a := h.raise(t, h.run(t), `{"host":"x.example"}`, "")
		if _, err := h.worker(f.trust, nil).Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		if r := h.row(t, a.ID); r.State != "dead" || r.LastError != "no_recipient" {
			t.Fatalf("%q: row = %+v, want dead/no_recipient", email, r)
		}
		if len(f.seen()) != 0 {
			t.Fatalf("%q: the relay was contacted: %v", email, f.seen())
		}
	}

	f := newFakeSMTP(t, true)
	h := routedHarness(t, mailChannelJSON(f, `,"to":["secops@example.com"]`), ownerRoute)
	h.addPerson(t, "alice", "victim@example.com\r\nBcc: attacker@example.com")
	h.raise(t, h.run(t), `{"host":"x.example"}`, "")
	if _, err := h.worker(f.trust, nil).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	head, _, _ := strings.Cut(f.data, "\n\n")
	if !strings.Contains(head, "\nTo: secops@example.com\n") || strings.Contains(f.data, "attacker") || strings.Contains(f.data, "victim") {
		t.Fatalf("message:\n%s", f.data)
	}
}

// TestSMTPWorker_NoRelayReplyTextReachesLastErrorTheAuditRowOrTheLog: a relay that refuses with
// text is stored as its code.
func TestSMTPWorker_NoRelayReplyTextReachesLastErrorTheAuditRowOrTheLog(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	f := newFakeSMTP(t, true)
	f.mailCode = "550 " + secretRelayText
	h := routedHarness(t, mailChannelJSON(f, `,"to":["secops@example.com"],"username":"relay-user","password":"`+smtpPassword+`"`), ownerRoute)
	a := h.raise(t, h.run(t), `{"host":"x.example"}`, "")
	if _, err := h.worker(f.trust, nil).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := h.row(t, a.ID)
	if r.State != "dead" || r.LastError != "smtp_reply:550" {
		t.Fatalf("row = %+v, want dead/smtp_reply:550", r)
	}
	evs := h.rec.byAction("approval.notify.failed")
	if len(evs) != 1 {
		t.Fatalf("%d audit rows, want 1", len(evs))
	}
	haystack := r.LastError + string(evs[0].Data) + logs.String()
	for _, leak := range []string{secretRelayText, smtpPassword, "127.0.0.1"} {
		if strings.Contains(haystack, leak) {
			t.Fatalf("%q leaked into last_error, the audit row or the log:\n%s", leak, haystack)
		}
	}
}

// TestSMTPWorker_AnUntrustedRelayLeavesAVerifyClassAndNoAuth: through the worker's default client (no
// extra roots) the relay's certificate does not verify.
func TestSMTPWorker_AnUntrustedRelayLeavesAVerifyClassAndNoAuth(t *testing.T) {
	f := newFakeSMTP(t, true)
	h := routedHarness(t, mailChannelJSON(f, `,"to":["secops@example.com"],"username":"relay-user","password":"`+smtpPassword+`"`), `[{"tiers":[{"after":"0s","channels":["mail"]}]}]`)
	a := h.raise(t, h.run(t), `{"host":"x.example"}`, "")
	if _, err := h.worker(nil, nil).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := h.row(t, a.ID); r.State != "pending" || r.LastError != "tls_verify" {
		t.Fatalf("row = %+v, want pending/tls_verify", r)
	}
	if f.saw("AUTH") || f.saw("DATA") {
		t.Fatalf("commands after the verify failure: %v", f.seen())
	}
}
