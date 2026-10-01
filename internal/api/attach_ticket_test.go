// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// memTicketStore models the attach_tickets rows in memory with the SAME contract
// the SQL has (migration 0026 / store.PG.ConsumeAttachTicket): consume deletes
// the row on ANY redemption attempt for a live token, and expiry is enforced by
// the lookup itself — the run-id binding is the CALLER's check. The SQL is proven
// against Postgres in internal/store/store_ephemeral_pg_test.go; this fake keeps
// the wrapper's rules testable without a DSN.
type memTicketStore struct {
	store.Store
	mu sync.Mutex
	m  map[string]memTicket
}

type memTicket struct {
	t   store.AttachTicket
	exp time.Time
}

func newMemTicketStore() *memTicketStore { return &memTicketStore{m: map[string]memTicket{}} }

func (s *memTicketStore) MintAttachTicket(_ context.Context, token string, t store.AttachTicket, _, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[token] = memTicket{t: t, exp: expiresAt}
	return nil
}

func (s *memTicketStore) ConsumeAttachTicket(_ context.Context, token string, now time.Time) (store.AttachTicket, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mt, ok := s.m[token]
	if !ok || !mt.exp.After(now) {
		return store.AttachTicket{}, false, nil
	}
	delete(s.m, token) // single-use: burned on any redemption attempt
	return mt.t, true, nil
}

func TestAttachTicket(t *testing.T) {
	st := newMemTicketStore()
	ctx := context.Background()
	run := uuid.New()
	other := uuid.New()
	now := time.Unix(1_700_000_000, 0)

	tok, err := mintAttachTicket(ctx, st, run, types.ActorHuman, "alice", oidc.RoleAdmin, now)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if tok == "" {
		t.Fatal("mint returned empty ticket")
	}

	// Wrong run id: rejected (and the ticket is burned on the attempt).
	if _, ok, _ := consumeAttachTicket(ctx, st, tok, other, now); ok {
		t.Fatal("ticket redeemed against the wrong run id")
	}
	if _, ok, _ := consumeAttachTicket(ctx, st, tok, run, now); ok {
		t.Fatal("ticket survived a burned redemption attempt (must be single-use)")
	}

	// Fresh ticket: redeems once, carries attribution + role, then is gone.
	tok2, _ := mintAttachTicket(ctx, st, run, types.ActorHuman, "bob", oidc.RoleUser, now)
	ta, ok, err := consumeAttachTicket(ctx, st, tok2, run, now)
	if err != nil || !ok {
		t.Fatalf("valid ticket did not redeem: ok=%v err=%v", ok, err)
	}
	if ta.principal != "bob" || ta.actorType != types.ActorHuman {
		t.Fatalf("attribution lost: got %v/%q", ta.actorType, ta.principal)
	}
	if ta.role != oidc.RoleUser {
		t.Fatalf("role lost: got %q, want %q", ta.role, oidc.RoleUser)
	}
	if _, ok, _ := consumeAttachTicket(ctx, st, tok2, run, now); ok {
		t.Fatal("ticket redeemed twice")
	}

	// Expiry: a ticket presented after its TTL is rejected.
	tok3, _ := mintAttachTicket(ctx, st, run, types.ActorHuman, "carol", oidc.RoleAdmin, now)
	if _, ok, _ := consumeAttachTicket(ctx, st, tok3, run, now.Add(attachTicketTTL+time.Second)); ok {
		t.Fatal("expired ticket redeemed")
	}
}

// TestAttachTicketStoreError: a store failure must NOT read as a rejected
// ticket — the caller distinguishes them (500 vs 403), so the error has to
// survive the wrapper instead of collapsing into ok=false.
func TestAttachTicketStoreError(t *testing.T) {
	ta, ok, err := consumeAttachTicket(context.Background(), errTicketStore{}, "tok", uuid.New(), time.Now())
	if err == nil {
		t.Fatal("store failure was swallowed; the caller would 403 a healthy ticket")
	}
	if ok || ta != (ticketActor{}) {
		t.Fatalf("store failure must not authenticate: ok=%v actor=%+v", ok, ta)
	}
}

type errTicketStore struct{ store.Store }

func (errTicketStore) ConsumeAttachTicket(context.Context, string, time.Time) (store.AttachTicket, bool, error) {
	return store.AttachTicket{}, false, context.DeadlineExceeded
}

// TestAttachWS_TicketRoleAuthorization pins item 3's WS-handler-side re-check
// (attach.go's handleAttachWS): the ticket lane bypasses humanOrAdminAuth /
// requireOperator entirely, so the ticket's OWN stamped role/principal
// (captured at MINT time) is the only authorization signal left when the
// WebSocket route is reached via ?ticket=. This complements
// authz_test.go's matrix, which only exercises the ticket-LESS fallback
// lane (classAdmin) — chi.Walk reports one route regardless of which lane a
// request takes, so this property needs its own test.
//
// The check runs BEFORE websocket.Accept, so a denial is a plain HTTP 403 —
// testable with httptest, no real WebSocket client needed. An ALLOWED
// scenario is asserted by absence of that specific 403 (the plain
// httptest.ResponseRecorder this test drives cannot complete a real
// WebSocket upgrade — coder/websocket's Accept fails for its OWN unrelated
// reason once past this check, same as any non-WS httptest client hitting a
// WS route).
func TestAttachWS_TicketRoleAuthorization(t *testing.T) {
	ast := newAuthzStore()
	h := newHarness(t)
	cfg := baseTestConfig(h, ast)
	cfg.Runner = &fakeRunner{} // past the "no runner configured" 503 into the ticket-role check
	srv := New(cfg)

	ownedRun := uuid.New()
	ast.mu.Lock()
	ast.runs[ownedRun] = types.AgentRun{ID: ownedRun, CreatedBy: "alice", State: types.RunRunning, SandboxRef: "sbx-1"}
	ast.mu.Unlock()

	const deniedMsg = "attach ticket does not authorize this run"

	mint := func(principal, role string) string {
		t.Helper()
		tok, err := mintAttachTicket(context.Background(), ast, ownedRun, types.ActorHuman, principal, role, time.Now())
		if err != nil {
			t.Fatalf("mint(%s, %s): %v", principal, role, err)
		}
		return tok
	}
	attach := func(tok string) *httptest.ResponseRecorder {
		return do(t, srv, http.MethodGet, "/api/v1/runs/"+ownedRun.String()+"/attach?ticket="+tok, "", "")
	}

	// The owning member's ticket must get PAST the ticket-role check.
	if w := attach(mint("alice", oidc.RoleUser)); w.Code == http.StatusForbidden && strings.Contains(w.Body.String(), deniedMsg) {
		t.Fatalf("owning member's ticket was refused by the ticket-role check: %s", w.Body.String())
	}
	// A non-owning member's ticket must be refused BY THE TICKET-ROLE CHECK
	// specifically (not merely fail for some unrelated reason).
	if w := attach(mint("mallory", oidc.RoleUser)); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), deniedMsg) {
		t.Fatalf("non-owning member's ticket: code=%d body=%q, want 403 %q", w.Code, w.Body.String(), deniedMsg)
	}
	// An admin-role ticket authorizes the run regardless of who minted it.
	if w := attach(mint("root-admin", oidc.RoleAdmin)); w.Code == http.StatusForbidden && strings.Contains(w.Body.String(), deniedMsg) {
		t.Fatalf("admin-role ticket was refused by the ticket-role check: %s", w.Body.String())
	}
}

// TestAttachWS_TicketDenialsAreAudited: the ?ticket= lane is the only route to a
// live PTY that never runs humanOrAdminAuth, and it audited NONE of its own
// refusals — so probing it left no trace at all, where the sibling SSH gateway
// records every rejection under ssh.authenticate. Both refusals in the lane (a ticket
// that does not resolve, and a ticket that resolves but does not authorize the
// run it names) must now land in the trail, with the principal named only when
// the ticket actually proved one.
func TestAttachWS_TicketDenialsAreAudited(t *testing.T) {
	ast := newAuthzStore()
	h := newHarness(t)
	cfg := baseTestConfig(h, ast)
	cfg.Runner = &fakeRunner{}
	srv := New(cfg)

	run := uuid.New()
	ast.mu.Lock()
	ast.runs[run] = types.AgentRun{ID: run, CreatedBy: "alice", State: types.RunRunning, SandboxRef: "sbx-1"}
	ast.mu.Unlock()

	denials := func() []types.AuditEvent {
		var out []types.AuditEvent
		for _, ev := range h.audit.events {
			if ev.Action == "session.attach" && ev.Outcome == "failure" {
				out = append(out, ev)
			}
		}
		return out
	}

	// (1) a ticket that does not resolve at all.
	if w := do(t, srv, http.MethodGet, "/api/v1/runs/"+run.String()+"/attach?ticket=deadbeef", "", ""); w.Code != http.StatusForbidden {
		t.Fatalf("bogus ticket: code = %d, want 403", w.Code)
	}
	got := denials()
	if len(got) != 1 {
		t.Fatalf("a rejected attach ticket produced %d session.attach failures, want 1 — the lane is unaudited", len(got))
	}
	if got[0].Actor != "unknown" {
		t.Errorf("actor = %q, want \"unknown\": the caller proved no principal", got[0].Actor)
	}
	if got[0].RunID == nil || *got[0].RunID != run {
		t.Errorf("denial not attributed to the run being probed: %v", got[0].RunID)
	}

	// (2) a ticket that resolves but does not authorize this run.
	tok, err := mintAttachTicket(context.Background(), ast, run, types.ActorHuman, "mallory", oidc.RoleUser, time.Now())
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if w := do(t, srv, http.MethodGet, "/api/v1/runs/"+run.String()+"/attach?ticket="+tok, "", ""); w.Code != http.StatusForbidden {
		t.Fatalf("non-owner ticket: code = %d, want 403", w.Code)
	}
	got = denials()
	if len(got) != 2 {
		t.Fatalf("the ticket-role refusal produced %d session.attach failures total, want 2", len(got))
	}
	if got[1].Actor != "mallory" {
		t.Errorf("actor = %q, want mallory: this ticket DID prove a principal", got[1].Actor)
	}
	if !strings.Contains(string(got[1].Data), "does not authorize this run") {
		t.Errorf("denial data = %s, want the refusal reason", got[1].Data)
	}
}

// TestAttach_RefusesAKeptRun: a run the lease ended is RUNNING with its agent
// stopped, so both attach gates refuse it with a plain 409, rather than minting
// a ticket or upgrading a WebSocket that dies on its first exec.
func TestAttach_RefusesAKeptRun(t *testing.T) {
	ast := newAuthzStore()
	h := newHarness(t)
	cfg := baseTestConfig(h, ast)
	cfg.Runner = &fakeRunner{}
	srv := New(cfg)

	run, endedAt := uuid.New(), time.Now()
	ast.mu.Lock()
	ast.runs[run] = types.AgentRun{ID: run, CreatedBy: "alice", OperatorOwned: true, State: types.RunRunning, SandboxRef: "sbx-1",
		LostAt: &endedAt, LostReason: types.LostEnded}
	ast.mu.Unlock()

	const refused = "run has ended; cannot attach"
	w := do(t, srv, http.MethodPost, "/api/v1/runs/"+run.String()+"/attach-ticket", adminToken, "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), refused) {
		t.Errorf("ticket mint: code=%d body=%q, want 409 %q", w.Code, w.Body.String(), refused)
	}
	tok, err := mintAttachTicket(context.Background(), ast, run, types.ActorHuman, "alice", oidc.RoleUser, time.Now())
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	w = do(t, srv, http.MethodGet, "/api/v1/runs/"+run.String()+"/attach?ticket="+tok, "", "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), refused) {
		t.Errorf("attach: code=%d body=%q, want 409 %q", w.Code, w.Body.String(), refused)
	}
}

// TestAttachWS_TicketMintedBeforeCutoffIsRefused is the terminal lane's twin of
// the UI gateway's: a ticket admitted before a revoke is refused at consume
// with the bad-ticket 403, and the refusal leaves a denied session.attach row
// naming the ticket's principal.
func TestAttachWS_TicketMintedBeforeCutoffIsRefused(t *testing.T) {
	ast := newAuthzStore()
	h := newHarness(t)
	cfg := baseTestConfig(h, ast)
	cfg.Runner = &fakeRunner{}
	rev := newCutoffRevocations()
	cfg.SessionRevocations = rev
	srv := New(cfg)

	run := uuid.New()
	ast.mu.Lock()
	ast.runs[run] = types.AgentRun{ID: run, CreatedBy: "alice", State: types.RunRunning, SandboxRef: "sbx-1"}
	ast.mu.Unlock()

	issued := time.Now()
	tok, err := mintAttachTicket(context.Background(), ast, run, types.ActorHuman, "alice", oidc.RoleUser, issued)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	rev.nowFunc = func() time.Time { return issued.Add(time.Second) }
	if err := rev.RevokeSub(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}

	w := do(t, srv, http.MethodGet, "/api/v1/runs/"+run.String()+"/attach?ticket="+tok, "", "")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "invalid, expired, or already-used attach ticket") {
		t.Fatalf("a ticket minted before the cutoff: %d %s, want the 403 bad-ticket body", w.Code, w.Body.String())
	}
	var denied []types.AuditEvent
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "session.attach" && ev.Outcome == "failure" {
			denied = append(denied, ev)
		}
	}
	if len(denied) != 1 || denied[0].Actor != "alice" || !strings.Contains(string(denied[0].Data), `"reason":"revoked"`) {
		t.Fatalf("denied rows = %+v, want one naming alice with reason revoked", denied)
	}
}

// TestAttachWS_TicketAuthority: the terminal lane redeems through the same helper
// as the UI gateway, so the same authority checks bind it (#1474, #1475). Every
// refusal is the bad-ticket 403 but for an unanswerable lookup, which is a 503.
func TestAttachWS_TicketAuthority(t *testing.T) {
	type setup struct {
		name    string
		ticket  func(t *testing.T, ast *authzStore, rev *cutoffRevocations, run uuid.UUID) string
		want    int
		reason  string // the session.attach failure row's reason ("" = no row expected)
		cfgMods func(*authzStore, *Config)
	}
	now := time.Now()
	viaTicket := func(t *testing.T, ast *authzStore, run uuid.UUID, principal string, revoke bool) string {
		t.Helper()
		_, via := seedDelegation(t, ast.fakeDelegateStore, principal)
		if revoke {
			if _, err := ast.RevokeDelegate(context.Background(), via.Delegate, now); err != nil {
				t.Fatal(err)
			}
		}
		tok, err := mintAttachTicket(audit.WithDelegation(context.Background(), via), ast, run, types.ActorHuman, principal, oidc.RoleUser, now)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	for _, tc := range []setup{
		{name: "an email-named revoke after the mint",
			ticket: func(t *testing.T, ast *authzStore, rev *cutoffRevocations, run uuid.UUID) string {
				tok, err := mintAttachTicket(withOIDCEmail(context.Background(), "alice@corp.example"), ast, run, types.ActorHuman, "alice", oidc.RoleUser, now)
				if err != nil {
					t.Fatal(err)
				}
				rev.nowFunc = func() time.Time { return now.Add(time.Second) }
				_ = rev.RevokeSub(context.Background(), "alice@corp.example")
				return tok
			}, want: http.StatusForbidden, reason: "revoked"},
		{name: "an unreadable revocation store",
			ticket: func(t *testing.T, ast *authzStore, rev *cutoffRevocations, run uuid.UUID) string {
				tok, _ := mintAttachTicket(context.Background(), ast, run, types.ActorHuman, "alice", oidc.RoleUser, now)
				rev.err = errors.New("store unreachable")
				return tok
			}, want: http.StatusServiceUnavailable, reason: "revocation_unavailable"},
		{name: "a row with no authority time",
			ticket: func(t *testing.T, ast *authzStore, rev *cutoffRevocations, run uuid.UUID) string {
				ast.mu.Lock()
				defer ast.mu.Unlock()
				ast.tickets["legacy"] = store.AttachTicket{RunID: run, ActorType: types.ActorHuman, Principal: "alice", Role: oidc.RoleUser}
				return "legacy"
			}, want: http.StatusForbidden},
		{name: "a portal revoked after the mint",
			ticket: func(t *testing.T, ast *authzStore, rev *cutoffRevocations, run uuid.UUID) string {
				return viaTicket(t, ast, run, "alice", true)
			}, want: http.StatusForbidden, reason: "delegation_ended"},
		{name: "a portal whose store cannot answer",
			ticket: func(t *testing.T, ast *authzStore, rev *cutoffRevocations, run uuid.UUID) string {
				return viaTicket(t, ast, run, "alice", false)
			}, want: http.StatusServiceUnavailable, reason: "delegation_unavailable",
			cfgMods: func(ast *authzStore, cfg *Config) { cfg.Store = &delegationErrAuthzStore{ast} }},
		{name: "a live portal grant is admitted past the authority checks",
			ticket: func(t *testing.T, ast *authzStore, rev *cutoffRevocations, run uuid.UUID) string {
				return viaTicket(t, ast, run, "alice", false)
			}, want: http.StatusUpgradeRequired},
		{name: "a ticket with no email and no revoke is admitted",
			ticket: func(t *testing.T, ast *authzStore, rev *cutoffRevocations, run uuid.UUID) string {
				tok, _ := mintAttachTicket(context.Background(), ast, run, types.ActorHuman, "alice", oidc.RoleUser, now)
				return tok
			}, want: http.StatusUpgradeRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ast := newAuthzStore()
			h := newHarness(t)
			cfg := baseTestConfig(h, ast)
			cfg.Runner = &fakeRunner{}
			rev := newCutoffRevocations()
			cfg.SessionRevocations = rev
			if tc.cfgMods != nil {
				tc.cfgMods(ast, &cfg)
			}
			srv := New(cfg)
			run := uuid.New()
			ast.mu.Lock()
			ast.runs[run] = types.AgentRun{ID: run, CreatedBy: "alice", State: types.RunRunning, SandboxRef: "sbx-1"}
			ast.mu.Unlock()

			tok := tc.ticket(t, ast, rev, run)
			w := do(t, srv, http.MethodGet, "/api/v1/runs/"+run.String()+"/attach?ticket="+tok, "", "")
			if w.Code != tc.want {
				t.Fatalf("code = %d %s, want %d", w.Code, w.Body.String(), tc.want)
			}
			var rows []types.AuditEvent
			for _, ev := range h.audit.snapshot() {
				if ev.Action == "session.attach" && ev.Outcome == "failure" {
					rows = append(rows, ev)
				}
			}
			if tc.reason == "" {
				return
			}
			if len(rows) != 1 || !strings.Contains(string(rows[0].Data), `"reason":"`+tc.reason+`"`) {
				t.Fatalf("session.attach failure rows = %+v, want one with reason %s", rows, tc.reason)
			}
		})
	}
}

// delegationErrAuthzStore is authzStore with an unanswerable portal grant lookup.
type delegationErrAuthzStore struct{ *authzStore }

func (delegationErrAuthzStore) GetDelegatedTokenByID(context.Context, uuid.UUID, time.Time) (types.DelegatedToken, error) {
	return types.DelegatedToken{}, errors.New("store unreachable")
}
