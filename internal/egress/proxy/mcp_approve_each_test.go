// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// approveEachRow is one approval row the fake control plane holds.
type approveEachRow struct {
	id    uuid.UUID
	scope types.MCPCallScope
	state types.ApprovalState
	polls int
}

// eachCP is a control plane for approve_each: it checks every raised scope the way the real one
// does (strict decode, the key recomputed from the scope's own fields), folds a raise into the
// PENDING row with the same call_sha256, and answers polls from its rows.
type eachCP struct {
	srv *httptest.Server

	mu       sync.Mutex
	rows     []*approveEachRow
	rejoins  []uuid.UUID
	posts    int                 // raises and rejoins answered, of any outcome
	refuse   int                 // when set, every new raise is answered with this status
	newState types.ApprovalState // the state a new row starts in; PENDING when empty
	// approveOnPoll approves a PENDING row when it is polled for the n-th time (0 = never).
	approveOnPoll int
	// stall, when set, holds the first APPROVED answer to a poll until it is closed; stalled is
	// closed once that answer is held.
	stall, stalled chan struct{}
	// holdRaise, when set, holds the answer to the next raise that makes a new row until it is
	// closed; raiseHeld is closed once that answer is held. The row exists meanwhile.
	holdRaise, raiseHeld chan struct{}
}

func newEachCP(t *testing.T) *eachCP {
	t.Helper()
	c := &eachCP{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/internal/approvals" {
			defer func() { c.posts++ }()
			var body struct {
				Kind           types.ApprovalKind `json:"kind"`
				RequestedScope json.RawMessage    `json:"requested_scope"`
				RejoinID       *uuid.UUID         `json:"rejoin_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Kind != types.ApprovalToolCall {
				http.Error(w, "bad kind", http.StatusBadRequest)
				return
			}
			var scope types.MCPCallScope
			dec := json.NewDecoder(bytes.NewReader(body.RequestedScope))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&scope); err != nil {
				http.Error(w, "bad scope", http.StatusBadRequest)
				return
			}
			if key, err := scope.ComputeKey(); err != nil || key != scope.CallSHA256 {
				http.Error(w, "scope does not hash to its call_sha256", http.StatusBadRequest)
				return
			}
			if body.RejoinID != nil {
				c.rejoins = append(c.rejoins, *body.RejoinID)
				for _, row := range c.rows {
					if row.id == *body.RejoinID && row.state == types.ApprovalPending {
						_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: row.id, State: row.state})
						return
					}
				}
				http.Error(w, "no pending approval to rejoin", http.StatusNotFound)
				return
			}
			for _, row := range c.rows {
				if row.state == types.ApprovalPending && row.scope.CallSHA256 == scope.CallSHA256 {
					_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: row.id, State: row.state})
					return
				}
			}
			if c.refuse != 0 {
				http.Error(w, "refused", c.refuse)
				return
			}
			row := &approveEachRow{id: uuid.New(), scope: scope, state: types.ApprovalPending}
			if c.newState != "" {
				row.state = c.newState
			}
			c.rows = append(c.rows, row)
			if c.holdRaise != nil {
				release := c.holdRaise
				c.holdRaise = nil
				close(c.raiseHeld)
				c.mu.Unlock()
				<-release
				c.mu.Lock()
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: row.id, State: types.ApprovalPending})
			return
		}
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/internal/approvals/") {
			for _, row := range c.rows {
				if r.URL.Path == "/api/v1/internal/approvals/"+row.id.String() {
					row.polls++
					if c.approveOnPoll != 0 && row.polls >= c.approveOnPoll && row.state == types.ApprovalPending {
						row.state = types.ApprovalApproved
					}
					answer := types.ApprovalRequest{ID: row.id, State: row.state}
					if c.stall != nil && row.state == types.ApprovalApproved {
						release := c.stall
						c.stall = nil
						close(c.stalled)
						c.mu.Unlock()
						<-release
						c.mu.Lock()
					}
					_ = json.NewEncoder(w).Encode(answer)
					return
				}
			}
		}
		http.Error(w, "unexpected", http.StatusTeapot)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *eachCP) decide(i int, s types.ApprovalState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i < len(c.rows) { // a missing row is left for the test's own count check
		c.rows[i].state = s
	}
}

func (c *eachCP) snapshot() []approveEachRow {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]approveEachRow, len(c.rows))
	for i, row := range c.rows {
		out[i] = *row
	}
	return out
}

func (c *eachCP) count() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.rows) }

// wire points p at the fake control plane and makes a window second and a poll short.
func (c *eachCP) wire(t *testing.T, p *Proxy) {
	t.Helper()
	p.approval = newApprovalClient(c.srv.URL, newTokenSource("tok"), p.runID, c.srv.Client())
	prevPoll, prevSecond := holdPollInterval, mcpApproveEachSecond
	holdPollInterval, mcpApproveEachSecond = 2*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { holdPollInterval, mcpApproveEachSecond = prevPoll, prevSecond })
}

// newEachFixture is the broker fixture with rules and a fake control plane wired in.
func newEachFixture(t *testing.T, rules ...types.ToolRule) (*mcpFixture, *eachCP) {
	t.Helper()
	if len(rules) == 0 {
		rules = []types.ToolRule{{Tool: "mcp__linear__delete_issue", Effect: types.ToolApproveEach}}
	}
	f := newMCPFixture(t, types.RunPolicySpec{ToolRules: rules}, nil, nil)
	f.p.mcp.setApproveEach(&MCPApproveEachConfig{})
	cp := newEachCP(t)
	cp.wire(t, f.p)
	return f, cp
}

// eachCall is a tools/call body with id, raw tool name and arguments given as JSON text.
func eachCall(id int, tool, args string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, id, tool, args)
}

// toolResultText reads an isError tool result; it fails the test on anything else.
func toolResultText(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var resp struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !resp.Result.IsError || len(resp.Result.Content) != 1 || resp.Result.Content[0].Type != "text" {
		t.Fatalf("answer %d %q, want one isError text result", rec.Code, rec.Body.String())
	}
	return resp.Result.Content[0].Text
}

func wantText(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if got := toolResultText(t, rec); !strings.Contains(got, want) {
		t.Fatalf("text %q, want it to contain %q", got, want)
	}
}

// upstreamParams is the params object of the last body the MCP server got.
func upstreamParams(t *testing.T, u *mcpUpstream) map[string]json.RawMessage {
	t.Helper()
	u.mu.Lock()
	body := append([]byte(nil), u.body...)
	u.mu.Unlock()
	var msg map[string]json.RawMessage
	var params map[string]json.RawMessage
	if err := json.Unmarshal(body, &msg); err != nil || json.Unmarshal(msg["params"], &params) != nil {
		t.Fatalf("upstream body %q is not a tools/call", body)
	}
	return params
}

// waitEachRows waits until the fake control plane holds n rows.
func waitEachRows(t *testing.T, cp *eachCP, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for cp.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("control plane has %d rows, want %d", cp.count(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestMCPApproveEachHoldsAndForwards: the call waits on the open request, is forwarded once a
// person approves it, and the forward's row names the approval.
func TestMCPApproveEachHoldsAndForwards(t *testing.T) {
	f, cp := newEachFixture(t)
	cp.approveOnPoll = 2
	rec := f.post(t, mcpToken("codex-cli"), "linear", eachCall(1, "delete_issue", `{"id":"ENG-1"}`))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"jsonrpc":"2.0","id":1,"result":{}}` || f.up.hitCount() != 1 {
		t.Fatalf("answer %d %q, upstream hits %d; want the server's answer after one forward", rec.Code, rec.Body.String(), f.up.hitCount())
	}
	rows := cp.snapshot()
	if len(rows) != 1 || rows[0].polls < 2 {
		t.Fatalf("rows %+v, want one row polled until approved", rows)
	}
	s := rows[0].scope
	if s.Tool != "mcp__linear__delete_issue" || s.MCPServer != "linear" || s.MCPHost != "mcp.linear.test" || s.MCPTool != "" ||
		string(s.Arguments) != `{"id":"ENG-1"}` || len(s.Extra) != 0 || s.HoldSeconds != 45 {
		t.Fatalf("scope %+v", s)
	}
	if want, _ := types.MCPCallKey("linear", "delete_issue", json.RawMessage(`{"id":"ENG-1"}`), nil); s.CallSHA256 != want {
		t.Fatalf("call_sha256 %s, want %s", s.CallSHA256, want)
	}
	d := findDecision(t, f.buf, "brokered:mcp:approved")
	if d.Decision != egress.Allow || d.ApprovalID == nil || *d.ApprovalID != rows[0].id {
		t.Fatalf("approved row %+v, want allow naming approval %s", d, rows[0].id)
	}
}

// TestMCPApproveEachWindowsByHarness: the window a scope states is the harness's (Claude Code
// longer than Codex CLI's 60 s tool timeout allows), and the sealed config's windows win.
func TestMCPApproveEachWindowsByHarness(t *testing.T) {
	f, cp := newEachFixture(t)
	cp.approveOnPoll = 1
	f.post(t, mcpToken("claude-code"), "linear", eachCall(1, "delete_issue", `{}`))
	f.post(t, mcpToken("codex-cli"), "linear", eachCall(1, "delete_issue", `{"n":1}`))
	f.p.mcp.setApproveEach(&MCPApproveEachConfig{Windows: map[string]int{"codex-cli": 7}})
	f.post(t, mcpToken("codex-cli"), "linear", eachCall(1, "delete_issue", `{"n":2}`))
	var got []int
	for _, row := range cp.snapshot() {
		got = append(got, row.scope.HoldSeconds)
	}
	if fmt.Sprint(got) != "[240 45 7]" {
		t.Fatalf("hold_seconds %v, want [240 45 7]", got)
	}
}

// TestMCPApproveEachSendsExactlyWhatWasShown: the server receives the canonical arguments and
// extra params byte for byte as the scope holds them, the raw id and name, and of _meta only the
// progress token.
func TestMCPApproveEachSendsExactlyWhatWasShown(t *testing.T) {
	f, cp := newEachFixture(t)
	cp.approveOnPoll = 1
	body := `{"jsonrpc":"2.0","id":"call-7","method":"tools/call","params":{"_meta":{"progressToken":"pt-1","other":"x"},` +
		`"task":{"ttl":1.0e3},"arguments":{"z":[1.0,2],"a":"<b>&é"},"name":"delete_issue"}}`
	if rec := f.post(t, mcpToken("codex-cli"), "linear", body); rec.Code != http.StatusOK || f.up.hitCount() != 1 {
		t.Fatalf("answer %d %q, upstream hits %d", rec.Code, rec.Body.String(), f.up.hitCount())
	}
	rows := cp.snapshot()
	if len(rows) != 1 {
		t.Fatalf("rows %d, want 1", len(rows))
	}
	s := rows[0].scope
	if string(s.Arguments) != `{"a":"\u003cb\u003e\u0026é","z":[1,2]}` || string(s.Extra) != `{"task":{"ttl":1000}}` {
		t.Fatalf("scope arguments %s extra %s", s.Arguments, s.Extra)
	}
	f.up.mu.Lock()
	sent := string(f.up.body)
	f.up.mu.Unlock()
	want := `{"jsonrpc":"2.0","id":"call-7","method":"tools/call","params":{"name":"delete_issue","arguments":` + string(s.Arguments) +
		`,"task":{"ttl":1000},"_meta":{"progressToken":"pt-1"}}}`
	if sent != want {
		t.Fatalf("upstream body\n %s\nwant\n %s", sent, want)
	}
	if params := upstreamParams(t, f.up); !bytes.Equal(params["arguments"], s.Arguments) {
		t.Fatalf("upstream arguments %s, scope arguments %s", params["arguments"], s.Arguments)
	}
}

// TestMCPApproveEachKeepsTheClientsIDText: the id goes to the server as the client wrote it, so the answer
// carries an id the client matches byte for byte.
func TestMCPApproveEachKeepsTheClientsIDText(t *testing.T) {
	f, cp := newEachFixture(t)
	cp.approveOnPoll = 1
	body := `{"jsonrpc":"2.0","id":1.0,"method":"tools/call","params":{"name":"delete_issue","arguments":{"id":"ENG-1"}}}`
	if rec := f.post(t, mcpToken("codex-cli"), "linear", body); rec.Code != http.StatusOK || f.up.hitCount() != 1 {
		t.Fatalf("answer %d %q, upstream hits %d", rec.Code, rec.Body.String(), f.up.hitCount())
	}
	f.up.mu.Lock()
	sent := string(f.up.body)
	f.up.mu.Unlock()
	if !strings.HasPrefix(sent, `{"jsonrpc":"2.0","id":1.0,"method":"tools/call"`) {
		t.Fatalf("upstream body %s, want the id 1.0 as sent", sent)
	}
}

// TestMCPApproveEachOncePerApproval: one approval sends one call. Two identical calls in flight
// on it forward once; the other asks again.
func TestMCPApproveEachOncePerApproval(t *testing.T) {
	f, cp := newEachFixture(t)
	call := eachCall(1, "delete_issue", `{"id":"ENG-1"}`)
	var wg sync.WaitGroup
	recs := make([]*httptest.ResponseRecorder, 2)
	for i := range recs {
		wg.Add(1)
		go func() { defer wg.Done(); recs[i] = f.post(t, mcpToken("codex-cli"), "linear", call) }()
	}
	waitEachRows(t, cp, 1)
	time.Sleep(50 * time.Millisecond) // both are waiting on the one row
	cp.decide(0, types.ApprovalApproved)
	wg.Wait()
	if f.up.hitCount() != 1 {
		t.Fatalf("upstream hits %d, want 1: one approval is one call", f.up.hitCount())
	}
	if rows := cp.snapshot(); len(rows) != 2 || rows[1].state != types.ApprovalPending {
		t.Fatalf("rows %+v, want the loser to raise a second row", rows)
	}
	waiting := 0
	for _, rec := range recs {
		if strings.Contains(rec.Body.String(), "waiting for a person") {
			waiting++
		}
	}
	if waiting != 1 {
		t.Fatalf("answers %q / %q, want exactly one waiting text", recs[0].Body.String(), recs[1].Body.String())
	}
	// A third identical call spends nothing: the first approval is gone.
	f.post(t, mcpToken("codex-cli"), "linear", call)
	if f.up.hitCount() != 1 {
		t.Fatalf("upstream hits %d after a third call, want 1", f.up.hitCount())
	}
}

// TestMCPApproveEachChangedArgumentsAskAgain: an approval is for its arguments only; a call with
// other arguments asks again and the first approval stays unspent for its own call.
func TestMCPApproveEachChangedArgumentsAskAgain(t *testing.T) {
	f, cp := newEachFixture(t)
	x, y := eachCall(1, "delete_issue", `{"id":"ENG-1"}`), eachCall(2, "delete_issue", `{"id":"ENG-2"}`)
	wantText(t, f.post(t, mcpToken("codex-cli"), "linear", x), "waiting for a person to approve this call")
	cp.decide(0, types.ApprovalApproved)
	wantText(t, f.post(t, mcpToken("codex-cli"), "linear", y), "waiting for a person to approve this call")
	if cp.count() != 2 || f.up.hitCount() != 0 {
		t.Fatalf("rows %d, upstream hits %d; want a second row and nothing sent", cp.count(), f.up.hitCount())
	}
	if rec := f.post(t, mcpToken("codex-cli"), "linear", x); rec.Code != http.StatusOK || f.up.hitCount() != 1 {
		t.Fatalf("answer %q, upstream hits %d; want the approved call sent", rec.Body.String(), f.up.hitCount())
	}
	if p := upstreamParams(t, f.up); string(p["arguments"]) != `{"id":"ENG-1"}` {
		t.Fatalf("sent arguments %s, want the approved ones", p["arguments"])
	}
}

// TestMCPApproveEachDeclineIsSticky: a declined call is refused for the rest of the run without
// asking again; other arguments still ask.
func TestMCPApproveEachDeclineIsSticky(t *testing.T) {
	f, cp := newEachFixture(t)
	cp.newState = types.ApprovalDenied
	call := eachCall(1, "delete_issue", `{"id":"ENG-1"}`)
	wantText(t, f.post(t, mcpToken("codex-cli"), "linear", call), "A person declined this call")
	wantText(t, f.post(t, mcpToken("codex-cli"), "linear", call), "identical retries are refused")
	if cp.count() != 1 || f.up.hitCount() != 0 {
		t.Fatalf("rows %d, upstream hits %d; want one row and nothing sent", cp.count(), f.up.hitCount())
	}
	if d := findDecision(t, f.buf, "brokered:mcp:approval-denied"); d.Decision != egress.Deny || d.ApprovalID == nil {
		t.Fatalf("denied row %+v", d)
	}
	f.post(t, mcpToken("codex-cli"), "linear", eachCall(2, "delete_issue", `{"id":"ENG-2"}`))
	if cp.count() != 2 {
		t.Fatalf("rows %d, want other arguments to ask", cp.count())
	}
}

// TestMCPApproveEachExpiredAsksAgain: a request that closed undecided is forgotten; the next
// identical call asks again.
func TestMCPApproveEachExpiredAsksAgain(t *testing.T) {
	f, cp := newEachFixture(t)
	cp.newState = types.ApprovalExpired
	call := eachCall(1, "delete_issue", `{"id":"ENG-1"}`)
	wantText(t, f.post(t, mcpToken("codex-cli"), "linear", call), "closed without a decision")
	if d := findDecision(t, f.buf, "brokered:mcp:approval-closed"); d.Decision != egress.Deny || d.ApprovalID == nil {
		t.Fatalf("closed row %+v", d)
	}
	cp.newState = types.ApprovalCancelled
	wantText(t, f.post(t, mcpToken("codex-cli"), "linear", call), "closed without a decision")
	if cp.count() != 2 || f.up.hitCount() != 0 {
		t.Fatalf("rows %d, upstream hits %d; want a second row and nothing sent", cp.count(), f.up.hitCount())
	}
}

// TestMCPApproveEachWindowEndsWithWaitingText: when the window ends the call is answered with the
// waiting text as a tool error, the row stays PENDING, and a retry rejoins it.
func TestMCPApproveEachWindowEndsWithWaitingText(t *testing.T) {
	f, cp := newEachFixture(t)
	call := eachCall(1, "delete_issue", `{"id":"ENG-1"}`)
	start := time.Now()
	rec := f.post(t, mcpToken("codex-cli"), "linear", call)
	rows := cp.snapshot()
	if len(rows) != 1 || rows[0].state != types.ApprovalPending || f.up.hitCount() != 0 {
		t.Fatalf("rows %+v, upstream hits %d", rows, f.up.hitCount())
	}
	if el := time.Since(start); el < 400*time.Millisecond {
		t.Fatalf("answered after %s, want the 45-second window (10 ms seconds in this test)", el)
	}
	wantText(t, rec, "Wardyn is waiting for a person to approve this call (request "+rows[0].id.String()+"). It was not sent.")
	if d := findDecision(t, f.buf, "brokered:mcp:approval-wait"); d.Decision != egress.Pending || d.ApprovalID == nil || *d.ApprovalID != rows[0].id {
		t.Fatalf("wait row %+v", d)
	}
	f.post(t, mcpToken("codex-cli"), "linear", call)
	cp.mu.Lock()
	rejoins := append([]uuid.UUID(nil), cp.rejoins...)
	cp.mu.Unlock()
	if cp.count() != 1 || len(rejoins) != 1 || rejoins[0] != rows[0].id {
		t.Fatalf("rows %d, rejoins %v; want the retry to rejoin row %s", cp.count(), rejoins, rows[0].id)
	}
}

// TestMCPApproveEachClientGoneStopsWaiting: a client that goes away ends the wait at once; the row
// stays PENDING and nothing is sent.
func TestMCPApproveEachClientGoneStopsWaiting(t *testing.T) {
	f, cp := newEachFixture(t)
	f.p.mcp.setApproveEach(&MCPApproveEachConfig{Windows: map[string]int{"codex-cli": 300}})
	ctx, cancel := context.WithCancel(context.Background())
	req := mustLocalReq(t, http.MethodPost, "/wardyn/mcp/"+mcpToken("codex-cli")+"/linear", strings.NewReader(eachCall(1, "delete_issue", `{}`)))
	req = req.WithContext(ctx)
	done := make(chan struct{})
	go func() { f.p.ServeHTTP(httptest.NewRecorder(), req); close(done) }()
	waitEachRows(t, cp, 1)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the wait went on after the client left")
	}
	if rows := cp.snapshot(); rows[0].state != types.ApprovalPending || f.up.hitCount() != 0 {
		t.Fatalf("rows %+v, upstream hits %d", rows, f.up.hitCount())
	}
}

// TestMCPApproveEachBatchRefused: a batch holding an approve_each call is refused whole, every
// request answered -32002, and nothing is asked or sent.
func TestMCPApproveEachBatchRefused(t *testing.T) {
	f, cp := newEachFixture(t)
	body := "[" + mcpInit + "," + eachCall(2, "delete_issue", `{}`) + "]"
	rec := f.post(t, mcpToken("codex-cli"), "linear", body)
	var out []struct {
		ID    int `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out) != 2 {
		t.Fatalf("answer %q, want two errors", rec.Body.String())
	}
	for _, e := range out {
		if e.Error.Code != -32002 || e.Error.Message != "Send this tool call on its own: it needs a person's approval" {
			t.Fatalf("answer %q", rec.Body.String())
		}
	}
	if cp.count() != 0 || f.up.hitCount() != 0 {
		t.Fatalf("rows %d, upstream hits %d", cp.count(), f.up.hitCount())
	}
	wantDecision(t, f.buf, egress.Deny, "brokered:mcp:approval-refused")
}

// TestMCPApproveEachTooLarge: arguments over 32 KiB are refused, never truncated or asked.
func TestMCPApproveEachTooLarge(t *testing.T) {
	f, cp := newEachFixture(t)
	big := `{"text":"` + strings.Repeat("x", 32<<10) + `"}`
	wantText(t, f.post(t, mcpToken("codex-cli"), "linear", eachCall(1, "delete_issue", big)), "larger than Wardyn can show a person (32 KiB)")
	if cp.count() != 0 || f.up.hitCount() != 0 {
		t.Fatalf("rows %d, upstream hits %d", cp.count(), f.up.hitCount())
	}
	wantDecision(t, f.buf, egress.Deny, "brokered:mcp:approval-refused")
}

// TestMCPApproveEachRepeatedKey: arguments that repeat a key at any depth are refused: a server
// could read the other value.
func TestMCPApproveEachRepeatedKey(t *testing.T) {
	f, cp := newEachFixture(t)
	wantText(t, f.post(t, mcpToken("codex-cli"), "linear", eachCall(1, "delete_issue", `{"a":{"b":1,"b":2}}`)), "repeats a key")
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_issue","arguments":{},"Arguments":{"id":"x"}}}`
	if rec := f.post(t, mcpToken("codex-cli"), "linear", body); !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("a case-variant arguments key answered %q, want a refusal", rec.Body.String())
	}
	if cp.count() != 0 || f.up.hitCount() != 0 {
		t.Fatalf("rows %d, upstream hits %d", cp.count(), f.up.hitCount())
	}
}

// TestMCPApproveEachCaps: 16 calls may wait at once and 1024 distinct calls may be asked about in
// a run; past either the call is refused, never sent.
func TestMCPApproveEachCaps(t *testing.T) {
	f, cp := newEachFixture(t)
	f.p.mcp.setApproveEach(&MCPApproveEachConfig{Windows: map[string]int{"codex-cli": 300}})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		req := mustLocalReq(t, http.MethodPost, "/wardyn/mcp/"+mcpToken("codex-cli")+"/linear", strings.NewReader(eachCall(i, "delete_issue", fmt.Sprintf(`{"n":%d}`, i))))
		go func() { defer wg.Done(); f.p.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx)) }()
	}
	waitEachRows(t, cp, 16)
	time.Sleep(20 * time.Millisecond)
	wantText(t, f.post(t, mcpToken("codex-cli"), "linear", eachCall(99, "delete_issue", `{"n":99}`)), "Too many calls are already waiting for a person in this run")
	cancel()
	wg.Wait()

	cp.newState = types.ApprovalDenied
	for i := 16; i < 1024; i++ {
		f.post(t, mcpToken("codex-cli"), "linear", eachCall(i, "delete_issue", fmt.Sprintf(`{"n":%d}`, i)))
	}
	if cp.count() != 1024 {
		t.Fatalf("rows %d, want 1024", cp.count())
	}
	wantText(t, f.post(t, mcpToken("codex-cli"), "linear", eachCall(1, "delete_issue", `{"n":"new"}`)), "asked about too many different calls")
	if cp.count() != 1024 || f.up.hitCount() != 0 {
		t.Fatalf("rows %d, upstream hits %d", cp.count(), f.up.hitCount())
	}
	wantDecision(t, f.buf, egress.Deny, "brokered:mcp:approval-refused")
}

// TestMCPApproveEachRaiseRefused: when the control plane refuses the request for an approval, the
// call is refused and not sent.
func TestMCPApproveEachRaiseRefused(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests} {
		f, cp := newEachFixture(t)
		cp.refuse = status
		wantText(t, f.post(t, mcpToken("codex-cli"), "linear", eachCall(1, "delete_issue", `{}`)), "was not sent")
		if f.up.hitCount() != 0 {
			t.Fatalf("status %d: upstream hits %d", status, f.up.hitCount())
		}
		wantDecision(t, f.buf, egress.Deny, "brokered:mcp:approval-refused")
	}
}

// TestMCPApproveEachDenyWins: a tool that one of its names denies is blocked, even when another
// of its names says approve_each.
func TestMCPApproveEachDenyWins(t *testing.T) {
	f, cp := newEachFixture(t,
		types.ToolRule{Tool: "mcp__linear__delete-issue", Effect: types.ToolApproveEach},
		types.ToolRule{Tool: "mcp__linear__delete_issue", Effect: types.ToolDeny})
	rec := f.post(t, mcpToken("codex-cli"), "linear", eachCall(1, "delete-issue", `{}`))
	if !strings.Contains(rec.Body.String(), `"code":-32001`) || cp.count() != 0 || f.up.hitCount() != 0 {
		t.Fatalf("answer %q, rows %d, upstream hits %d; want the block", rec.Body.String(), cp.count(), f.up.hitCount())
	}
}

// TestMCPApproveEachNewProxyAsksAgain: the spend table lives in the run's one proxy, so a new
// proxy asks again even for a call already approved.
func TestMCPApproveEachNewProxyAsksAgain(t *testing.T) {
	f, cp := newEachFixture(t)
	call := eachCall(1, "delete_issue", `{"id":"ENG-1"}`)
	f.post(t, mcpToken("codex-cli"), "linear", call)
	cp.decide(0, types.ApprovalApproved)
	g := newMCPFixture(t, types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "mcp__linear__delete_issue", Effect: types.ToolApproveEach}}}, nil, nil)
	g.p.mcp.setApproveEach(&MCPApproveEachConfig{})
	cp.wire(t, g.p)
	wantText(t, g.post(t, mcpToken("codex-cli"), "linear", call), "waiting for a person")
	if cp.count() != 2 || g.up.hitCount() != 0 {
		t.Fatalf("rows %d, upstream hits %d; want a new row and nothing sent", cp.count(), g.up.hitCount())
	}
}

// TestMCPApproveEachRawNameInKey: delete.issue and delete_issue share one display name and one
// rule, but are two tools. An approval for one is never spent on the other; each needs its own,
// and each forward carries its own raw name.
func TestMCPApproveEachRawNameInKey(t *testing.T) {
	f, cp := newEachFixture(t)
	args := `{"id":"ENG-1"}`
	wantText(t, f.post(t, mcpToken("codex-cli"), "linear", eachCall(1, "delete_issue", args)), "waiting for a person")
	cp.decide(0, types.ApprovalApproved)
	wantText(t, f.post(t, mcpToken("codex-cli"), "linear", eachCall(2, "delete.issue", args)), "waiting for a person")
	rows := cp.snapshot()
	if len(rows) != 2 || f.up.hitCount() != 0 {
		t.Fatalf("rows %d, upstream hits %d; want a second row and nothing sent", len(rows), f.up.hitCount())
	}
	if s := rows[1].scope; s.Tool != "mcp__linear__delete_issue" || s.MCPTool != "delete.issue" || s.CallSHA256 == rows[0].scope.CallSHA256 {
		t.Fatalf("second scope %+v, want the raw name delete.issue and its own key", s)
	}
	cp.decide(1, types.ApprovalApproved)
	for i, tool := range []string{"delete.issue", "delete_issue"} {
		if rec := f.post(t, mcpToken("codex-cli"), "linear", eachCall(3+i, tool, args)); rec.Code != http.StatusOK || f.up.hitCount() != i+1 {
			t.Fatalf("%s: answer %q, upstream hits %d", tool, rec.Body.String(), f.up.hitCount())
		}
		var name string
		_ = json.Unmarshal(upstreamParams(t, f.up)["name"], &name)
		if name != tool {
			t.Fatalf("forwarded name %q, want %q", name, tool)
		}
	}
}

// TestMCPApproveEachOtherCallsUntouched: a call of a tool that is not approve_each, and every
// other message, pass byte for byte.
func TestMCPApproveEachOtherCallsUntouched(t *testing.T) {
	f, cp := newEachFixture(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_issues","arguments":{"b":1.0,"a":2}}}`
	if rec := f.post(t, mcpToken("codex-cli"), "linear", body); rec.Code != http.StatusOK || f.up.hitCount() != 1 {
		t.Fatalf("answer %d %q", rec.Code, rec.Body.String())
	}
	f.up.mu.Lock()
	sent := string(f.up.body)
	f.up.mu.Unlock()
	if sent != body || cp.count() != 0 {
		t.Fatalf("sent %q, rows %d; want the body unchanged and nothing asked", sent, cp.count())
	}
}

// TestMCPApproveEachStaleApprovedPollNeverRespends: two calls wait on approval A of one call key.
// One answer saying A is approved is held back while the other call spends A, and a later call
// spends approval B. The held answer then arrives: A is spent for good, so nothing is sent a third
// time.
func TestMCPApproveEachStaleApprovedPollNeverRespends(t *testing.T) {
	f, cp := newEachFixture(t)
	f.p.mcp.setApproveEach(&MCPApproveEachConfig{Windows: map[string]int{"codex-cli": 300}})
	// Registered after the control plane, so it runs first: a held answer never blocks its Close.
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	within := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(time.Millisecond)
		}
	}
	closed := func(ch <-chan struct{}) func() bool {
		return func() bool {
			select {
			case <-ch:
				return true
			default:
				return false
			}
		}
	}
	call := eachCall(1, "delete_issue", `{"id":"ENG-1"}`)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); f.post(t, mcpToken("codex-cli"), "linear", call) }()
	}
	waiters := make(chan struct{})
	go func() { wg.Wait(); close(waiters) }()
	// Both calls hold A: each took a slot and had its raise (or rejoin) answered with the one row.
	within("both calls to wait on A", func() bool {
		cp.mu.Lock()
		posts, rows := cp.posts, len(cp.rows)
		cp.mu.Unlock()
		f.p.mcp.calls.mu.Lock()
		defer f.p.mcp.calls.mu.Unlock()
		k := f.p.mcp.calls.byKey
		return posts == 2 && rows == 1 && len(k) == 1 && func() bool {
			for _, v := range k {
				return v.live == 2
			}
			return false
		}()
	})
	cp.mu.Lock()
	cp.stall, cp.stalled = release, make(chan struct{})
	stalled := cp.stalled
	cp.mu.Unlock()
	cp.decide(0, types.ApprovalApproved)
	within("one A=APPROVED answer to be held", closed(stalled))
	within("the other call to spend A", func() bool { return f.up.hitCount() == 1 })
	// A third call raises B, which is approved and spent.
	third := make(chan struct{})
	go func() { defer close(third); f.post(t, mcpToken("codex-cli"), "linear", call) }()
	within("the third call to raise B", func() bool { return cp.count() == 2 })
	cp.decide(1, types.ApprovalApproved)
	within("the third call to finish", closed(third))
	if f.up.hitCount() != 2 {
		t.Fatalf("upstream hits %d after B, want 2", f.up.hitCount())
	}
	once.Do(func() { close(release) }) // the stale A=APPROVED answer arrives
	within("the held call to finish", closed(waiters))
	if f.up.hitCount() != 2 {
		t.Fatalf("upstream hits %d, want 2: two approvals send two calls", f.up.hitCount())
	}
}

// TestMCPCallsSpentApprovalNotReclaimedAfterKeyForgotten: a call F reads approval A from the
// approved fast path, waiter W spends A and leaves (the key is forgotten), and call N recreates
// the key waiting on B. A must not be claimable again: F would send it a second time.
func TestMCPCallsSpentApprovalNotReclaimedAfterKeyForgotten(t *testing.T) {
	const k = "K"
	a, b := uuid.New(), uuid.New()
	var c mcpCalls
	// 1. W waits on A, which is approved, and has not claimed it yet.
	c.admit(k)
	c.install(k, a)
	c.update(k, a, apApproved)
	// 2. F admits and reads A on the approved fast path.
	fid, fstate, _ := c.admit(k)
	// 3. W claims A, sends, and leaves.
	c.claim(k, a)
	c.leave(k)
	// 4. N admits and waits on B.
	c.admit(k)
	c.install(k, b)
	// 5. F claims what it read.
	if fstate == apApproved && fid == a && c.claim(k, a) {
		t.Fatal("approval A was claimed twice: one approval sends one call")
	}
	if c.claim(k, a) {
		t.Fatal("spent approval A is claimable after its key was recreated")
	}
}
