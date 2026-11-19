// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The tool-approval route (the gate Claude Code's permission tool calls) answers a call whose
// rule is approve_each by raising the same per-call approval the broker would, so the gated
// lane waits as long as it already does and the broker then spends that approval: one prompt.

// gatePost is one permission-tool call to the proxy's approval route. An empty input sends none.
func gatePost(t *testing.T, f *mcpFixture, tool, input string) *httptest.ResponseRecorder {
	t.Helper()
	payload := map[string]any{"tool": tool, "cmd": "summary"}
	if input != "" {
		payload["input"] = json.RawMessage(input)
	}
	body, err := json.Marshal(map[string]any{"kind": "tool_call", "payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	f.p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, routeApprovalsCreate, bytes.NewReader(body)))
	return rec
}

// gateAnswer is what the toolgate reads from the route: an id and state for a raised row, a
// state alone for a decision the proxy made.
func gateAnswer(t *testing.T, rec *httptest.ResponseRecorder) (id uuid.UUID, state string) {
	t.Helper()
	var out struct {
		ID    uuid.UUID `json:"id"`
		State string    `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("answer %d %q is not JSON: %v", rec.Code, rec.Body.String(), err)
	}
	return out.ID, out.State
}

func wantGateDecision(t *testing.T, rec *httptest.ResponseRecorder, state string) {
	t.Helper()
	if id, got := gateAnswer(t, rec); rec.Code != http.StatusOK || got != state || id != uuid.Nil {
		t.Fatalf("answer %d %q, want 200 and a bare %s", rec.Code, rec.Body.String(), state)
	}
}

// TestGateApproveEachRaisesCallScope: the gate raises one row whose scope is the call's typed
// scope (the broker's shape and key), answers its id as PENDING, and sends nothing.
func TestGateApproveEachRaisesCallScope(t *testing.T) {
	f, cp := newEachFixture(t)
	input := `{"id":"ENG-1","b":2}`
	rec := gatePost(t, f, "mcp__linear__delete_issue", input)
	if rec.Code != http.StatusCreated {
		t.Fatalf("answer %d %q, want 201", rec.Code, rec.Body.String())
	}
	id, state := gateAnswer(t, rec)
	rows := cp.snapshot()
	if len(rows) != 1 || id != rows[0].id || state != "PENDING" || f.up.hitCount() != 0 {
		t.Fatalf("answer id %s state %q, rows %d, upstream hits %d; want the one row's id, PENDING, nothing sent", id, state, len(rows), f.up.hitCount())
	}
	key, err := types.MCPCallKey("linear", "delete_issue", json.RawMessage(input), nil)
	if err != nil {
		t.Fatal(err)
	}
	s := rows[0].scope
	if s.Tool != "mcp__linear__delete_issue" || s.MCPServer != "linear" || s.MCPHost != "mcp.linear.test" || s.MCPTool != "" ||
		string(s.Arguments) != `{"b":2,"id":"ENG-1"}` || len(s.Extra) != 0 || s.CallSHA256 != key || s.HoldSeconds != 0 {
		t.Fatalf("scope %+v, want the broker's shape for delete_issue with canonical arguments, no hold window", s)
	}
	if d := findDecision(t, f.buf, ruleSourceApprovals); d.ApprovalID == nil || *d.ApprovalID != id {
		t.Fatalf("decision %+v, want the raised row's id", d)
	}
}

// TestGateApproveEachThenBrokerOnePrompt: Claude Code echoes the input to the MCP call, so the
// broker finds the gate's row, sees it approved and sends the call: one row, one forward. A
// second identical call has no approval left and asks again.
func TestGateApproveEachThenBrokerOnePrompt(t *testing.T) {
	f, cp := newEachFixture(t)
	gatePost(t, f, "mcp__linear__delete_issue", `{"id":"ENG-1"}`)
	// A gate call while the row waits joins it; once approved it answers the same id.
	id1, _ := gateAnswer(t, gatePost(t, f, "mcp__linear__delete_issue", `{"id":"ENG-1"}`))
	cp.decide(0, types.ApprovalApproved)
	id2, state := gateAnswer(t, gatePost(t, f, "mcp__linear__delete_issue", `{ "id" : "ENG-1" }`))
	if cp.count() != 1 || id1 != id2 || state != "PENDING" {
		t.Fatalf("rows %d, ids %s %s, state %q; want one row and its id each time", cp.count(), id1, id2, state)
	}
	rec := f.post(t, mcpToken("claude-code"), "linear", eachCall(1, "delete_issue", `{"id":"ENG-1"}`))
	if rec.Code != http.StatusOK || f.up.hitCount() != 1 || cp.count() != 1 {
		t.Fatalf("answer %d %q, upstream hits %d, rows %d; want one forward on the gate's row", rec.Code, rec.Body.String(), f.up.hitCount(), cp.count())
	}
	if d := findDecision(t, f.buf, ruleSourceMCPApproved); d.ApprovalID == nil || *d.ApprovalID != id1 {
		t.Fatalf("forward row %+v, want it to name the gate's approval", d)
	}
	wantText(t, f.post(t, mcpToken("claude-code"), "linear", eachCall(2, "delete_issue", `{"id":"ENG-1"}`)), "waiting for a person")
	if cp.count() != 2 || f.up.hitCount() != 1 {
		t.Fatalf("rows %d, upstream hits %d; want the spent approval to ask again and send nothing", cp.count(), f.up.hitCount())
	}
}

// TestGateApproveEachUnroutableDenied: with no attached remote server for the key nothing could
// ask with the arguments, so the gate refuses; a hold would be looser than the rule.
func TestGateApproveEachUnroutableDenied(t *testing.T) {
	f, cp := newEachFixture(t, types.ToolRule{Tool: "mcp__nowhere__drop", Effect: types.ToolApproveEach})
	wantGateDecision(t, gatePost(t, f, "mcp__nowhere__drop", `{}`), "DENIED")
	if cp.count() != 0 || cp.posts != 0 {
		t.Fatalf("rows %d, posts %d; want the control plane left alone", cp.count(), cp.posts)
	}
	if d := findDecision(t, f.buf, "policy:tool-approve-each-unroutable"); d.Decision != egress.Deny || d.Cause == "" {
		t.Fatalf("decision %+v, want a deny naming the tool", d)
	}
}

// TestGateApproveEachWithoutInputLeavesItToBroker: a gate that sent no usable input (an older
// toolgate, or arguments too large for it) cannot show the arguments, so it answers APPROVED
// without a row and the broker asks.
func TestGateApproveEachWithoutInputLeavesItToBroker(t *testing.T) {
	for name, input := range map[string]string{"absent": "", "null": "null", "array": "[1]", "string": `"x"`, "repeated key": `{"a":1,"a":2}`} {
		t.Run(name, func(t *testing.T) {
			f, cp := newEachFixture(t)
			wantGateDecision(t, gatePost(t, f, "mcp__linear__delete_issue", input), "APPROVED")
			if cp.count() != 0 {
				t.Fatalf("rows %d, want none from the gate", cp.count())
			}
			if d := findDecision(t, f.buf, "policy:tool-approve-each"); d.Decision != egress.Allow {
				t.Fatalf("decision %+v, want the gate's pass-on row", d)
			}
			wantText(t, f.post(t, mcpToken("claude-code"), "linear", eachCall(1, "delete_issue", `{"id":"ENG-1"}`)), "waiting for a person")
			if cp.count() != 1 {
				t.Fatalf("rows %d, want the broker to ask", cp.count())
			}
		})
	}
}

// TestGateApproveEachDeclineSticky: a declined call is answered DENIED at the gate for the rest
// of the run without a new row, the broker refuses it too, and other arguments still ask.
func TestGateApproveEachDeclineSticky(t *testing.T) {
	f, cp := newEachFixture(t)
	gatePost(t, f, "mcp__linear__delete_issue", `{"id":"ENG-1"}`)
	cp.decide(0, types.ApprovalDenied)
	wantGateDecision(t, gatePost(t, f, "mcp__linear__delete_issue", `{"id":"ENG-1"}`), "DENIED")
	if cp.count() != 1 {
		t.Fatalf("rows %d, want the declined call not to ask again", cp.count())
	}
	if d := findDecision(t, f.buf, "policy:tool-approve-each-declined"); d.Decision != egress.Deny || d.ApprovalID == nil {
		t.Fatalf("decision %+v, want a deny naming the declined row", d)
	}
	wantText(t, f.post(t, mcpToken("claude-code"), "linear", eachCall(1, "delete_issue", `{"id":"ENG-1"}`)), "identical retries are refused")
	if cp.count() != 1 || f.up.hitCount() != 0 {
		t.Fatalf("rows %d, upstream hits %d; want the broker to refuse without asking", cp.count(), f.up.hitCount())
	}
	if rec := gatePost(t, f, "mcp__linear__delete_issue", `{"id":"ENG-2"}`); rec.Code != http.StatusCreated || cp.count() != 2 {
		t.Fatalf("answer %d %q, rows %d; want other arguments to ask", rec.Code, rec.Body.String(), cp.count())
	}
}

// TestGateApproveEachExpiredAsksAgain: a row that closed undecided is forgotten, so the next
// identical gate call raises a new one.
func TestGateApproveEachExpiredAsksAgain(t *testing.T) {
	f, cp := newEachFixture(t)
	gatePost(t, f, "mcp__linear__delete_issue", `{"id":"ENG-1"}`)
	cp.decide(0, types.ApprovalExpired)
	id, _ := gateAnswer(t, gatePost(t, f, "mcp__linear__delete_issue", `{"id":"ENG-1"}`))
	rows := cp.snapshot()
	if len(rows) != 2 || id != rows[1].id {
		t.Fatalf("rows %d, answered id %s; want a second row and its id", len(rows), id)
	}
}

// TestGateApproveEachRaiseRefused: a raise the control plane refuses is never waved on.
func TestGateApproveEachRaiseRefused(t *testing.T) {
	f, cp := newEachFixture(t)
	cp.refuse = http.StatusTooManyRequests
	rec := gatePost(t, f, "mcp__linear__delete_issue", `{"id":"ENG-1"}`)
	if rec.Code == http.StatusCreated || strings.Contains(rec.Body.String(), "APPROVED") {
		t.Fatalf("answer %d %q, want a refusal", rec.Code, rec.Body.String())
	}
	if cp.count() != 0 {
		t.Fatalf("rows %d, want none", cp.count())
	}
}

// TestGateApproveEachTwoNamesNeedTheirOwnApproval: two raw tools that share a display name
// (delete.issue and delete_issue) are two calls. With only delete.issue known to the broker the
// gate resolves it, raises for the raw name and a delete_issue call needs its own approval.
func TestGateApproveEachTwoNamesNeedTheirOwnApproval(t *testing.T) {
	f, cp := newEachFixture(t)
	args := `{"id":"ENG-1"}`
	// The broker has seen delete.issue only; its call waits and its row is the gate's to reuse.
	wantText(t, f.post(t, mcpToken("claude-code"), "linear", eachCall(1, "delete.issue", args)), "waiting for a person")
	if cp.count() != 1 {
		t.Fatalf("rows %d, want the broker's row", cp.count())
	}
	id, state := gateAnswer(t, gatePost(t, f, "mcp__linear__delete_issue", args))
	rows := cp.snapshot()
	if len(rows) != 1 || id != rows[0].id || state != "PENDING" || rows[0].scope.MCPTool != "delete.issue" {
		t.Fatalf("rows %+v, answered %s %s; want the gate to resolve the one raw tool and join its row", rows, id, state)
	}
	cp.decide(0, types.ApprovalApproved)
	wantText(t, f.post(t, mcpToken("claude-code"), "linear", eachCall(2, "delete_issue", args)), "waiting for a person")
	if cp.count() != 2 || f.up.hitCount() != 0 {
		t.Fatalf("rows %d, upstream hits %d; want delete_issue to need its own approval", cp.count(), f.up.hitCount())
	}
	if rec := f.post(t, mcpToken("claude-code"), "linear", eachCall(3, "delete.issue", args)); rec.Code != http.StatusOK || f.up.hitCount() != 1 {
		t.Fatalf("answer %d %q, upstream hits %d; want delete.issue sent on its approval", rec.Code, rec.Body.String(), f.up.hitCount())
	}
	var name string
	_ = json.Unmarshal(upstreamParams(t, f.up)["name"], &name)
	if name != "delete.issue" {
		t.Fatalf("forwarded name %q, want delete.issue", name)
	}
}

// TestGateApproveEachAmbiguousNameLeavesItToBroker: once the broker has seen two raw tools with
// one display name, the gate cannot say which the call is, so it raises nothing and the broker
// asks for the raw tool that arrives.
func TestGateApproveEachAmbiguousNameLeavesItToBroker(t *testing.T) {
	f, cp := newEachFixture(t)
	args := `{"id":"ENG-1"}`
	wantText(t, f.post(t, mcpToken("claude-code"), "linear", eachCall(1, "delete.issue", args)), "waiting for a person")
	wantText(t, f.post(t, mcpToken("claude-code"), "linear", eachCall(2, "delete_issue", args)), "waiting for a person")
	wantGateDecision(t, gatePost(t, f, "mcp__linear__delete_issue", args), "APPROVED")
	if cp.count() != 2 {
		t.Fatalf("rows %d, want the gate to raise none of its own", cp.count())
	}
	if d := findDecision(t, f.buf, "policy:tool-approve-each"); d.Decision != egress.Allow {
		t.Fatalf("decision %+v, want the gate's pass-on row", d)
	}
}

// TestGateApproveEachDelayedRaiseCannotRespend: two gate calls raise the same call and the control
// plane folds them into one approval A. The first one's answer is held while a person approves A,
// the broker sends the call on A and leaves. When the held answer arrives, A is long spent: the
// next identical call needs a fresh approval, and nothing more is sent until a person gives it.
func TestGateApproveEachDelayedRaiseCannotRespend(t *testing.T) {
	f, cp := newEachFixture(t)
	args := `{"id":"ENG-1"}`
	cp.holdRaise, cp.raiseHeld = make(chan struct{}), make(chan struct{})
	release, held := cp.holdRaise, cp.raiseHeld
	var once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	t.Cleanup(letGo) // an early failure must not leave the control plane's handler blocked
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- gatePost(t, f, "mcp__linear__delete_issue", args) }()
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the first gate call never reached the control plane")
	}
	if rec := gatePost(t, f, "mcp__linear__delete_issue", args); rec.Code != http.StatusCreated || cp.count() != 1 {
		t.Fatalf("second gate call: answer %d %q, rows %d; want it folded into the one row", rec.Code, rec.Body.String(), cp.count())
	}
	cp.decide(0, types.ApprovalApproved)
	if rec := f.post(t, mcpToken("claude-code"), "linear", eachCall(1, "delete_issue", args)); rec.Code != http.StatusOK || f.up.hitCount() != 1 {
		t.Fatalf("answer %d %q, upstream hits %d; want the call sent once on A", rec.Code, rec.Body.String(), f.up.hitCount())
	}
	letGo()
	select {
	case rec := <-done:
		if rec.Code == http.StatusInternalServerError {
			t.Fatalf("held gate call: answer %d %q", rec.Code, rec.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the held gate call never finished")
	}
	wantText(t, f.post(t, mcpToken("claude-code"), "linear", eachCall(2, "delete_issue", args)), "waiting for a person")
	if cp.count() != 2 || f.up.hitCount() != 1 {
		t.Fatalf("rows %d, upstream hits %d; want a fresh approval asked for and still one send", cp.count(), f.up.hitCount())
	}
	cp.decide(1, types.ApprovalApproved)
	if rec := f.post(t, mcpToken("claude-code"), "linear", eachCall(3, "delete_issue", args)); rec.Code != http.StatusOK || f.up.hitCount() != 2 {
		t.Fatalf("answer %d %q, upstream hits %d; want the second send on the second approval", rec.Code, rec.Body.String(), f.up.hitCount())
	}
}
