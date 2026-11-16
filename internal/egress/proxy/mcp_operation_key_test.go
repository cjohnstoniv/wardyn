// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// opSend is one send record the fake control plane keeps for an operation key.
type opSend struct {
	id       uuid.UUID
	sha      string
	at       time.Time
	answered bool
}

// opPost is one send record post the fake control plane got, with the MCP server's hit count then.
type opPost struct {
	id       uuid.UUID
	answered bool
	hitsAt   int
}

// opCP is a control plane that keeps send records as the real one does for one run's namespace: a
// record only for an approved row with an operation key, a second record of a key answered 409 with
// the first, and a raise of a key already sent answered 409 instead of raising. It folds a raise into
// the PENDING row with the same hash and key, and answers polls from its rows.
type opCP struct {
	srv  *httptest.Server
	hits func() int // the MCP server's hit count

	mu       sync.Mutex
	rows     []*approveEachRow
	raw      []json.RawMessage // each new row's requested_scope as the proxy posted it
	posts    int               // raises and rejoins answered, of any outcome
	approve  bool              // a new row starts APPROVED
	sends    map[string]opSend // by operation key
	sent     []opPost
	sentFail int // when set, every send record post is answered with this status
}

func newOpCP(t *testing.T, hits func() int) *opCP {
	t.Helper()
	c := &opCP{hits: hits, sends: map[string]opSend{}}
	c.srv = httptest.NewServer(http.HandlerFunc(c.serve))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *opCP) serve(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	const base = "/api/v1/internal/approvals"
	rest, under := strings.CutPrefix(r.URL.Path, base+"/")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == base:
		c.posts++
		c.raise(w, r)
	case r.Method == http.MethodPost && under && strings.HasSuffix(rest, "/sent"):
		c.record(w, r, strings.TrimSuffix(rest, "/sent"))
	case r.Method == http.MethodGet && under:
		for _, row := range c.rows {
			if rest == row.id.String() {
				row.polls++
				_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: row.id, State: row.state})
				return
			}
		}
		http.Error(w, "no such approval", http.StatusNotFound)
	default:
		http.Error(w, "unexpected", http.StatusTeapot)
	}
}

// opConflict answers 409 for a call with hash sha whose key was sent as s.
func opConflict(w http.ResponseWriter, s opSend, sha string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	if s.sha != sha {
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "reused", "reason": types.MCPOperationKeyReused})
		return
	}
	_ = json.NewEncoder(w).Encode(types.MCPCallSentAnswer{Error: "already sent", Reason: types.MCPOperationAlreadySent,
		ApprovalID: s.id, SentAt: s.at, Answered: s.answered})
}

func (c *opCP) raise(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind           types.ApprovalKind `json:"kind"`
		RequestedScope json.RawMessage    `json:"requested_scope"`
		RejoinID       *uuid.UUID         `json:"rejoin_id"`
	}
	var scope types.MCPCallScope
	_ = json.NewDecoder(r.Body).Decode(&body)
	dec := json.NewDecoder(bytes.NewReader(body.RequestedScope))
	dec.DisallowUnknownFields()
	if body.Kind != types.ApprovalToolCall || dec.Decode(&scope) != nil {
		http.Error(w, "bad raise", http.StatusBadRequest)
		return
	}
	if key, err := scope.ComputeKey(); err != nil || key != scope.CallSHA256 {
		http.Error(w, "scope does not hash to its call_sha256", http.StatusBadRequest)
		return
	}
	if s, sent := c.sends[scope.OperationKey]; sent && scope.OperationKey != "" {
		opConflict(w, s, scope.CallSHA256)
		return
	}
	for _, row := range c.rows {
		if row.state != types.ApprovalPending {
			continue
		}
		if body.RejoinID != nil && row.id == *body.RejoinID ||
			body.RejoinID == nil && row.scope.CallSHA256 == scope.CallSHA256 && row.scope.OperationKey == scope.OperationKey {
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: row.id, State: row.state})
			return
		}
	}
	if body.RejoinID != nil {
		http.Error(w, "no pending approval to rejoin", http.StatusNotFound)
		return
	}
	row := &approveEachRow{id: uuid.New(), scope: scope, state: types.ApprovalPending}
	if c.approve {
		row.state = types.ApprovalApproved
	}
	c.rows = append(c.rows, row)
	c.raw = append(c.raw, append(json.RawMessage(nil), body.RequestedScope...))
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: row.id, State: row.state})
}

func (c *opCP) record(w http.ResponseWriter, r *http.Request, rawID string) {
	var body struct {
		Answered *bool `json:"answered"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	id, err := uuid.Parse(rawID)
	if err != nil || dec.Decode(&body) != nil || body.Answered == nil {
		http.Error(w, "bad send record", http.StatusBadRequest)
		return
	}
	c.sent = append(c.sent, opPost{id: id, answered: *body.Answered, hitsAt: c.hits()})
	if c.sentFail != 0 {
		http.Error(w, "send record refused", c.sentFail)
		return
	}
	var row *approveEachRow
	for _, rw := range c.rows {
		if rw.id == id && rw.state == types.ApprovalApproved && rw.scope.OperationKey != "" {
			row = rw
		}
	}
	if row == nil {
		http.Error(w, "approval not found", http.StatusNotFound)
		return
	}
	key := row.scope.OperationKey
	s, sent := c.sends[key]
	switch {
	case *body.Answered && (!sent || s.id != id):
		http.Error(w, "approval not found", http.StatusNotFound)
	case *body.Answered:
		s.answered = true
		c.sends[key] = s
		w.WriteHeader(http.StatusNoContent)
	case sent:
		opConflict(w, s, row.scope.CallSHA256)
	default:
		s = opSend{id: id, sha: row.scope.CallSHA256, at: time.Now().UTC().Truncate(time.Second)}
		c.sends[key] = s
		_ = json.NewEncoder(w).Encode(types.MCPCallSentAnswer{ApprovalID: id, SentAt: s.at})
	}
}

func (c *opCP) raises() int { c.mu.Lock(); defer c.mu.Unlock(); return c.posts }

func (c *opCP) records() []opPost {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]opPost(nil), c.sent...)
}

func (c *opCP) send(key string) opSend { c.mu.Lock(); defer c.mu.Unlock(); return c.sends[key] }

// seed records s as key's send, as another proxy's record would.
func (c *opCP) seed(key string, s opSend) { c.mu.Lock(); defer c.mu.Unlock(); c.sends[key] = s }

// set moves row i to state s, as a person's decision does.
func (c *opCP) set(i int, s types.ApprovalState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows[i].state = s
}

func (c *opCP) snapshot() ([]approveEachRow, []json.RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]approveEachRow, len(c.rows))
	for i, row := range c.rows {
		out[i] = *row
	}
	return out, append([]json.RawMessage(nil), c.raw...)
}

// newOpFixture is the broker fixture with linear's delete_issue on approve_each and a control plane that
// keeps send records.
func newOpFixture(t *testing.T) (*mcpFixture, *opCP) {
	t.Helper()
	f := newMCPFixture(t, types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "mcp__linear__delete_issue", Effect: types.ToolApproveEach}}}, nil, nil)
	f.p.mcp.setApproveEach(&MCPApproveEachConfig{})
	cp := newOpCP(t, f.up.hitCount)
	(&eachCP{srv: cp.srv}).wire(t, f.p)
	return f, cp
}

// keyedCall is a tools/call of linear's delete_issue with arguments args and a _meta holding a progress
// token and the operation key keyJSON (JSON text).
func keyedCall(id int, args, keyJSON string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"delete_issue","arguments":%s,"_meta":{"progressToken":"pt-1","wardyn.dev/operation-key":%s}}}`, id, args, keyJSON)
}

// opCallKey is the hash of linear's delete_issue with args, which no operation key changes.
func opCallKey(t *testing.T, args string) string {
	t.Helper()
	key, err := types.MCPCallKey("linear", "delete_issue", json.RawMessage(args), nil)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// wantSentText checks the agent's answer to a call whose operation was already sent as s: it names the
// approval and the time, says whether the server answered, and that nothing was sent again.
func wantSentText(t *testing.T, rec *httptest.ResponseRecorder, s opSend) {
	t.Helper()
	got := toolResultText(t, rec)
	clause := "no answer from its server was recorded"
	if s.answered {
		clause = "its server answered"
	}
	for _, want := range []string{s.id.String(), s.at.UTC().Format(time.RFC3339), clause, "did not send it again"} {
		if !strings.Contains(got, want) {
			t.Fatalf("text %q, want it to contain %q", got, want)
		}
	}
}

// TestMCPOperationKeyRidesTheRaiseNotTheSend: a call's operation key reaches the control plane in the
// raised scope, outside the call's hash, and never the MCP server, which gets of _meta only the progress
// token.
func TestMCPOperationKeyRidesTheRaiseNotTheSend(t *testing.T) {
	f, cp := newOpFixture(t)
	cp.approve = true
	rec := f.post(t, mcpToken("codex-cli"), "linear", keyedCall(1, `{"id":"ENG-1"}`, `"step-1"`))
	if rec.Code != http.StatusOK || f.up.hitCount() != 1 {
		t.Fatalf("answer %d %q, upstream hits %d; want one send", rec.Code, rec.Body.String(), f.up.hitCount())
	}
	rows, raw := cp.snapshot()
	if len(rows) != 1 || rows[0].scope.OperationKey != "step-1" || rows[0].scope.CallSHA256 != opCallKey(t, `{"id":"ENG-1"}`) ||
		!strings.Contains(string(raw[0]), `"operation_key":"step-1"`) {
		t.Fatalf("raised %+v (%s); want one row carrying step-1 and the keyless call hash", rows, raw)
	}
	f.up.mu.Lock()
	got := string(f.up.body)
	f.up.mu.Unlock()
	if want := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_issue","arguments":{"id":"ENG-1"},"_meta":{"progressToken":"pt-1"}}}`; got != want {
		t.Fatalf("the server got\n %s\nwant\n %s", got, want)
	}
}

// TestMCPOperationKeyMalformedIsRefusedUnasked: a key that is not 1 to 128 of the allowed characters, or
// not a string, is answered with the key's rule and nothing is asked or sent; a case variant of the
// member's name is a request Wardyn cannot read.
func TestMCPOperationKeyMalformedIsRefusedUnasked(t *testing.T) {
	f, cp := newOpFixture(t)
	for _, key := range []string{`"step 1"`, `""`, `"` + strings.Repeat("a", 129) + `"`, `"caf\u00e9"`, `7`, `null`} {
		if got := toolResultText(t, f.post(t, mcpToken("codex-cli"), "linear", keyedCall(1, `{"id":"ENG-1"}`, key))); got != mcpCallOperationKeyText {
			t.Fatalf("key %s: text %q, want the key's rule", key, got)
		}
		if d := wantDecision(t, f.buf, egress.Deny, ruleSourceMCPApprovalRefused); d.Cause != mcpCallOperationKeyText {
			t.Fatalf("key %s: refused row cause %q", key, d.Cause)
		}
	}
	body := strings.Replace(keyedCall(2, `{"id":"ENG-1"}`, `"step-1"`), `"wardyn.dev/`, `"Wardyn.dev/`, 1)
	if rec := f.post(t, mcpToken("codex-cli"), "linear", body); !strings.Contains(rec.Body.String(), `"code":-32700`) ||
		!strings.Contains(rec.Body.String(), "ambiguous key") {
		t.Fatalf("a case variant of the member = %q, want -32700 naming the ambiguous key", rec.Body.String())
	}
	if cp.raises() != 0 || f.up.hitCount() != 0 {
		t.Fatalf("raises %d, upstream hits %d; want nothing asked or sent", cp.raises(), f.up.hitCount())
	}
}

// TestMCPOperationKeyRecordsBeforeSending: an approved keyed call's send is recorded before the MCP server
// gets it and stamped answered after; the forward's row names the approval.
func TestMCPOperationKeyRecordsBeforeSending(t *testing.T) {
	f, cp := newOpFixture(t)
	cp.approve = true
	if rec := f.post(t, mcpToken("codex-cli"), "linear", keyedCall(1, `{"id":"ENG-1"}`, `"step-1"`)); rec.Code != http.StatusOK || f.up.hitCount() != 1 {
		t.Fatalf("answer %d %q, upstream hits %d; want one send", rec.Code, rec.Body.String(), f.up.hitCount())
	}
	rows, _ := cp.snapshot()
	if len(rows) != 1 {
		t.Fatalf("rows %+v, want one", rows)
	}
	id := rows[0].id
	if got, want := cp.records(), []opPost{{id: id, answered: false, hitsAt: 0}, {id: id, answered: true, hitsAt: 1}}; !slices.Equal(got, want) {
		t.Fatalf("send records %+v, want %+v: recorded before the send, stamped after", got, want)
	}
	if s := cp.send("step-1"); s.id != id || !s.answered {
		t.Fatalf("send %+v, want approval %s, answered", s, id)
	}
	if d := findDecision(t, f.buf, ruleSourceMCPApproved); d.ApprovalID == nil || *d.ApprovalID != id {
		t.Fatalf("approved row %+v, want it to name %s", d, id)
	}
}

// TestMCPOperationKeyUnrecordedIsNotSent: an approved keyed call whose send the control plane did not
// record with a 200 is not sent, and the agent is told so.
func TestMCPOperationKeyUnrecordedIsNotSent(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusNotFound, http.StatusInternalServerError, http.StatusNoContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f, cp := newOpFixture(t)
			cp.approve, cp.sentFail = true, status
			rec := f.post(t, mcpToken("codex-cli"), "linear", keyedCall(1, `{"id":"ENG-1"}`, `"step-1"`))
			if got := toolResultText(t, rec); got != mcpCallSendUnrecordedText || f.up.hitCount() != 0 {
				t.Fatalf("text %q, upstream hits %d; want the unrecorded text and no send", got, f.up.hitCount())
			}
			rows, _ := cp.snapshot()
			d := wantDecision(t, f.buf, egress.Deny, ruleSourceMCPApprovalRefused)
			if len(rows) != 1 || d.ApprovalID == nil || *d.ApprovalID != rows[0].id || d.Cause != "the send could not be recorded" {
				t.Fatalf("refused row %+v, rows %+v; want the cause naming the approval", d, rows)
			}
		})
	}
}

// TestMCPOperationKeyAlreadySentIsAnsweredWithItsSend: a keyed call whose key was already sent with this
// call is answered with that send and neither asked about nor sent again, whether its server answered
// (with any status) or not, and whether this proxy recorded the send or another did first.
func TestMCPOperationKeyAlreadySentIsAnsweredWithItsSend(t *testing.T) {
	call := func(id int) string { return keyedCall(id, `{"id":"ENG-1"}`, `"step-1"`) }
	for name, c := range map[string]struct {
		status   int  // the server's answer to the send
		down     bool // the server is gone before the send
		answered bool
	}{
		"answered":            {status: http.StatusOK, answered: true},
		"answered with a 500": {status: http.StatusInternalServerError, answered: true},
		"never answered":      {down: true},
	} {
		t.Run(name, func(t *testing.T) {
			f, cp := newOpFixture(t)
			cp.approve = true
			f.up.status = c.status
			if c.down {
				// The fixture's server also answers the credential, which is resolved before the record:
				// held first, so only the server is gone at the send.
				if _, err := f.p.mcpCredential(t.Context(), f.p.mcp.server("linear")); err != nil {
					t.Fatal(err)
				}
				f.up.srv.Close()
			}
			f.post(t, mcpToken("codex-cli"), "linear", call(1))
			s := cp.send("step-1")
			if s.id == uuid.Nil || s.answered != c.answered {
				t.Fatalf("send %+v, want one recorded with answered %v", s, c.answered)
			}
			hits := f.up.hitCount()
			wantSentText(t, f.post(t, mcpToken("codex-cli"), "linear", call(2)), s)
			d := wantDecision(t, f.buf, egress.Deny, ruleSourceMCPApprovalSent)
			if d.ApprovalID == nil || *d.ApprovalID != s.id || cp.raises() != 2 || f.up.hitCount() != hits {
				t.Fatalf("sent row %+v, raises %d, upstream hits %d; want it naming %s, two raises and no new send", d, cp.raises(), f.up.hitCount(), s.id)
			}
		})
	}
	t.Run("recorded by another proxy first", func(t *testing.T) {
		f, cp := newOpFixture(t)
		wantText(t, f.post(t, mcpToken("codex-cli"), "linear", call(1)), "waiting for a person")
		other := opSend{id: uuid.New(), sha: opCallKey(t, `{"id":"ENG-1"}`), at: time.Now().UTC().Add(-time.Hour).Truncate(time.Second), answered: true}
		cp.seed("step-1", other)
		cp.set(0, types.ApprovalApproved)
		wantSentText(t, f.post(t, mcpToken("codex-cli"), "linear", call(2)), other)
		d := wantDecision(t, f.buf, egress.Deny, ruleSourceMCPApprovalSent)
		if d.ApprovalID == nil || *d.ApprovalID != other.id || f.up.hitCount() != 0 || len(cp.records()) != 1 {
			t.Fatalf("sent row %+v, upstream hits %d, records %+v; want it naming %s after one refused record and no send",
				d, f.up.hitCount(), cp.records(), other.id)
		}
	})
}

// TestMCPOperationKeyReusedForAnotherCallIsRefused: a key already sent with one call refuses another call
// carrying it, unasked and unsent, at the raise and at the send record alike.
func TestMCPOperationKeyReusedForAnotherCallIsRefused(t *testing.T) {
	f, cp := newOpFixture(t)
	cp.approve = true
	if rec := f.post(t, mcpToken("codex-cli"), "linear", keyedCall(1, `{"id":"ENG-1"}`, `"step-1"`)); rec.Code != http.StatusOK {
		t.Fatalf("first call = %d %q, want sent", rec.Code, rec.Body.String())
	}
	wantReused := func(rec *httptest.ResponseRecorder, at string) {
		t.Helper()
		if got := toolResultText(t, rec); got != mcpCallKeyReusedText {
			t.Fatalf("%s: text %q, want the reused text", at, got)
		}
		if d := wantDecision(t, f.buf, egress.Deny, ruleSourceMCPApprovalRefused); d.Cause != "an operation key already used for another call" || d.ApprovalID != nil {
			t.Fatalf("%s: refused row %+v, want the reused cause and no approval", at, d)
		}
		if f.up.hitCount() != 1 {
			t.Fatalf("%s: upstream hits %d, want only the first call's", at, f.up.hitCount())
		}
	}
	wantReused(f.post(t, mcpToken("codex-cli"), "linear", keyedCall(2, `{"id":"ENG-2"}`, `"step-1"`)), "the raise")
	cp.mu.Lock()
	cp.approve = false
	cp.mu.Unlock()
	wantText(t, f.post(t, mcpToken("codex-cli"), "linear", keyedCall(3, `{"id":"ENG-3"}`, `"step-2"`)), "waiting for a person")
	cp.seed("step-2", opSend{id: uuid.New(), sha: opCallKey(t, `{"id":"ENG-4"}`), at: time.Now().UTC().Truncate(time.Second)})
	cp.set(1, types.ApprovalApproved)
	wantReused(f.post(t, mcpToken("codex-cli"), "linear", keyedCall(4, `{"id":"ENG-3"}`, `"step-2"`)), "the send record")
}

// TestMCPKeylessCallPostsNoSendRecord: a call without an operation key, with a _meta or without one, is
// raised and sent as before and posts no send record.
func TestMCPKeylessCallPostsNoSendRecord(t *testing.T) {
	f, cp := newOpFixture(t)
	cp.approve = true
	for i, body := range []string{
		eachCall(1, "delete_issue", `{"id":"ENG-1"}`),
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"delete_issue","arguments":{"id":"ENG-2"},"_meta":{"progressToken":"pt-2"}}}`,
	} {
		if rec := f.post(t, mcpToken("codex-cli"), "linear", body); rec.Code != http.StatusOK || f.up.hitCount() != i+1 {
			t.Fatalf("call %d: answer %d %q, upstream hits %d", i+1, rec.Code, rec.Body.String(), f.up.hitCount())
		}
	}
	if rows, _ := cp.snapshot(); len(cp.records()) != 0 || cp.raises() != 2 || len(rows) != 2 || rows[0].scope.OperationKey != "" {
		t.Fatalf("send records %+v, raises %d, rows %+v; want no record and two keyless raises", cp.records(), cp.raises(), rows)
	}
}

// TestMCPOperationKeysEachHaveTheirOwnApproval: one call under two operation keys is two operations, each
// asked about and sent under an approval of its own.
func TestMCPOperationKeysEachHaveTheirOwnApproval(t *testing.T) {
	f, cp := newOpFixture(t)
	keys := []string{`"step-1"`, `"step-2"`}
	for i, key := range keys {
		wantText(t, f.post(t, mcpToken("codex-cli"), "linear", keyedCall(i+1, `{"id":"ENG-1"}`, key)), "waiting for a person")
	}
	rows, _ := cp.snapshot()
	if len(rows) != 2 || rows[0].scope.OperationKey != "step-1" || rows[1].scope.OperationKey != "step-2" {
		t.Fatalf("rows %+v, want one per operation key", rows)
	}
	cp.set(0, types.ApprovalApproved)
	cp.set(1, types.ApprovalApproved)
	for i, key := range keys {
		if rec := f.post(t, mcpToken("codex-cli"), "linear", keyedCall(3+i, `{"id":"ENG-1"}`, key)); rec.Code != http.StatusOK || f.up.hitCount() != i+1 {
			t.Fatalf("%s: answer %d %q, upstream hits %d", key, rec.Code, rec.Body.String(), f.up.hitCount())
		}
		if d := findDecision(t, f.buf, ruleSourceMCPApproved); d.ApprovalID == nil || *d.ApprovalID != rows[i].id {
			t.Fatalf("%s: sent under %v, want %s", key, d.ApprovalID, rows[i].id)
		}
	}
}

// TestMCPOperationKeyDeclineHoldsForTheCall: a decline holds for the call, not its operation key, so the
// same call under a new key, or under none, is refused for the run without asking, at the broker and at
// the gate; other arguments still ask.
func TestMCPOperationKeyDeclineHoldsForTheCall(t *testing.T) {
	f, cp := newOpFixture(t)
	wantText(t, f.post(t, mcpToken("codex-cli"), "linear", keyedCall(1, `{"id":"ENG-1"}`, `"a"`)), "waiting for a person")
	cp.set(0, types.ApprovalDenied)
	wantText(t, f.post(t, mcpToken("codex-cli"), "linear", keyedCall(2, `{"id":"ENG-1"}`, `"a"`)), "A person declined this call")
	wantText(t, f.post(t, mcpToken("codex-cli"), "linear", keyedCall(3, `{"id":"ENG-1"}`, `"b"`)), "identical retries are refused")
	wantText(t, f.post(t, mcpToken("codex-cli"), "linear", eachCall(4, "delete_issue", `{"id":"ENG-1"}`)), "identical retries are refused")
	wantGateDecision(t, gatePost(t, f, "mcp__linear__delete_issue", `{"id":"ENG-1"}`), "DENIED")
	rows, _ := cp.snapshot()
	if len(rows) != 1 || f.up.hitCount() != 0 {
		t.Fatalf("rows %d, upstream hits %d; want the declined call not asked about again and nothing sent", len(rows), f.up.hitCount())
	}
	if d := findDecision(t, f.buf, "policy:tool-approve-each-declined"); d.Decision != egress.Deny || d.ApprovalID == nil || *d.ApprovalID != rows[0].id {
		t.Fatalf("gate decision %+v, want a deny naming the declined row", d)
	}
	wantText(t, f.post(t, mcpToken("codex-cli"), "linear", keyedCall(5, `{"id":"ENG-2"}`, `"b"`)), "waiting for a person")
	if rows, _ := cp.snapshot(); len(rows) != 2 {
		t.Fatalf("rows %d, want other arguments to ask", len(rows))
	}
}

// TestMCPOperationKeyUnresolvedServerIsNotRecorded: an approved keyed call whose server's address does
// not resolve is refused before its send is recorded, so no record stands for a call never sent, and the
// same call under the same key asks again and is sent once the address resolves.
func TestMCPOperationKeyUnresolvedServerIsNotRecorded(t *testing.T) {
	res := &toggleResolver{err: errors.New("no such host")}
	f := newMCPFixture(t, types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "mcp__linear__delete_issue", Effect: types.ToolApproveEach}}}, res, nil)
	f.p.mcp.setApproveEach(&MCPApproveEachConfig{})
	cp := newOpCP(t, f.up.hitCount)
	(&eachCP{srv: cp.srv}).wire(t, f.p)
	cp.approve = true
	if rec := f.post(t, mcpToken("codex-cli"), "linear", keyedCall(1, `{"id":"ENG-1"}`, `"step-1"`)); rec.Code == http.StatusOK || f.up.hitCount() != 0 {
		t.Fatalf("answer %d %q, upstream hits %d; want a refusal and no send", rec.Code, rec.Body.String(), f.up.hitCount())
	}
	if got := cp.records(); len(got) != 0 {
		t.Fatalf("send records %+v, want none for a call that was not sent", got)
	}
	wantDecision(t, f.buf, egress.Deny, "builtin:resolve-failed")
	res.err, res.ips = nil, []net.IP{net.ParseIP("93.184.216.34")}
	if rec := f.post(t, mcpToken("codex-cli"), "linear", keyedCall(2, `{"id":"ENG-1"}`, `"step-1"`)); rec.Code != http.StatusOK || f.up.hitCount() != 1 {
		t.Fatalf("answer %d %q, upstream hits %d; want the call asked again and sent", rec.Code, rec.Body.String(), f.up.hitCount())
	}
	rows, _ := cp.snapshot()
	if got := cp.records(); len(rows) != 2 || len(got) != 2 || got[0].id != rows[1].id {
		t.Fatalf("rows %d, send records %+v; want a second approval and its record only", len(rows), got)
	}
}
