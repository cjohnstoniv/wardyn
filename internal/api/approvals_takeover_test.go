// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// takeoverApprovals adds the human_takeover CAS (store.PG.TakeOverApproval/HandBackApproval,
// pinned against Postgres in internal/store) to the authz approvals double.
type takeoverApprovals struct{ *authzApprovals }

func (a takeoverApprovals) TakeOver(_ context.Context, id uuid.UUID, holder string) (types.ApprovalRequest, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ap, ok := a.byID[id]
	if !ok {
		return ap, "", store.ErrNotFound
	}
	open := ap.State == types.ApprovalPending || (ap.State == types.ApprovalApproved && ap.HandedBackAt == nil)
	if ap.Kind != types.ApprovalHumanTakeover || !open || ap.TakeoverHolder == holder {
		return ap, "", store.ErrAlreadyDecided
	}
	prev := ap.TakeoverHolder
	if ap.State == types.ApprovalPending {
		now := time.Now().UTC()
		ap.State, ap.DecidedAt, ap.DecidedBy = types.ApprovalApproved, &now, holder
	}
	ap.TakeoverHolder = holder
	a.byID[id] = ap
	return ap, prev, nil
}

func (a takeoverApprovals) HandBack(_ context.Context, id uuid.UUID, holder string, at time.Time) (types.ApprovalRequest, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ap, ok := a.byID[id]
	if !ok {
		return ap, store.ErrNotFound
	}
	if ap.Kind != types.ApprovalHumanTakeover || ap.State != types.ApprovalApproved || ap.HandedBackAt != nil {
		return ap, store.ErrAlreadyDecided
	}
	if ap.TakeoverHolder != holder {
		return ap, store.ErrTakeoverNotHolder
	}
	ap.HandedBackAt = &at
	a.byID[id] = ap
	return ap, nil
}

const takeoverOwnerSub = "sub-takeover-owner"

type takeoverFixture struct {
	srv   *Server
	aap   takeoverApprovals
	audit *recRecorder
	owner *http.Cookie
	run   uuid.UUID
}

// newTakeoverFixture is a server with a RUNNING run owned by a member, recorded when recorded is set.
func newTakeoverFixture(t *testing.T, recorded bool) takeoverFixture {
	t.Helper()
	ast := newAuthzStore()
	aap := takeoverApprovals{newAuthzApprovals(ast)}
	h := newHarness(t)
	cfg := baseTestConfig(h, ast)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Approvals = aap
	if recorded {
		casts, err := recording.NewFSStore(t.TempDir())
		if err != nil {
			t.Fatalf("NewFSStore: %v", err)
		}
		cfg.RecordingStore = casts
	}
	srv := New(cfg)
	runID := uuid.New()
	ast.mu.Lock()
	ast.runs[runID] = types.AgentRun{ID: runID, CreatedBy: takeoverOwnerSub, State: types.RunRunning, SandboxRef: "sbx-1"}
	ast.mu.Unlock()
	return takeoverFixture{srv: srv, aap: aap, audit: h.audit,
		owner: ssoSession(t, takeoverOwnerSub, "owner@corp.example", oidc.RoleUser), run: runID}
}

func (f takeoverFixture) seed(kind types.ApprovalKind, state types.ApprovalState, holder string) uuid.UUID {
	id := uuid.New()
	f.aap.mu.Lock()
	defer f.aap.mu.Unlock()
	f.aap.byID[id] = types.ApprovalRequest{ID: id, RunID: f.run, Kind: kind, State: state, TakeoverHolder: holder,
		RequestedScope: json.RawMessage(`{"reason":"the sign-in sends a code to a phone"}`), RequestedAt: time.Now().UTC().Add(-2 * time.Minute)}
	return id
}

// takeoverRows is every takeover.* and recording.* row, in order: action and its data keys.
func takeoverRows(t *testing.T, a *recRecorder) [][2]string {
	t.Helper()
	var out [][2]string
	for _, ev := range a.snapshot() {
		if !strings.HasPrefix(ev.Action, "takeover.") && !strings.HasPrefix(ev.Action, "recording.") {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("%s data: %v", ev.Action, err)
		}
		keys := make([]string, 0, len(data))
		for k := range data {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		out = append(out, [2]string{ev.Action + "/" + string(ev.ActorType) + ":" + ev.Actor, strings.Join(keys, ",")})
	}
	return out
}

// TestTakeover_RaiseWritesTheRequestRow: the agent's raise (through the proxy, run token) is stored
// with its canonical {reason} scope and audited as takeover.request; a scope with anything else is
// refused before anything is stored.
func TestTakeover_RaiseWritesTheRequestRow(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)
	for _, bad := range []string{`{"reason":"   "}`, `{"reason":"x","cmd":"rm -rf /"}`, `{"reason":"` + strings.Repeat("y", types.MaxTakeoverReason+40) + `"}`} {
		w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals", tok, `{"kind":"human_takeover","requested_scope":`+bad+`}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("scope %.40s: status = %d, want 400", bad, w.Code)
		}
	}
	if len(h.approvals.requested) != 0 {
		t.Fatal("a refused takeover scope was stored")
	}
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals", tok,
		`{"kind":"human_takeover","requested_scope":{"reason":"  I can't finish the sign-in  "}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("raise: status = %d body=%s", w.Code, w.Body.String())
	}
	got := h.approvals.requested[0]
	if got.Kind != types.ApprovalHumanTakeover || got.RunID != runID || string(got.RequestedScope) != `{"reason":"I can't finish the sign-in"}` {
		t.Fatalf("stored %+v scope=%s", got, got.RequestedScope)
	}
	rows := takeoverRows(t, h.audit)
	if len(rows) != 1 || !strings.HasPrefix(rows[0][0], "takeover.request/agent:") || rows[0][1] != "approval_id,reason" {
		t.Fatalf("audit rows = %v, want one takeover.request by the agent with approval_id,reason", rows)
	}
}

// TestTakeover_TakeAndHandBackAuditInOrder pins the five rows of one takeover on a recorded run
// (mock M8-HandBack): request is the raise's; then takeover.begin, recording.pause, recording.resume,
// takeover.close, each with a closed set of keys, so nothing typed, no clipboard and no screen can
// ride along.
func TestTakeover_TakeAndHandBackAuditInOrder(t *testing.T) {
	f := newTakeoverFixture(t, true)
	id := f.seed(types.ApprovalHumanTakeover, types.ApprovalPending, "")
	w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/take-over", f.owner, "")
	if w.Code != http.StatusOK {
		t.Fatalf("take over: %d %s", w.Code, w.Body.String())
	}
	var took takeoverResponse
	if err := json.Unmarshal(w.Body.Bytes(), &took); err != nil || took.State != types.ApprovalApproved ||
		took.TakeoverHolder != takeoverOwnerSub || took.Surface != "terminal" {
		t.Fatalf("take over answer: %+v %v", took, err)
	}
	w = doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/hand-back", f.owner, "")
	if w.Code != http.StatusOK {
		t.Fatalf("hand back: %d %s", w.Code, w.Body.String())
	}
	want := [][2]string{
		{"takeover.begin/human:" + takeoverOwnerSub, "after_ms,approval_id,surface"},
		{"recording.pause/system:wardyn", "approval_id,reason"},
		{"recording.resume/system:wardyn", "approval_id,gap_ms,reason"},
		{"takeover.close/human:" + takeoverOwnerSub, "approval_id,held_ms"},
	}
	if got := takeoverRows(t, f.audit); !slices.Equal(got, want) {
		t.Fatalf("audit rows =\n%v\nwant\n%v", got, want)
	}
	if w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/hand-back", f.owner, ""); w.Code != http.StatusConflict {
		t.Fatalf("second hand back: %d, want 409", w.Code)
	}
}

// TestTakeover_UnrecordedRunHasNothingToPause (mock M8-States 10): no recording, no pause rows.
func TestTakeover_UnrecordedRunHasNothingToPause(t *testing.T) {
	f := newTakeoverFixture(t, false)
	id := f.seed(types.ApprovalHumanTakeover, types.ApprovalPending, "")
	for _, verb := range []string{"/take-over", "/hand-back"} {
		if w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+verb, f.owner, ""); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", verb, w.Code, w.Body.String())
		}
	}
	for _, r := range takeoverRows(t, f.audit) {
		if strings.HasPrefix(r[0], "recording.") {
			t.Fatalf("an unrecorded run wrote %v", r)
		}
	}
}

// TestTakeover_Refusals: a takeover request is never approved or denied (every tier), the verbs
// refuse another kind and a stranger, only the holder hands back, and decline is a recorded DENIED.
func TestTakeover_Refusals(t *testing.T) {
	f := newTakeoverFixture(t, true)
	id := f.seed(types.ApprovalHumanTakeover, types.ApprovalPending, "")
	for _, verb := range []string{"/approve", "/deny"} {
		w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+verb, f.owner, "")
		if w.Code != http.StatusConflict || reasonOf(t, w) != reasonTakeoverNotDecidable {
			t.Errorf("owner %s: %d %s, want 409 %s", verb, w.Code, w.Body.String(), reasonTakeoverNotDecidable)
		}
		w = do(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+verb, adminToken, "")
		if w.Code != http.StatusConflict || reasonOf(t, w) != reasonTakeoverNotDecidable {
			t.Errorf("admin token %s: %d %s, want 409 %s", verb, w.Code, w.Body.String(), reasonTakeoverNotDecidable)
		}
	}
	// A stranger's answer is the byte-identical 404 a missing approval gets: no oracle on the id.
	stranger := ssoSession(t, "sub-stranger", "stranger@corp.example", oidc.RoleUser)
	for _, verb := range []string{"/take-over", "/hand-back", "/decline"} {
		w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+verb, stranger, "")
		missing := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+uuid.New().String()+verb, stranger, "")
		if w.Code != http.StatusNotFound || missing.Code != http.StatusNotFound || w.Body.String() != missing.Body.String() {
			t.Errorf("stranger %s: %d %q, want the missing approval's %d %q", verb, w.Code, w.Body.String(), missing.Code, missing.Body.String())
		}
	}
	tool := f.seed(types.ApprovalToolCall, types.ApprovalPending, "")
	if w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+tool.String()+"/take-over", f.owner, ""); w.Code != http.StatusConflict ||
		reasonOf(t, w) != reasonNotATakeoverRequest {
		t.Errorf("take over a tool_call: %d %s", w.Code, w.Body.String())
	}
	held := f.seed(types.ApprovalHumanTakeover, types.ApprovalApproved, "someone-else")
	if w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+held.String()+"/hand-back", f.owner, ""); w.Code != http.StatusConflict ||
		reasonOf(t, w) != reasonTakeoverNotHolder {
		t.Errorf("hand back by a non-holder: %d %s", w.Code, w.Body.String())
	}
	w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/decline", f.owner, "")
	var got types.ApprovalRequest
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.State != types.ApprovalDenied || got.DecidedBy != takeoverOwnerSub {
		t.Fatalf("decline: %d %s", w.Code, w.Body.String())
	}
	if w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/take-over", f.owner, ""); w.Code != http.StatusConflict {
		t.Fatalf("take over a declined request: %d, want 409", w.Code)
	}
}

// TestTakeover_InputIsNotTeedToTheAttachRecorder: what the person types into the console terminal
// while they drive is not teed to the console's per-attach session recorder; only what the terminal
// shows is. The boot seed's cast is wardyn-rec's (cmd/wardyn-rec TestRec_TypedInputIsNotRecorded).
func TestTakeover_InputIsNotTeedToTheAttachRecorder(t *testing.T) {
	srv, fr, _, run, ts, returned := attachSlotsTestServer(t)
	dir := t.TempDir()
	casts, err := recording.NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	srv.cfg.RecordingStore = casts
	c := dialAttach(t, ts, srv, run.ID, holderOwner, "")
	readAttachMode(t, c)
	sess := waitForSession(t, fr, 0)
	const typed = "hunter2-typed-by-the-person"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageBinary, []byte(typed+"\r")); err != nil {
		t.Fatalf("type: %v", err)
	}
	waitFor(t, "the keystrokes to reach the terminal", func() bool {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		for _, w := range sess.writes {
			if strings.Contains(string(w), typed) {
				return true
			}
		}
		return false
	})
	sess.feed("Code: shown-on-screen\r\n")
	if _, _, err := c.Read(ctx); err != nil {
		t.Fatalf("read output: %v", err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
	recvSlotEvent(t, returned, "the attach handler to return")
	var cast strings.Builder
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			cast.Write(b)
		}
		return nil
	})
	if !strings.Contains(cast.String(), "shown-on-screen") {
		t.Fatalf("no recording of the session was written (%d bytes)", cast.Len())
	}
	if strings.Contains(cast.String(), typed) {
		t.Fatal("the person's keystrokes were teed to the per-attach recording")
	}
}

// TestAttention_TakeoverLeadsAndIsTheOwners: a PENDING takeover request puts its run under
// "Needs you" as kind takeover, ahead of a held approval on the same run, and is the owner's to act on.
func TestAttention_TakeoverLeadsAndIsTheOwners(t *testing.T) {
	s := attentionFixture(t)
	runID := uuid.New()
	run := types.AgentRun{ID: runID, State: types.RunRunning, CreatedBy: "sub-owner"}
	pending := []types.ApprovalRequest{
		pendingRow(runID, types.ApprovalToolCall, `{"tool":"Bash","cmd":"ls"}`, nil),
		pendingRow(runID, types.ApprovalHumanTakeover, `{"reason":"x"}`, nil),
	}
	s.projectHolds(pending)
	owner := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil).WithContext(memberCtxFor("sub-owner"))
	if got := s.attention(owner, run, pending, false); got == nil || got.Kind != types.AttentionTakeover || got.By != types.AttentionYou {
		t.Fatalf("owner: attention = %+v, want takeover by you", got)
	}
	admin := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil).WithContext(adminCtx())
	if got := s.heldCandidates(admin, run, pending, true); len(got) == 0 || got[0].kind != types.AttentionTakeover || got[0].by != types.AttentionOwner {
		t.Fatalf("admin view: candidates = %+v, want takeover by owner first", got)
	}
}
