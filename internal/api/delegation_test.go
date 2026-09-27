// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
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
	}
	var refused, admitted int
	for key, rc := range routeMatrix {
		switch rc.class {
		case classAnonymous, classInternal, classDevice, classPortal:
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
	via := types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()}
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
