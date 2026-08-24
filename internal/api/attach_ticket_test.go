// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

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
	tok2, _ := mintAttachTicket(ctx, st, run, types.ActorHuman, "bob", oidc.RoleMember, now)
	ta, ok, err := consumeAttachTicket(ctx, st, tok2, run, now)
	if err != nil || !ok {
		t.Fatalf("valid ticket did not redeem: ok=%v err=%v", ok, err)
	}
	if ta.principal != "bob" || ta.actorType != types.ActorHuman {
		t.Fatalf("attribution lost: got %v/%q", ta.actorType, ta.principal)
	}
	if ta.role != oidc.RoleMember {
		t.Fatalf("role lost: got %q, want %q", ta.role, oidc.RoleMember)
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
	if w := attach(mint("alice", oidc.RoleMember)); w.Code == http.StatusForbidden && strings.Contains(w.Body.String(), deniedMsg) {
		t.Fatalf("owning member's ticket was refused by the ticket-role check: %s", w.Body.String())
	}
	// A non-owning member's ticket must be refused BY THE TICKET-ROLE CHECK
	// specifically (not merely fail for some unrelated reason).
	if w := attach(mint("mallory", oidc.RoleMember)); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), deniedMsg) {
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
// records every rejection under ssh.auth. Both refusals in the lane (a ticket
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
	tok, err := mintAttachTicket(context.Background(), ast, run, types.ActorHuman, "mallory", oidc.RoleMember, time.Now())
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
