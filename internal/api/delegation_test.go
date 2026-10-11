// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fakeDelegateStore is store.DelegateStore in memory, keyed by the hash of each
// credential the way store_delegates.go is, and filtering revoked portals and
// expired tokens in the lookup the way its WHERE does. Embedded in authzStore,
// so the matrix walks the delegation routes with the capability present.
type fakeDelegateStore struct {
	mu        sync.Mutex
	delegates map[uuid.UUID]types.Delegate
	byCred    map[string]uuid.UUID
	tokens    map[string]types.DelegatedToken
}

func newFakeDelegateStore() *fakeDelegateStore {
	return &fakeDelegateStore{
		delegates: map[uuid.UUID]types.Delegate{},
		byCred:    map[string]uuid.UUID{},
		tokens:    map[string]types.DelegatedToken{},
	}
}

var _ store.DelegateStore = (*authzStore)(nil)

func (f *fakeDelegateStore) CreateDelegate(_ context.Context, d types.Delegate, raw string) (types.Delegate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := memHash(raw)
	if _, dup := f.byCred[h]; dup {
		return types.Delegate{}, store.ErrConflict
	}
	d.CredentialSHA256, d.CreatedAt = h, time.Now().UTC()
	f.delegates[d.ID], f.byCred[h] = d, d.ID
	return d, nil
}

func (f *fakeDelegateStore) GetDelegateByRaw(_ context.Context, raw string) (types.Delegate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.delegates[f.byCred[memHash(raw)]]
	if !ok || d.RevokedAt != nil {
		return types.Delegate{}, store.ErrNotFound
	}
	return d, nil
}

func (f *fakeDelegateStore) ListDelegates(context.Context) ([]types.Delegate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []types.Delegate{}
	for _, d := range f.delegates {
		out = append(out, d)
	}
	return out, nil
}

func (f *fakeDelegateStore) RevokeDelegate(_ context.Context, id uuid.UUID, now time.Time) (types.Delegate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.delegates[id]
	if !ok || d.RevokedAt != nil {
		return types.Delegate{}, store.ErrNotFound
	}
	d.RevokedAt = &now
	f.delegates[id] = d
	return d, nil
}

func (f *fakeDelegateStore) MintDelegatedToken(_ context.Context, t types.DelegatedToken, raw string, _ time.Time) (types.DelegatedToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[memHash(raw)] = t
	return t, nil
}

func (f *fakeDelegateStore) GetDelegatedTokenByRaw(_ context.Context, raw string, now time.Time) (types.DelegatedToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tokens[memHash(raw)]
	if !ok || !t.ExpiresAt.After(now) || f.delegates[t.DelegateID].RevokedAt != nil {
		return types.DelegatedToken{}, store.ErrNotFound
	}
	return t, nil
}

func (f *fakeDelegateStore) GetDelegatedTokenByID(_ context.Context, id uuid.UUID, now time.Time) (types.DelegatedToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tokens {
		if t.ID == id && t.ExpiresAt.After(now) && f.delegates[t.DelegateID].RevokedAt == nil {
			return t, nil
		}
	}
	return types.DelegatedToken{}, store.ErrNotFound
}

// seedDelegation registers a portal and hands it a live delegated token for
// principal at the given role's reach — the state a successful exchange
// leaves, without an IdP (delegation_pg_test.go drives the real exchange).
func seedDelegation(t *testing.T, f *fakeDelegateStore, principal string) (string, types.DelegationVia) {
	t.Helper()
	ctx := context.Background()
	d, err := f.CreateDelegate(ctx, types.Delegate{ID: uuid.New(), Name: "portal", IdPClientID: "portal-client", Group: "portal-users"},
		newBearer(delegateCredentialPrefix))
	if err != nil {
		t.Fatal(err)
	}
	raw := newBearer(delegatedTokenPrefix)
	tok := types.DelegatedToken{
		ID: uuid.New(), DelegateID: d.ID, Principal: principal, Email: principal + "@corp.example",
		UserType: types.UserTypeStandard, Groups: []string{"portal-users"},
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(delegatedTokenTTL),
	}
	if _, err := f.MintDelegatedToken(ctx, tok, raw, time.Now()); err != nil {
		t.Fatal(err)
	}
	return raw, types.DelegationVia{Delegate: d.ID, Grant: tok.ID}
}

// TestDelegation_AllowListRouteWalk is acceptance test 6 of #1142: a delegated
// token reaches exactly the delegation allow-list. Every other route a human
// credential can reach — walked from routeMatrix, the chi.Walk-proven census —
// answers 403 with reason delegation_scope and writes an authz.denied row that
// names the portal, before any handler runs; and every allow-listed route is
// admitted. The routes the brief names are asserted to be in the refused set,
// so the walk cannot pass by losing one.
func TestDelegation_AllowListRouteWalk(t *testing.T) {
	srv, ast, aap, _ := newAuthzMatrixServer(t)
	rec := srv.cfg.Audit.(*recRecorder)
	const person = "sub-person"
	tok, via := seedDelegation(t, ast.fakeDelegateStore, person)

	ownRun := func() uuid.UUID {
		id := uuid.New()
		ast.mu.Lock()
		ast.runs[id] = types.AgentRun{ID: id, CreatedBy: person, State: types.RunRunning, Agent: "claude-code"}
		ast.mu.Unlock()
		return id
	}

	for key := range delegationAllowed {
		if _, ok := routeMatrix[key]; !ok {
			t.Errorf("delegationAllowed names %q, which the router does not register", key)
		}
	}
	mustRefuse := map[string]bool{
		"PUT /api/v1/secrets/{name}":              false,
		"POST /api/v1/me/tokens":                  false,
		"POST /api/v1/me/ssh-keys":                false,
		"POST /api/v1/approvals/{id}/approve":     false,
		"POST /api/v1/approvals/{id}/deny":        false,
		"POST /api/v1/admin/delegates":            false,
		"GET /api/v1/admin/delegates":             false,
		"DELETE /api/v1/admin/delegates/{id}":     false,
		"POST /api/v1/runs/{id}/revive":           false,
		"POST /api/v1/sessions/revoke":            false,
		"GET /api/v1/runs/{id}/attach":            false,
		"GET /api/v1/runs/{id}/recording/{runID}": false,
		"GET /api/v1/runs/{id}/policy":            false,
		"GET /api/v1/runs/{id}/ado-tokens":        false,
		// A member's own Azure DevOps disconnect revokes their run tokens.
		"DELETE /api/v1/scm/azure-devops/connection": false,
	}
	var refused, admitted int
	for key, rc := range routeMatrix {
		switch rc.class {
		case classAnonymous, classInternal, classDevice, classPortal, classSCIM:
			continue // a delegated token is not a credential there at all
		}
		method, pattern, _ := strings.Cut(key, " ")
		id := ownRun()
		if rc.entity == entityApproval {
			id = aap.seed(id) // the person's OWN approval: still not delegable
		}
		t.Run(key, func(t *testing.T) {
			before := len(rec.snapshot())
			w := do(t, srv, method, buildPath(pattern, id.String()), tok, bodyFor(method, rc))
			if delegationAllowed[key] {
				admitted++
				if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
					t.Fatalf("allow-listed route refused a delegated token: %d %s", w.Code, w.Body.String())
				}
				return
			}
			refused++
			var body errorBody
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if w.Code != http.StatusForbidden || body.Reason != string(authz.ReasonDelegationScope) {
				t.Fatalf("status = %d reason = %q, want 403 delegation_scope; body=%s", w.Code, body.Reason, w.Body.String())
			}
			if _, named := mustRefuse[key]; named {
				mustRefuse[key] = true
			}
			rows := rec.snapshot()[before:]
			if len(rows) != 1 || rows[0].Action != authz.AuditAction || rows[0].Actor != person ||
				!strings.Contains(string(rows[0].Data), `"reason":"delegation_scope"`) ||
				!strings.Contains(string(rows[0].Data), `"delegate":"`+via.Delegate.String()+`"`) {
				t.Fatalf("refusal rows = %+v, want one authz.denied delegation_scope row naming the person and the portal", rows)
			}
		})
	}
	for key, seen := range mustRefuse {
		if !seen {
			t.Errorf("%q was not walked and refused", key)
		}
	}
	if admitted != len(delegationAllowed) || refused < 100 {
		t.Fatalf("walked %d refused / %d admitted routes, want every allow-listed route and the rest of the human surface", refused, admitted)
	}
}

// delegatedOutputStore keeps memRunOutputs' rows while still offering the
// portal capability: memRunOutputs embeds only store.Store, so on its own it
// would hide store.DelegateStore from the delegated lane.
type delegatedOutputStore struct {
	*memRunOutputs
	store.DelegateStore
	store.RunsByCreatorPager
}

func newDelegatedOutputServer(t *testing.T, person string, shape ...func(*Config)) (*Server, *authzStore, *memRunOutputs, string, types.DelegationVia) {
	t.Helper()
	var mem *memRunOutputs
	var ast *authzStore
	srv, ast, _, _ := newAuthzMatrixServer(t, func(c *Config) {
		ast = c.Store.(*authzStore)
		mem = newMemRunOutputs(c.Store)
		c.Store = delegatedOutputStore{memRunOutputs: mem, DelegateStore: ast.fakeDelegateStore, RunsByCreatorPager: ast}
		for _, f := range shape {
			f(c)
		}
	})
	tok, via := seedDelegation(t, ast.fakeDelegateStore, person)
	return srv, ast, mem, tok, via
}

func seedOutputRun(ast *authzStore, mem *memRunOutputs, owner, source, output string) uuid.UUID {
	id := uuid.New()
	now := time.Now()
	ast.mu.Lock()
	ast.runs[id] = types.AgentRun{ID: id, CreatedBy: owner, State: types.RunCompleted, Agent: "claude-code", Interactive: source == paneSnapshotSource}
	ast.mu.Unlock()
	mem.rows[id] = store.RunOutput{RunID: id, Output: []byte(output), Source: source, CapturedAt: &now}
	return id
}

// A portal's delegated token reads the command output of its person's run on
// the same predicate as the other run routes: someone else's run is the same
// 404 GET /runs/{id} gives, and a served read writes no refusal row.
func TestDelegation_RunOutputReadsTheOwnersTail(t *testing.T) {
	const person = "sub-person"
	srv, ast, mem, tok, _ := newDelegatedOutputServer(t, person)
	own := seedOutputRun(ast, mem, person, "stdout", "hello\n")
	foreign := seedOutputRun(ast, mem, "sub-someone-else", "stdout", "not yours\n")

	w := do(t, srv, http.MethodGet, "/api/v1/runs/"+own.String()+"/output", tok, "")
	var body runOutputResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusOK || body.Output != "hello\n" || body.Source != "stdout" {
		t.Fatalf("own run: %d %s, want 200 with output %q from stdout", w.Code, w.Body, "hello\n")
	}
	if reasons := auditReasons(t, srv, "authz.denied"); len(reasons) != 0 {
		t.Fatalf("a served read wrote refusal rows: %v", reasons)
	}

	got := do(t, srv, http.MethodGet, "/api/v1/runs/"+foreign.String()+"/output", tok, "")
	want := do(t, srv, http.MethodGet, "/api/v1/runs/"+foreign.String(), tok, "")
	if got.Code != http.StatusNotFound || got.Code != want.Code || strings.Contains(got.Body.String(), "not yours") {
		t.Fatalf("foreign run: %d %s, want the %d GET /runs/{id} gives and none of its output", got.Code, got.Body, want.Code)
	}
}

// Option A of #1423: a portal reads command output only. A row recovered from
// the recording, or an interactive run's pane snapshot, stays the person's own:
// the delegated read is refused with delegation_scope naming the portal, and the
// person's own session reads the same row.
func TestDelegation_RunOutputRecordingDerived(t *testing.T) {
	const person = "sub-person"
	// With the row withheld the read falls through to the run's own answer: an
	// exec run kept nothing a portal may read, an interactive run's terminal is the recording's.
	for source, wantReason := range map[string]string{recordingOutputSource: reasonRunOutputNotKept, paneSnapshotSource: reasonRunOutputInteractive} {
		t.Run(source, func(t *testing.T) {
			srv, ast, mem, tok, via := newDelegatedOutputServer(t, person)
			id := seedOutputRun(ast, mem, person, source, "from the recording\n")
			path := "/api/v1/runs/" + id.String() + "/output"

			w := do(t, srv, http.MethodGet, path, tok, "")
			if w.Code != http.StatusConflict || errorReason(w) != wantReason || strings.Contains(w.Body.String(), "from the recording") {
				t.Fatalf("delegated read: %d %s, want 409 %s and none of the row", w.Code, w.Body, wantReason)
			}
			rec := srv.cfg.Audit.(*recRecorder)
			var rows []types.AuditEvent
			for _, ev := range rec.snapshot() {
				if ev.Action == authz.AuditAction {
					rows = append(rows, ev)
				}
			}
			if len(rows) != 1 || !strings.Contains(string(rows[0].Data), `"reason":"delegation_scope"`) ||
				!strings.Contains(string(rows[0].Data), `"delegate":"`+via.Delegate.String()+`"`) {
				t.Fatalf("refusal rows = %+v, want one delegation_scope row naming the portal", rows)
			}

			own := doSSO(t, srv, http.MethodGet, path, ssoSession(t, person, "person@corp.example", oidc.RoleUser), "")
			if own.Code != http.StatusOK || !strings.Contains(own.Body.String(), `"source":"`+source+`"`) {
				t.Fatalf("the person's own read: %d %s, want 200 with source %s", own.Code, own.Body, source)
			}
		})
	}
}

// TestDelegation_NeverOperator pins isOperator and isSecurityOperator's own
// refusal of a delegated context, independent of the role clamp and of the
// allow-list: an admin person, a security admin, and a context whose human is
// missing altogether — the no-human arm that otherwise reads as the admin token.
func TestDelegation_NeverOperator(t *testing.T) {
	s := &Server{}
	via := types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()}
	for name, ctx := range map[string]context.Context{
		"admin person":          operatorCtx("sub-a", "a@corp.example", oidc.RoleAdmin),
		"security admin person": operatorCtx("sub-s", "s@corp.example", oidc.RoleSecurityAdmin),
		"no human on context":   context.Background(),
	} {
		ctx := audit.WithDelegation(ctx, via)
		if s.isOperator(ctx) || s.isSecurityOperator(ctx) {
			t.Errorf("%s: a delegated context passed an admin predicate", name)
		}
	}
}

// TestDelegation_TokenCannotMintAPIToken pins handleCreateAPIToken's own
// refusal, reached directly so the allow-list is not what refuses: ten minutes
// of delegation must never become a permanent wdn_ credential.
func TestDelegation_TokenCannotMintAPIToken(t *testing.T) {
	srv := New(baseTestConfig(newHarness(t), newTokenMemStore()))
	ctx := withHumanIdentity(context.Background(), "sub-person", "p@corp.example", oidc.RoleUser, types.UserTypeStandard, []string{}, false)
	ctx = audit.WithDelegation(ctx, types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/me/tokens", strings.NewReader(`{"name":"x"}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.handleCreateAPIToken(w, r)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "delegated token") {
		t.Fatalf("mint from a delegated context: %d %s, want 403", w.Code, w.Body.String())
	}
}

// TestDelegation_AttachTicketCarriesVia is acceptance test 10's ticket lane: a
// ticket minted on the delegated lane stores the portal, and the attach
// WebSocket — which runs no auth middleware — replays it, so session.attach
// names the portal beside the person. A foreign run's ticket is the 404 a
// stranger gets.
func TestDelegation_AttachTicketCarriesVia(t *testing.T) {
	rec := &sshTestRecorder{}
	srv, st, _, run := holderTestServerWithAudit(t, rec)
	tok, via := seedDelegation(t, st.authzStore.fakeDelegateStore, holderOwner)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	t.Cleanup(ts.Close)

	w := do(t, srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/attach-ticket", tok, "")
	if w.Code != http.StatusOK {
		t.Fatalf("delegated attach ticket on the person's own run: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Ticket string `json:"ticket"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	st.authzStore.mu.Lock()
	stored := st.authzStore.tickets[out.Ticket]
	st.authzStore.mu.Unlock()
	if stored.Via == nil || *stored.Via != via {
		t.Fatalf("stored ticket via = %v, want %+v", stored.Via, via)
	}

	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/runs/" + run.ID.String() + "/attach?ticket=" + out.Ticket
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial attach: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	readAttachMode(t, c)
	var attach *types.AuditEvent
	waitFor(t, "session.attach", func() bool {
		attach = findAudit(rec.snapshot(), run.ID, "session.attach", "success")
		return attach != nil
	})
	if attach.Actor != holderOwner || viaOf(t, *attach) != via {
		t.Fatalf("session.attach = %+v, want the person as actor and the portal in data.via", attach)
	}

	foreign := uuid.New()
	st.authzStore.mu.Lock()
	st.authzStore.runs[foreign] = types.AgentRun{ID: foreign, CreatedBy: "someone-else", State: types.RunRunning, SandboxRef: "sbx-2"}
	st.authzStore.mu.Unlock()
	if w := do(t, srv, http.MethodPost, "/api/v1/runs/"+foreign.String()+"/attach-ticket", tok, ""); w.Code != http.StatusNotFound {
		t.Fatalf("delegated attach ticket on a foreign run: %d, want 404", w.Code)
	}
}

// TestDelegation_UIGatewayEntryCarriesVia is acceptance test 10's UI-gateway
// half: the gateway audits on the daemon's context, not the request's, so the
// ticket's via is written onto its entry rows explicitly — success and refusal.
func TestDelegation_UIGatewayEntryCarriesVia(t *testing.T) {
	h := newUIHarness(t, okBackend())
	ds := newFakeDelegateStore()
	h.srv.cfg.Store = &uiDelegateStore{h.store, ds}
	_, via := seedDelegation(t, ds, h.owner)
	delegated := audit.WithDelegation(context.Background(), via)
	ticket := func() string {
		tok, err := mintAttachTicket(delegated, h.store, h.run.ID, types.ActorHuman, h.owner, oidc.RoleUser, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	if w := h.enter(url.Values{"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {ticket()}}); w.Code != http.StatusFound {
		t.Fatalf("enter: %d %s", w.Code, w.Body.String())
	}
	if w := h.enter(url.Values{"run": {h.run.ID.String()}, "app": {"nope"}, "ticket": {ticket()}}); w.Code != http.StatusForbidden {
		t.Fatalf("enter an undeclared app: %d %s", w.Code, w.Body.String())
	}
	h.audit.mu.Lock()
	defer h.audit.mu.Unlock()
	var seen int
	for _, ev := range h.audit.events {
		if ev.Action != "ui.authorize" {
			continue
		}
		seen++
		if viaOf(t, ev) != via {
			t.Fatalf("ui.authorize %s names %s, want %+v", ev.Outcome, ev.Data, via)
		}
	}
	if seen != 2 {
		t.Fatalf("ui.authorize rows = %d, want the success and the refusal", seen)
	}
}

// TestDelegation_AttachPromoteAndDetachCarryVia is #1234's attach half: a
// delegated ticket's session.detach is written on the daemon's context, and a
// delegated observer's session.promote long after its attach, so the portal
// must be carried onto both rather than read off the request.
func TestDelegation_AttachPromoteAndDetachCarryVia(t *testing.T) {
	rec := &sshTestRecorder{}
	srv, st, _, run := holderTestServerWithAudit(t, rec)
	tok, via := seedDelegation(t, st.authzStore.fakeDelegateStore, holderOwner)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	t.Cleanup(ts.Close)
	dial := func() *websocket.Conn {
		t.Helper()
		w := do(t, srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/attach-ticket", tok, "")
		var out struct {
			Ticket string `json:"ticket"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusOK {
			t.Fatalf("delegated attach ticket: %d %s", w.Code, w.Body.String())
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/runs/"+run.ID.String()+"/attach?ticket="+out.Ticket, nil)
		if err != nil {
			t.Fatalf("dial attach: %v", err)
		}
		t.Cleanup(func() { _ = c.CloseNow() })
		return c
	}

	writer := dial()
	if readAttachMode(t, writer).ReadOnly {
		t.Fatal("the first delegated attach was admitted read-only")
	}
	waitFor(t, "the writer to register", func() bool { return srv.attachHolderFor(run.ID) != nil })
	observer := dial()
	if !readAttachMode(t, observer).ReadOnly {
		t.Fatal("the second delegated attach was admitted writable")
	}
	if err := writer.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("close the writer: %v", err)
	}
	if readPromotionFrame(t, observer, holderOwner).ReadOnly {
		t.Fatal("the observer was not promoted")
	}
	if promo := waitForActorAudit(t, rec, run.ID, "session.promote", holderOwner); viaOf(t, *promo) != via {
		t.Fatalf("session.promote data = %s, want via %+v", promo.Data, via)
	}
	if err := observer.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("close the promoted socket: %v", err)
	}
	var detaches []types.AuditEvent
	waitFor(t, "both session.detach rows", func() bool {
		detaches = detaches[:0]
		for _, ev := range rec.snapshot() {
			if ev.Action == "session.detach" && ev.RunID != nil && *ev.RunID == run.ID {
				detaches = append(detaches, ev)
			}
		}
		return len(detaches) == 2
	})
	for _, ev := range detaches {
		if viaOf(t, ev) != via {
			t.Fatalf("session.detach data = %s, want via %+v", ev.Data, via)
		}
	}
}

// uiPauseDelegateStore is uiPauseStore with the portal tables beside it.
type uiPauseDelegateStore struct {
	*uiPauseStore
	*fakeDelegateStore
}

// TestDelegation_UIGatewayRelayRowsCarryVia is #1234's UI-gateway half: the
// relay audits on the daemon's context, so the session minted from a delegated
// ticket carries the portal onto ui.start, ui.open, ui.close, a re-assert's
// ui.authorize refusal and the run.resume its presence thaws a paused run
// with, not only onto the entry row.
func TestDelegation_UIGatewayRelayRowsCarryVia(t *testing.T) {
	h := newUIHarness(t, closingBackend("sandbox app"))
	h.launcher = 5 // the app was started by this request, so ui.start is written
	ds := newFakeDelegateStore()
	h.srv.cfg.Store = &uiPauseDelegateStore{&uiPauseStore{uiMemStore: h.store, pauseMarks: pauseMarks{paused: true}}, ds}
	_, via := seedDelegation(t, ds, h.owner)
	paused, pausedAt := h.run, h.clock.now()
	paused.PausedAt, paused.PausedReason = &pausedAt, types.PauseReason("idle")
	h.store.putRun(paused)
	tok, err := mintAttachTicket(audit.WithDelegation(context.Background(), via), h.store, h.run.ID, types.ActorHuman, h.owner, oidc.RoleUser, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	w := h.enter(url.Values{"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {tok}})
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == uiCookieName {
			cookie = c
		}
	}
	if w.Code != http.StatusFound || cookie == nil {
		t.Fatalf("enter: %d %s, want a redirect with the relay cookie", w.Code, w.Body.String())
	}
	if r := h.relay("/ide", cookie, nil); r.Code != http.StatusOK {
		t.Fatalf("relay: %d %s", r.Code, r.Body.String())
	}
	handedOver := h.run
	handedOver.CreatedBy = "someone-else"
	h.store.putRun(handedOver)
	h.clock.advance(uiReassertInterval)
	if r := h.relay("/ide", cookie, nil); r.Code != http.StatusForbidden {
		t.Fatalf("relay on a handed-over run: %d %s, want 403", r.Code, r.Body.String())
	}

	want := map[string]bool{"run.resume": false, "ui.start": false, "ui.open": false, "ui.close": false, "ui.authorize/denied": false}
	waitFor(t, "the relay rows", func() bool {
		h.audit.mu.Lock()
		defer h.audit.mu.Unlock()
		for _, ev := range h.audit.events {
			key := ev.Action
			if ev.Action == "ui.authorize" {
				key += "/" + ev.Outcome
			}
			if _, ok := want[key]; ok {
				want[key] = true
			}
		}
		return !slices.Contains(slices.Collect(maps.Values(want)), false)
	})
	h.audit.mu.Lock()
	defer h.audit.mu.Unlock()
	for _, ev := range h.audit.events {
		if !strings.HasPrefix(ev.Action, "ui.") && ev.Action != "run.resume" {
			continue
		}
		if viaOf(t, ev) != via {
			t.Fatalf("%s/%s names %s, want %+v", ev.Action, ev.Outcome, ev.Data, via)
		}
	}
}

// TestDelegation_SecretAndSSHKeyWritesRefuseInTheHandler pins the in-handler
// refusal on PUT /secrets/{name} and POST /me/ssh-keys, reached directly so the
// allow-list is not what refuses: a delegated context gets the same 403
// delegation_scope, with its row, whatever delegationAllowed later says.
func TestDelegation_SecretAndSSHKeyWritesRefuseInTheHandler(t *testing.T) {
	srv, _, _, _ := newAuthzMatrixServer(t)
	rec := srv.cfg.Audit.(*recRecorder)
	via := types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()}
	ctx := withHumanIdentity(context.Background(), "sub-person", "p@corp.example", oidc.RoleUser, types.UserTypeStandard, []string{}, false)
	ctx = audit.WithDelegation(ctx, via)
	for name, c := range map[string]struct {
		path, body string
		handler    http.HandlerFunc
	}{
		"PUT /secrets/{name}": {"/api/v1/secrets/MY_TOKEN", `{"value":"a-long-enough-secret-value"}`, srv.handlePutSecret},
		"POST /me/ssh-keys":   {"/api/v1/me/ssh-keys", `{"public_key":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGb1ZQ0Tq3cZC0mH7Xp8c9r1c8gZ1lXq3j2a5pZ7a1Zb k"}`, srv.handleAddSSHKey},
	} {
		t.Run(name, func(t *testing.T) {
			before := len(rec.snapshot())
			method, _, _ := strings.Cut(name, " ")
			r := httptest.NewRequest(method, c.path, strings.NewReader(c.body)).WithContext(ctx)
			w := httptest.NewRecorder()
			c.handler(w, r)
			if w.Code != http.StatusForbidden || errorReason(w) != string(authz.ReasonDelegationScope) {
				t.Fatalf("delegated %s: %d %s, want 403 delegation_scope", name, w.Code, w.Body.String())
			}
			rows := rec.snapshot()[before:]
			if len(rows) != 1 || rows[0].Action != authz.AuditAction || viaOf(t, rows[0]) != via {
				t.Fatalf("rows = %+v, want one authz.denied row naming the portal", rows)
			}
		})
	}
}

// A portal's delegated token never reads what is recovered from the recording
// (#1423): the line a failed run's hint quotes from it is withheld on every
// route that serves the run, and the person's own session still reads it.
func TestDelegation_FailureHintQuoteIsNotServedToAPortal(t *testing.T) {
	const person = "sub-person"
	const quote = "QUOTED-FROM-THE-RECORDING"
	srv, ast, mem, tok, _ := newDelegatedOutputServer(t, person)
	id := seedOutputRun(ast, mem, person, "stdout", "x\n")
	ast.mu.Lock()
	run := ast.runs[id]
	run.State = types.RunFailed
	run.FailureHint = fmt.Sprintf(modelAccessHintFormat, quote)
	ast.runs[id] = run
	ast.mu.Unlock()
	own := ssoSession(t, person, "person@corp.example", oidc.RoleUser)
	for _, path := range []string{"/api/v1/runs/" + id.String(), "/api/v1/runs"} {
		w := do(t, srv, http.MethodGet, path, tok, "")
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), quote) || !strings.Contains(w.Body.String(), "model-access problem") {
			t.Errorf("delegated GET %s: %d %s, want 200 with the unquoted hint", path, w.Code, w.Body)
		}
		w = doSSO(t, srv, http.MethodGet, path, own, "")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), quote) {
			t.Errorf("the person's own GET %s: %d %s, want 200 with the quote", path, w.Code, w.Body)
		}
	}
}

// A delegated read never runs the recording repair and is never told a
// recording was erased: it answers as a run with no such row does, while the
// person's own session triggers the repair and hears the erasure.
func TestDelegation_RunOutputNeverRepairsOrReportsErasure(t *testing.T) {
	const person = "sub-person"
	fs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spy := &outputRecordingStore{Store: fs}
	srv, ast, mem, tok, _ := newDelegatedOutputServer(t, person, func(c *Config) {
		rr := newRecoveringRunner()
		rr.execOutputUncaptured = true
		c.Runner, c.RecordingStore = rr, spy
	})
	own := ssoSession(t, person, "person@corp.example", oidc.RoleUser)

	missing := seedOutputRun(ast, mem, person, "stdout", "")
	delete(mem.rows, missing)
	path := "/api/v1/runs/" + missing.String() + "/output"
	w := do(t, srv, http.MethodGet, path, tok, "")
	if w.Code != http.StatusConflict || errorReason(w) != reasonRunOutputNotCaptured || spy.opens.Load() != 0 {
		t.Fatalf("delegated read of a run with no row: %d %s after %d recording opens, want 409 %s and none", w.Code, w.Body, spy.opens.Load(), reasonRunOutputNotCaptured)
	}
	for _, ev := range srv.cfg.Audit.(*recRecorder).snapshot() {
		if ev.Action == "run.output.finalize" {
			t.Fatalf("a delegated read wrote %+v", ev)
		}
	}
	doSSO(t, srv, http.MethodGet, path, own, "")
	if spy.opens.Load() == 0 {
		t.Fatal("the person's own read did not reach the recording repair, so the delegated zero above proves nothing")
	}

	erased := seedOutputRun(ast, mem, person, recordingOutputSource, "gone")
	mem.recording[erased] = memRecordingOutput{erased: true}
	path = "/api/v1/runs/" + erased.String() + "/output"
	if w := do(t, srv, http.MethodGet, path, tok, ""); w.Code != http.StatusConflict || errorReason(w) == reasonRecordingErased {
		t.Fatalf("delegated read after a recording erasure: %d %s, want the 409 a run with no row gives", w.Code, w.Body)
	}
	if w := doSSO(t, srv, http.MethodGet, path, own, ""); w.Code != http.StatusGone || errorReason(w) != reasonRecordingErased {
		t.Fatalf("the person's own read after a recording erasure: %d %s, want 410 %s", w.Code, w.Body, reasonRecordingErased)
	}
}

// A portal reading an interactive run whose pane snapshot is withheld is not
// told the owner-only recording-off sentences, which are false for it.
func TestDelegation_RunOutputInteractiveSentenceIsNeutralForAPortal(t *testing.T) {
	const person = "sub-person"
	srv, ast, mem, tok, _ := newDelegatedOutputServer(t, person, func(c *Config) { c.RecordingStore = nil })
	id := seedOutputRun(ast, mem, person, paneSnapshotSource, "snapshot\n")
	path := "/api/v1/runs/" + id.String() + "/output"
	const neutral = "an interactive run keeps no output here: its terminal is the recording's to keep"
	if w := do(t, srv, http.MethodGet, path, tok, ""); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), neutral) {
		t.Fatalf("delegated read: %d %s, want 409 with %q", w.Code, w.Body, neutral)
	}
	delete(mem.rows, id)
	own := ssoSession(t, person, "person@corp.example", oidc.RoleUser)
	if w := doSSO(t, srv, http.MethodGet, path, own, ""); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Nothing was kept") {
		t.Fatalf("the person's own read: %d %s, want the owner's recording-off sentence", w.Code, w.Body)
	}
}
