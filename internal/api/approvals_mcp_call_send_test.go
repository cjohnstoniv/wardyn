// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sendStore is the authz fake store with the send record kept in memory as store.MCPCallSendStore keeps
// it: one row per namespace and key, never replaced.
type sendStore struct {
	*authzStore
	smu   sync.Mutex
	recs  map[[3]string]store.MCPCallSend
	reads int // GetMCPCallSend calls
}

var _ store.MCPCallSendStore = (*sendStore)(nil)

func (s *sendStore) RecordMCPCallSend(_ context.Context, rec store.MCPCallSend) (store.MCPCallSend, bool, error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	k := [3]string{rec.Owner, rec.OperationScope, rec.OperationKey}
	if got, ok := s.recs[k]; ok {
		return got, false, nil
	}
	rec.SentAt = time.Now().UTC()
	s.recs[k] = rec
	return rec, true, nil
}

func (s *sendStore) GetMCPCallSend(_ context.Context, owner, scope, key string) (store.MCPCallSend, error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	s.reads++
	got, ok := s.recs[[3]string{owner, scope, key}]
	if !ok {
		return store.MCPCallSend{}, store.ErrNotFound
	}
	return got, nil
}

func (s *sendStore) MarkMCPCallAnswered(_ context.Context, approvalID, runID uuid.UUID) (bool, error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	for k, rec := range s.recs {
		if rec.ApprovalID == approvalID && rec.RunID == runID {
			if rec.AnsweredAt == nil {
				now := time.Now().UTC()
				rec.AnsweredAt = &now
				s.recs[k] = rec
			}
			return true, nil
		}
	}
	return false, nil
}

func (s *sendStore) count() (reads, recs int) {
	s.smu.Lock()
	defer s.smu.Unlock()
	return s.reads, len(s.recs)
}

// sendFixture is a server whose store keeps send records.
type sendFixture struct {
	h   *harness
	srv *Server
	st  *sendStore
	aap *authzApprovals
}

func newSendFixture(t *testing.T) sendFixture {
	t.Helper()
	st := &sendStore{authzStore: newAuthzStore(), recs: map[[3]string]store.MCPCallSend{}}
	aap := newAuthzApprovals(st.authzStore)
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.Approvals = aap
	return sendFixture{h: h, srv: New(cfg), st: st, aap: aap}
}

// run adds a RUNNING run of owner with correlation ID corr ("" for none) and returns its id and token.
func (f sendFixture) run(t *testing.T, owner, corr string) (uuid.UUID, string) {
	t.Helper()
	id := uuid.New()
	f.st.mu.Lock()
	f.st.runs[id] = types.AgentRun{ID: id, CreatedBy: owner, CorrelationID: corr, State: types.RunRunning, SandboxRef: "sbx-1"}
	f.st.mu.Unlock()
	return id, f.h.mintRunToken(t, id)
}

// keyedScope is the scope the run proxy raises for jira's delete_issue with args, carrying operation key
// key ("" for none).
func keyedScope(t *testing.T, args, key string) string {
	t.Helper()
	var sc types.MCPCallScope
	if err := json.Unmarshal(mcpCallScopeJSON(t, "jira", "delete_issue", args), &sc); err != nil {
		t.Fatal(err)
	}
	sc.OperationKey = key
	raw, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func (f sendFixture) raise(t *testing.T, tok, scope string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, f.srv, http.MethodPost, "/api/v1/internal/approvals", tok, `{"kind":"tool_call","requested_scope":`+scope+`}`)
}

func (f sendFixture) sent(t *testing.T, tok string, id uuid.UUID, answered bool) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, f.srv, http.MethodPost, "/api/v1/internal/approvals/"+id.String()+"/sent", tok, fmt.Sprintf(`{"answered":%t}`, answered))
}

// approved raises scope on tok's run and approves the new row, as a person's once decision leaves it.
func (f sendFixture) approved(t *testing.T, tok, scope string) uuid.UUID {
	t.Helper()
	w := f.raise(t, tok, scope)
	var ap types.ApprovalRequest
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &ap) != nil {
		t.Fatalf("raise = %d %s, want 201", w.Code, w.Body.String())
	}
	f.set(ap.ID, types.ApprovalApproved)
	return ap.ID
}

func (f sendFixture) set(id uuid.UUID, state types.ApprovalState) {
	f.aap.mu.Lock()
	defer f.aap.mu.Unlock()
	row := f.aap.byID[id]
	row.State = state
	f.aap.byID[id] = row
}

func (f sendFixture) rows() int {
	f.aap.mu.Lock()
	defer f.aap.mu.Unlock()
	return len(f.aap.byID)
}

// sentAnswer decodes a send answer whose status must be want.
func sentAnswer(t *testing.T, w *httptest.ResponseRecorder, want int) types.MCPCallSentAnswer {
	t.Helper()
	var a types.MCPCallSentAnswer
	if w.Code != want || json.Unmarshal(w.Body.Bytes(), &a) != nil {
		t.Fatalf("answer = %d %s, want %d", w.Code, w.Body.String(), want)
	}
	return a
}

// TestKeylessMCPCallRaiseReadsNoSendRecord: a call with no operation key is raised as before and reads no
// send record; its stored scope carries no operation_key.
func TestKeylessMCPCallRaiseReadsNoSendRecord(t *testing.T) {
	f := newSendFixture(t)
	_, tok := f.run(t, "alice", "job-7")
	w := f.raise(t, tok, keyedScope(t, `{"id":1}`, ""))
	if reads, _ := f.st.count(); w.Code != http.StatusCreated || reads != 0 || strings.Contains(w.Body.String(), "operation_key") {
		t.Fatalf("keyless raise = %d %s with %d send reads; want 201, no read and no operation_key", w.Code, w.Body.String(), reads)
	}
}

// TestMadeKeyOfItsOwnCallIsRaised: a key of the made form that is the call's own (its hash and a positive
// occurrence) is raised as any key is.
func TestMadeKeyOfItsOwnCallIsRaised(t *testing.T) {
	f := newSendFixture(t)
	_, tok := f.run(t, "alice", "job-7")
	var sc types.MCPCallScope
	if err := json.Unmarshal([]byte(keyedScope(t, `{"id":1}`, "")), &sc); err != nil {
		t.Fatal(err)
	}
	if w := f.raise(t, tok, keyedScope(t, `{"id":1}`, "call:"+sc.CallSHA256+":1")); w.Code != http.StatusCreated {
		t.Fatalf("raise under the call's own made key = %d %s, want 201", w.Code, w.Body.String())
	}
}

// TestKeyedMCPCallIsSentOnceInItsNamespace: the first send record of a key is a 200 naming its approval; a
// second record, the same approval's own included, is a 409 naming the first; a raise of the same call with
// the key is answered 409 with that send and raises nothing, and says answered once the stamp is in.
func TestKeyedMCPCallIsSentOnceInItsNamespace(t *testing.T) {
	f := newSendFixture(t)
	_, tok := f.run(t, "alice", "job-7")
	scope := keyedScope(t, `{"id":1}`, "step-1")
	id := f.approved(t, tok, scope)
	if a := sentAnswer(t, f.sent(t, tok, id, false), http.StatusOK); a.ApprovalID != id || a.SentAt.IsZero() {
		t.Fatalf("first record = %+v, want approval %s and a time", a, id)
	}
	if a := sentAnswer(t, f.sent(t, tok, id, false), http.StatusConflict); a.Reason != reasonOperationAlreadySent || a.ApprovalID != id {
		t.Fatalf("second record = %+v, want %s naming %s", a, reasonOperationAlreadySent, id)
	}
	rows := f.rows()
	if a := sentAnswer(t, f.raise(t, tok, scope), http.StatusConflict); a.Reason != reasonOperationAlreadySent || a.ApprovalID != id || a.Answered || f.rows() != rows {
		t.Fatalf("raise after the send = %+v, %d rows (was %d); want %s naming %s, unanswered, nothing raised", a, f.rows(), rows, reasonOperationAlreadySent, id)
	}
	if w := f.sent(t, tok, id, true); w.Code != http.StatusNoContent {
		t.Fatalf("stamp = %d %s, want 204", w.Code, w.Body.String())
	}
	if a := sentAnswer(t, f.raise(t, tok, scope), http.StatusConflict); !a.Answered || a.ApprovalID != id {
		t.Fatalf("raise after the stamp = %+v, want answered, naming %s", a, id)
	}
}

// TestOperationNamespaceIsTheRunRows: the namespace is the owner and correlation ID of the run row the
// token names, never anything the proxy sends. A run of the same owner and correlation ID shares it;
// another owner's run, or a run with no correlation ID, has its own; a record body naming anything but
// answered is refused.
func TestOperationNamespaceIsTheRunRows(t *testing.T) {
	f := newSendFixture(t)
	scope := keyedScope(t, `{"id":1}`, "step-1")
	_, a := f.run(t, "alice", "job-7")
	_, sameCorr := f.run(t, "alice", "job-7")
	_, otherOwner := f.run(t, "bob", "job-7")
	_, noCorr := f.run(t, "alice", "")
	_, noCorrToo := f.run(t, "alice", "")
	if w := do(t, f.srv, http.MethodPost, "/api/v1/internal/approvals/"+uuid.NewString()+"/sent", a, `{"answered":false,"owner":"bob"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a record naming an owner = %d %s, want 400", w.Code, w.Body.String())
	}
	id := f.approved(t, a, scope)
	sentAnswer(t, f.sent(t, a, id, false), http.StatusOK)
	if got := sentAnswer(t, f.raise(t, sameCorr, scope), http.StatusConflict); got.ApprovalID != id {
		t.Fatalf("same owner and correlation ID = %+v, want the send under %s", got, id)
	}
	for name, tok := range map[string]string{"another owner": otherOwner, "no correlation ID": noCorr} {
		if w := f.raise(t, tok, scope); w.Code != http.StatusCreated {
			t.Fatalf("%s: raise = %d %s, want 201 (a namespace of its own)", name, w.Code, w.Body.String())
		}
	}
	own := f.approved(t, noCorr, scope)
	sentAnswer(t, f.sent(t, noCorr, own, false), http.StatusOK)
	if w := f.raise(t, noCorrToo, scope); w.Code != http.StatusCreated {
		t.Fatalf("another run with no correlation ID: raise = %d %s, want 201", w.Code, w.Body.String())
	}
	if got := sentAnswer(t, f.raise(t, noCorr, scope), http.StatusConflict); got.ApprovalID != own {
		t.Fatalf("the same run again = %+v, want the send under %s", got, own)
	}
}

// TestOperationKeyReusedWithAnotherCallIsRefused: a key sent with one call answers another call's raise,
// and another call's send record, 409 operation_key_reused, naming nothing of the first.
func TestOperationKeyReusedWithAnotherCallIsRefused(t *testing.T) {
	f := newSendFixture(t)
	_, tok := f.run(t, "alice", "job-7")
	first := f.approved(t, tok, keyedScope(t, `{"id":1}`, "step-1"))
	other := f.approved(t, tok, keyedScope(t, `{"id":2}`, "step-1")) // raised before the first was sent
	sentAnswer(t, f.sent(t, tok, first, false), http.StatusOK)
	for name, w := range map[string]*httptest.ResponseRecorder{
		"raise":  f.raise(t, tok, keyedScope(t, `{"id":2}`, "step-1")),
		"record": f.sent(t, tok, other, false),
	} {
		if a := sentAnswer(t, w, http.StatusConflict); a.Reason != reasonOperationKeyReused || a.ApprovalID != uuid.Nil || strings.Contains(w.Body.String(), first.String()) {
			t.Fatalf("%s = %d %s, want 409 %s naming nothing", name, w.Code, w.Body.String(), reasonOperationKeyReused)
		}
	}
}

// TestOnlyAnApprovedKeyedMCPCallOfTheRunRecords: a send is recorded only for an APPROVED MCP call row of
// the token's own run that carries an operation key; every other approval is 404 and records nothing.
func TestOnlyAnApprovedKeyedMCPCallOfTheRunRecords(t *testing.T) {
	f := newSendFixture(t)
	runID, tok := f.run(t, "alice", "job-7")
	_, stranger := f.run(t, "alice", "job-7")
	keyed := keyedScope(t, `{"id":1}`, "step-1")
	pending := f.approved(t, tok, keyed)
	f.set(pending, types.ApprovalPending)
	denied := f.approved(t, tok, keyed)
	f.set(denied, types.ApprovalDenied)
	approved := f.approved(t, tok, keyed)
	keyless := f.approved(t, tok, keyedScope(t, `{"id":1}`, ""))
	egress := f.aap.seed(runID)
	f.set(egress, types.ApprovalApproved)
	for name, c := range map[string]struct {
		tok string
		id  uuid.UUID
	}{
		"pending":          {tok, pending},
		"denied":           {tok, denied},
		"another run's":    {stranger, approved},
		"keyless":          {tok, keyless},
		"not an MCP call":  {tok, egress},
		"no such approval": {tok, uuid.New()},
	} {
		if w := f.sent(t, c.tok, c.id, false); w.Code != http.StatusNotFound || errorReason(w) != reasonApprovalNotFound {
			t.Errorf("%s: record = %d %s, want 404 %s", name, w.Code, w.Body.String(), reasonApprovalNotFound)
		}
	}
	if _, recs := f.st.count(); recs != 0 {
		t.Fatalf("%d sends recorded, want none", recs)
	}
	sentAnswer(t, f.sent(t, tok, approved, false), http.StatusOK)
}

// TestMalformedOperationKeyIsRefused: a key that is not 1 to 128 of the correlation ID's characters, is not
// a string, is spelled in another case or is given twice is a bad MCP call scope: 400, an auth.fail row and
// nothing raised.
func TestMalformedOperationKeyIsRefused(t *testing.T) {
	good := keyedScope(t, `{"id":1}`, "step-1")
	for name, scope := range map[string]string{
		"a space":                       strings.Replace(good, `"step-1"`, `"step 1"`, 1),
		"129 characters":                strings.Replace(good, `"step-1"`, `"`+strings.Repeat("a", 129)+`"`, 1),
		"not a string":                  strings.Replace(good, `"step-1"`, `7`, 1),
		"another case":                  strings.Replace(good, `"operation_key"`, `"Operation_Key"`, 1),
		"a made key of another call":    strings.Replace(good, `"step-1"`, `"call:`+strings.Repeat("0", 64)+`:1"`, 1),
		"a made key with no occurrence": strings.Replace(good, `"step-1"`, `"call:"`, 1),
		"twice":                         strings.Replace(good, `"operation_key":"step-1"`, `"operation_key":"step-1","operation_key":"step-2"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			raiseMCPRefused(t, types.ApprovalToolCall, scope, reasonMCPCallScopeInvalid)
		})
	}
}

// TestKeyedMCPCallWithoutASendRecordFailsClosed: with no store, or a store that keeps no send record, a
// keyed call is neither raised nor recorded (500); a keyless one is raised as before.
func TestKeyedMCPCallWithoutASendRecordFailsClosed(t *testing.T) {
	h := newHarness(t)
	tok := h.mintRunToken(t, uuid.New())
	raise := func(srv *Server, tok, scope string) *httptest.ResponseRecorder {
		return do(t, srv, http.MethodPost, "/api/v1/internal/approvals", tok, `{"kind":"tool_call","requested_scope":`+scope+`}`)
	}
	if w := raise(h.srv, tok, keyedScope(t, `{"id":1}`, "step-1")); w.Code != http.StatusInternalServerError || len(h.approvals.requested) != 0 {
		t.Fatalf("no store: keyed raise = %d %s, %d rows; want 500 and nothing raised", w.Code, w.Body.String(), len(h.approvals.requested))
	}
	if w := raise(h.srv, tok, keyedScope(t, `{"id":1}`, "")); w.Code != http.StatusCreated {
		t.Fatalf("no store: keyless raise = %d %s, want 201", w.Code, w.Body.String())
	}
	ast := newAuthzStore() // a store that keeps no send record
	aap := newAuthzApprovals(ast)
	cfg := baseTestConfig(h, ast)
	cfg.Approvals = aap
	srv := New(cfg)
	runID := uuid.New()
	ast.mu.Lock()
	ast.runs[runID] = types.AgentRun{ID: runID, CreatedBy: "alice", State: types.RunRunning, SandboxRef: "sbx-1"}
	ast.mu.Unlock()
	tok = h.mintRunToken(t, runID)
	if w := raise(srv, tok, keyedScope(t, `{"id":1}`, "step-1")); w.Code != http.StatusInternalServerError {
		t.Fatalf("no send record: keyed raise = %d %s, want 500", w.Code, w.Body.String())
	}
	id := uuid.New()
	aap.mu.Lock()
	aap.byID[id] = types.ApprovalRequest{ID: id, RunID: runID, Kind: types.ApprovalToolCall, State: types.ApprovalApproved,
		RequestedScope: json.RawMessage(keyedScope(t, `{"id":1}`, "step-1")), RequestedAt: time.Now().UTC()}
	aap.mu.Unlock()
	if w := do(t, srv, http.MethodPost, "/api/v1/internal/approvals/"+id.String()+"/sent", tok, `{"answered":false}`); w.Code != http.StatusInternalServerError {
		t.Fatalf("no send record: record = %d %s, want 500", w.Code, w.Body.String())
	}
}

// TestOperationKeyRulesMatchTheProxys: the reasons the proxy reads are these values, and an operation key
// holds exactly the characters a run's correlation ID may, up to the same length.
func TestOperationKeyRulesMatchTheProxys(t *testing.T) {
	if reasonOperationAlreadySent != types.MCPOperationAlreadySent || reasonOperationKeyReused != types.MCPOperationKeyReused {
		t.Fatalf("reasons %q, %q; the proxy reads %q, %q", reasonOperationAlreadySent, reasonOperationKeyReused,
			types.MCPOperationAlreadySent, types.MCPOperationKeyReused)
	}
	if types.MaxOperationKeyLen != maxCorrelationIDLen {
		t.Fatalf("MaxOperationKeyLen %d, the correlation ID's limit %d", types.MaxOperationKeyLen, maxCorrelationIDLen)
	}
	for r := rune(0); r < 0x80; r++ {
		if got, want := types.ValidOperationKey(string(r)), correlationIDRune(r); got != want {
			t.Errorf("rune %q: operation key %v, correlation ID %v", r, got, want)
		}
	}
	for _, r := range []rune{'\u00e9', '\u03a9', '\u2028', 0xff} {
		if types.ValidOperationKey(string(r)) {
			t.Errorf("rune %q is an operation key", r)
		}
	}
}

// TestMCPCallScopeKeysAreTheScopesFields: mcpCallScopeKeys, which the exact-key check reads, names every json
// key of types.MCPCallScope and nothing else, so a new field is never left to encoding/json's case folding.
func TestMCPCallScopeKeysAreTheScopesFields(t *testing.T) {
	var tags []string
	st := reflect.TypeFor[types.MCPCallScope]()
	for i := range st.NumField() {
		name, _, _ := strings.Cut(st.Field(i).Tag.Get("json"), ",")
		tags = append(tags, name)
	}
	got := slices.Sorted(slices.Values(mcpCallScopeKeys))
	slices.Sort(tags)
	if !slices.Equal(got, tags) {
		t.Fatalf("mcpCallScopeKeys %v, want the scope's json keys %v", got, tags)
	}
}
