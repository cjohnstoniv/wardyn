// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newSendOnceFixture is newOpFixture for a run whose approve_each calls are each sent once.
func newSendOnceFixture(t *testing.T) (*mcpFixture, *opCP) {
	t.Helper()
	f, cp := newOpFixture(t)
	f.p.mcp.setApproveEach(&MCPApproveEachConfig{SendOnce: true})
	return f, cp
}

// derivedKey is the key send once gives occurrence n of linear's delete_issue with args.
func derivedKey(t *testing.T, args string, n int) string {
	t.Helper()
	return fmt.Sprintf("call:%s:%d", opCallKey(t, args), n)
}

// earlierSend is a send of linear's delete_issue with args that another run's proxy recorded.
func earlierSend(t *testing.T, args string) opSend {
	t.Helper()
	return opSend{id: uuid.New(), sha: opCallKey(t, args), at: time.Now().UTC().Add(-time.Hour).Truncate(time.Second), answered: true}
}

// TestMCPSendOnceKeylessCallRaisesItsDerivedKey: with send once, a keyless call is raised under the key
// made from its hash and first occurrence, recorded before it is sent and stamped after, and the
// server gets exactly the call, with no key.
func TestMCPSendOnceKeylessCallRaisesItsDerivedKey(t *testing.T) {
	f, cp := newSendOnceFixture(t)
	cp.approve = true
	if rec := f.post(t, mcpToken("codex-cli"), "linear", eachCall(1, "delete_issue", `{"id":"ENG-1"}`)); rec.Code != http.StatusOK || f.up.hitCount() != 1 {
		t.Fatalf("answer %d %q, upstream hits %d; want one send", rec.Code, rec.Body.String(), f.up.hitCount())
	}
	key := derivedKey(t, `{"id":"ENG-1"}`, 1)
	rows, _ := cp.snapshot()
	if len(rows) != 1 || rows[0].scope.OperationKey != key || rows[0].scope.CallSHA256 != opCallKey(t, `{"id":"ENG-1"}`) {
		t.Fatalf("raised %+v; want one row raised under %s", rows, key)
	}
	if got, want := cp.records(), []opPost{{id: rows[0].id, answered: false, hitsAt: 0}, {id: rows[0].id, answered: true, hitsAt: 1}}; !slices.Equal(got, want) {
		t.Fatalf("send records %+v, want %+v: recorded before the send, stamped after", got, want)
	}
	f.up.mu.Lock()
	got := string(f.up.body)
	f.up.mu.Unlock()
	if want := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_issue","arguments":{"id":"ENG-1"}}}`; got != want {
		t.Fatalf("the server got\n %s\nwant\n %s", got, want)
	}
}

// TestMCPSendOnceRetryIsAnsweredOccurrenceByOccurrence: a new proxy of the same correlation ID (a retry
// in a new run) whose first such call another run sent is answered with that send, neither asked about
// nor sent; its next identical call is the second occurrence, never sent, so a person is asked.
func TestMCPSendOnceRetryIsAnsweredOccurrenceByOccurrence(t *testing.T) {
	f, cp := newSendOnceFixture(t)
	earlier := earlierSend(t, `{"id":"ENG-1"}`)
	cp.seed(derivedKey(t, `{"id":"ENG-1"}`, 1), earlier)
	wantSentText(t, f.post(t, mcpToken("codex-cli"), "linear", eachCall(1, "delete_issue", `{"id":"ENG-1"}`)), earlier)
	if d := wantDecision(t, f.buf, egress.Deny, ruleSourceMCPApprovalSent); d.ApprovalID == nil || *d.ApprovalID != earlier.id {
		t.Fatalf("sent row %+v, want it naming %s", d, earlier.id)
	}
	wantText(t, f.post(t, mcpToken("codex-cli"), "linear", eachCall(2, "delete_issue", `{ "id" : "ENG-1" }`)), "waiting for a person")
	rows, _ := cp.snapshot()
	if len(rows) != 1 || rows[0].scope.OperationKey != derivedKey(t, `{"id":"ENG-1"}`, 2) || f.up.hitCount() != 0 {
		t.Fatalf("rows %+v, upstream hits %d; want the second occurrence asked about and nothing sent", rows, f.up.hitCount())
	}
}

// TestMCPSendOnceRepeatInTheRunIsAskedAgain: in one proxy the same call made again after its send is the
// next occurrence, asked about and sent under an approval of its own, as without send once.
func TestMCPSendOnceRepeatInTheRunIsAskedAgain(t *testing.T) {
	f, cp := newSendOnceFixture(t)
	cp.approve = true
	for i := 1; i <= 2; i++ {
		if rec := f.post(t, mcpToken("codex-cli"), "linear", eachCall(i, "delete_issue", `{"id":"ENG-1"}`)); rec.Code != http.StatusOK || f.up.hitCount() != i {
			t.Fatalf("call %d: answer %d %q, upstream hits %d; want it sent", i, rec.Code, rec.Body.String(), f.up.hitCount())
		}
	}
	rows, _ := cp.snapshot()
	if len(rows) != 2 || rows[0].scope.OperationKey != derivedKey(t, `{"id":"ENG-1"}`, 1) || rows[1].scope.OperationKey != derivedKey(t, `{"id":"ENG-1"}`, 2) {
		t.Fatalf("rows %+v, want one per occurrence", rows)
	}
}

// TestMCPSendOnceKeepsTheCallersKey: a call carrying its caller's own operation key keeps it under send
// once.
func TestMCPSendOnceKeepsTheCallersKey(t *testing.T) {
	f, cp := newSendOnceFixture(t)
	wantText(t, f.post(t, mcpToken("codex-cli"), "linear", keyedCall(1, `{"id":"ENG-1"}`, `"step-1"`)), "waiting for a person")
	if rows, _ := cp.snapshot(); len(rows) != 1 || rows[0].scope.OperationKey != "step-1" {
		t.Fatalf("rows %+v, want one raised under the caller's key", rows)
	}
}

// TestGateSendOnceOnePromptAndARetryPassedOn: with send once, the permission tool's gate raises the call
// under the key the broker gives it, so the broker spends the gate's approval: one row, one recorded
// send. A call whose next occurrence another run already sent is passed to the broker with no row, and
// the broker answers it with that send.
func TestGateSendOnceOnePromptAndARetryPassedOn(t *testing.T) {
	f, cp := newSendOnceFixture(t)
	const args = `{"id":"ENG-1"}`
	gatePost(t, f, "mcp__linear__delete_issue", args)
	if rows, _ := cp.snapshot(); len(rows) != 1 || rows[0].scope.OperationKey != derivedKey(t, args, 1) {
		t.Fatalf("gate rows %+v, want one raised under %s", rows, derivedKey(t, args, 1))
	}
	cp.set(0, types.ApprovalApproved)
	rec := f.post(t, mcpToken("claude-code"), "linear", eachCall(1, "delete_issue", args))
	if rows, _ := cp.snapshot(); rec.Code != http.StatusOK || f.up.hitCount() != 1 || len(rows) != 1 || len(cp.records()) != 2 {
		t.Fatalf("answer %d %q, upstream hits %d, rows %d, records %+v; want one recorded send on the gate's row",
			rec.Code, rec.Body.String(), f.up.hitCount(), len(rows), cp.records())
	}
	earlier := earlierSend(t, args)
	cp.seed(derivedKey(t, args, 2), earlier)
	wantGateDecision(t, gatePost(t, f, "mcp__linear__delete_issue", args), "APPROVED")
	wantSentText(t, f.post(t, mcpToken("claude-code"), "linear", eachCall(2, "delete_issue", args)), earlier)
	if rows, _ := cp.snapshot(); len(rows) != 1 || f.up.hitCount() != 1 {
		t.Fatalf("rows %d, upstream hits %d; want no new row and no new send", len(rows), f.up.hitCount())
	}
}

// TestMCPSendOnceRefusesACallersKeyInTheMadeForm: a caller's own key that starts with "call:", the prefix
// of the keys send once makes, is refused unasked and unsent, with send once and without it, so no other
// call can hold the key a call is given; that call is then sent under its own made key.
func TestMCPSendOnceRefusesACallersKeyInTheMadeForm(t *testing.T) {
	made := `"` + derivedKey(t, `{"id":"ENG-1"}`, 1) + `"`
	for _, sendOnce := range []bool{false, true} {
		f, cp := newOpFixture(t)
		f.p.mcp.setApproveEach(&MCPApproveEachConfig{SendOnce: sendOnce})
		cp.approve = true
		for _, key := range []string{made, `"call:step-1"`} {
			if got := toolResultText(t, f.post(t, mcpToken("codex-cli"), "linear", keyedCall(1, `{"id":"ENG-2"}`, key))); got != mcpCallOperationKeyMadeText {
				t.Fatalf("send once %v, key %s: text %q, want the made-key rule", sendOnce, key, got)
			}
			if d := wantDecision(t, f.buf, egress.Deny, ruleSourceMCPApprovalRefused); d.Cause != mcpCallOperationKeyMadeText {
				t.Fatalf("send once %v, key %s: refused row cause %q", sendOnce, key, d.Cause)
			}
		}
		if cp.raises() != 0 || f.up.hitCount() != 0 {
			t.Fatalf("send once %v: raises %d, upstream hits %d; want nothing asked or sent", sendOnce, cp.raises(), f.up.hitCount())
		}
		if !sendOnce {
			continue
		}
		if rec := f.post(t, mcpToken("codex-cli"), "linear", eachCall(2, "delete_issue", `{"id":"ENG-1"}`)); rec.Code != http.StatusOK || f.up.hitCount() != 1 {
			t.Fatalf("the keyless call: answer %d %q, upstream hits %d; want it sent", rec.Code, rec.Body.String(), f.up.hitCount())
		}
		if rows, _ := cp.snapshot(); len(rows) != 1 || rows[0].scope.OperationKey != derivedKey(t, `{"id":"ENG-1"}`, 1) {
			t.Fatalf("rows %+v, want one raised under %s", rows, made)
		}
	}
}

// TestMCPSendOnceSentAnswerNamingNoApprovalIsNotTrusted: a 409 that says a call was already sent but names
// no approval is not read as a send: the call is not sent, nor said to have been, and the agent reads
// that Wardyn could not ask.
func TestMCPSendOnceSentAnswerNamingNoApprovalIsNotTrusted(t *testing.T) {
	f, cp := newSendOnceFixture(t)
	const args = `{"id":"ENG-1"}`
	cp.seed(derivedKey(t, args, 1), opSend{sha: opCallKey(t, args), at: time.Now().UTC().Truncate(time.Second), answered: true})
	if got := toolResultText(t, f.post(t, mcpToken("codex-cli"), "linear", eachCall(1, "delete_issue", args))); got != mcpCallNoAskText {
		t.Fatalf("text %q, want the could-not-ask text", got)
	}
	if d := wantDecision(t, f.buf, egress.Deny, ruleSourceMCPApprovalRefused); d.ApprovalID != nil || d.Cause != "the request for an approval could not be raised" {
		t.Fatalf("refused row %+v, want the could-not-raise cause and no approval", d)
	}
	if f.up.hitCount() != 0 || len(cp.records()) != 0 {
		t.Fatalf("upstream hits %d, records %+v; want nothing sent or recorded", f.up.hitCount(), cp.records())
	}
}
