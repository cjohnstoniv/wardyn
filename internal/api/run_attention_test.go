// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pendingRow builds a PENDING approval on runID of the given kind/scope,
// already Held-projected by the caller (attentionSrv.projectHolds) where the
// test needs that — attention() itself only reads Held/HeldUntil, never
// State, past the caller's own gate, so a case that means to feed attention()
// directly must project first (see attentionFixture.pending below).
func pendingRow(runID uuid.UUID, kind types.ApprovalKind, scope string, grantID *uuid.UUID) types.ApprovalRequest {
	return types.ApprovalRequest{
		ID: uuid.New(), RunID: runID, Kind: kind, GrantID: grantID,
		RequestedScope: json.RawMessage(scope), State: types.ApprovalPending, RequestedAt: time.Now().UTC(),
	}
}

// attentionFixture is a bare *Server (no store, no router) for calling
// attention()/heldCandidates directly — capServer's own pattern, widened with
// OIDC role context helpers for mayDecide's row-8 branch.
func attentionFixture(t *testing.T) *Server {
	t.Helper()
	return New(Config{}) // New defaults cfg.Now to time.Now, which projectHolds needs
}

func adminCtx() context.Context {
	return withOIDCRole(withOIDCEmail(withOIDCHuman(context.Background(), "sub-admin"), "admin@corp.example"), oidc.RoleAdmin)
}

func memberCtxFor(sub string) context.Context {
	return withOIDCRole(withOIDCEmail(withOIDCHuman(context.Background(), sub), sub+"@corp.example"), oidc.RoleUser)
}

// TestAttentionRules pins the rule table's rows 1-8, each isolated so a
// future regression names the exact row it broke.
func TestAttentionRules(t *testing.T) {
	s := attentionFixture(t)
	runID := uuid.New()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil).WithContext(adminCtx())

	t.Run("row 1: a terminal run needs nobody", func(t *testing.T) {
		run := types.AgentRun{ID: runID, State: types.RunCompleted, CreatedBy: "sub-admin"}
		pending := []types.ApprovalRequest{pendingRow(runID, types.ApprovalToolCall, `{}`, nil)}
		s.projectHolds(pending)
		if got := s.attention(req, run, pending, false); got != nil {
			t.Fatalf("attention = %+v, want nil (terminal outranks a held row)", got)
		}
	})

	t.Run("row 2: a lease-ended run reads as ended, not held", func(t *testing.T) {
		run := types.AgentRun{ID: runID, State: types.RunRunning, LostReason: types.LostEnded, CreatedBy: "sub-admin"}
		pending := []types.ApprovalRequest{pendingRow(runID, types.ApprovalToolCall, `{}`, nil)}
		s.projectHolds(pending)
		if got := s.attention(req, run, pending, false); got != nil {
			t.Fatalf("attention = %+v, want nil (lease-ended)", got)
		}
	})

	t.Run("row 3: a lost (reboot/outage) run needs a revive — owner in Admin view, you in User view", func(t *testing.T) {
		run := types.AgentRun{ID: runID, State: types.RunRunning, LostReason: types.LostReboot, CreatedBy: "sub-admin", LostAt: ptrTime(time.Now())}
		if got := s.attention(req, run, nil, false); got == nil || got.Kind != types.AttentionLost || got.By != types.AttentionYou {
			t.Fatalf("User view: attention = %+v, want {lost you}", got)
		}
		if got := s.attention(req, run, nil, true); got == nil || got.Kind != types.AttentionLost || got.By != types.AttentionOwner {
			t.Fatalf("Admin view: attention = %+v, want {lost owner} (H-3: Sign-in/revive is a User-view action)", got)
		}
	})

	t.Run("row 5: a passive-only run gets no attention", func(t *testing.T) {
		run := types.AgentRun{ID: runID, State: types.RunRunning, CreatedBy: "sub-admin"}
		pending := []types.ApprovalRequest{pendingRow(runID, types.ApprovalPushContent, `{}`, nil)}
		pending[0].RequestedAt = time.Now().Add(-601 * time.Second) // past push_content's own 600s ceiling
		s.projectHolds(pending)
		if got := s.attention(req, run, pending, false); got != nil {
			t.Fatalf("attention = %+v, want nil (nothing PENDING is actually held)", got)
		}
	})

	t.Run("row 6: an ADO sign-in/consent reauth is ado_consent, owner-cleared", func(t *testing.T) {
		run := types.AgentRun{ID: runID, State: types.RunRunning, CreatedBy: "sub-admin"}
		scope := `{"lane":"azure_devops","mechanism":"entra_signin","owner":"sub-admin","provider_id":"p1"}`
		pending := []types.ApprovalRequest{pendingRow(runID, types.ApprovalCredentialReauth, scope, nil)}
		s.projectHolds(pending)
		if got := s.attention(req, run, pending, false); got == nil || got.Kind != types.AttentionADOConsent || got.By != types.AttentionYou {
			t.Fatalf("User view: attention = %+v, want {ado_consent you}", got)
		}
		if got := s.attention(req, run, pending, true); got == nil || got.Kind != types.AttentionADOConsent || got.By != types.AttentionOwner {
			t.Fatalf("Admin view: attention = %+v, want {ado_consent owner}", got)
		}
	})

	t.Run("row 7: any other reauth is plain reauth, owner-cleared", func(t *testing.T) {
		run := types.AgentRun{ID: runID, State: types.RunRunning, CreatedBy: "sub-admin"}
		pending := []types.ApprovalRequest{pendingRow(runID, types.ApprovalCredentialReauth, `{"credential_source":"aws_sso"}`, nil)}
		s.projectHolds(pending)
		if got := s.attention(req, run, pending, false); got == nil || got.Kind != types.AttentionReauth || got.By != types.AttentionYou {
			t.Fatalf("attention = %+v, want {reauth you}", got)
		}
	})

	t.Run("row 8: a member's held admin-only row (tool_call/push_content/credential) is by=admin (H-3a)", func(t *testing.T) {
		memberReq := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil).WithContext(memberCtxFor("sub-member"))
		run := types.AgentRun{ID: runID, State: types.RunRunning, CreatedBy: "sub-member"}
		for _, kind := range []types.ApprovalKind{types.ApprovalToolCall, types.ApprovalPushContent, types.ApprovalCredential} {
			scope := `{}`
			if kind == types.ApprovalCredential {
				scope = `{"mode":"wait_for_review"}` // credential is held the same egress way, still admin-only
			}
			pending := []types.ApprovalRequest{pendingRow(runID, kind, scope, nil)}
			s.projectHolds(pending)
			got := s.attention(memberReq, run, pending, false)
			if got == nil || got.Kind != types.AttentionApproval || got.By != types.AttentionAdmin {
				t.Fatalf("kind=%s: attention = %+v, want {approval admin}", kind, got)
			}
		}
	})

	t.Run("row 8: a member's own ADO escalation is by=you", func(t *testing.T) {
		memberReq := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil).WithContext(memberCtxFor("sub-member"))
		run := types.AgentRun{ID: runID, State: types.RunRunning, CreatedBy: "sub-member"}
		grantID := uuid.New()
		scope := `{"lane":"azure_devops","grant_id":"` + grantID.String() + `","capability":"pr_create"}`
		pending := []types.ApprovalRequest{pendingRow(runID, types.ApprovalToolCall, scope, &grantID)}
		s.projectHolds(pending)
		if got := s.attention(memberReq, run, pending, false); got == nil || got.Kind != types.AttentionApproval || got.By != types.AttentionYou {
			t.Fatalf("attention = %+v, want {approval you}", got)
		}
	})

	t.Run("row 8: four-eyes on, the creator viewing their own egress is by=admin (H-3a)", func(t *testing.T) {
		t.Setenv(envEgressSecondHuman, "1")
		adminReq := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil).WithContext(adminCtx())
		run := types.AgentRun{ID: runID, State: types.RunRunning, CreatedBy: "sub-admin"}
		pending := []types.ApprovalRequest{pendingRow(runID, types.ApprovalEgressDomain, `{"host":"h","mode":"wait_for_review"}`, nil)}
		s.projectHolds(pending)
		if got := s.attention(adminReq, run, pending, false); got == nil || got.By != types.AttentionAdmin {
			t.Fatalf("attention = %+v, want by=admin (the creator cannot clear their own egress under four-eyes)", got)
		}
	})

	t.Run("multi-row precedence: 6, then 7, then 8, first whose by==you wins", func(t *testing.T) {
		memberReq := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil).WithContext(memberCtxFor("sub-member"))
		run := types.AgentRun{ID: runID, State: types.RunRunning, CreatedBy: "sub-member"}
		// An admin-only tool_call (rule 8, by=admin) PLUS the member's own
		// egress request (rule 8, by=you): the "you" candidate must win even
		// though it is not first in the pending slice.
		admin8 := pendingRow(runID, types.ApprovalToolCall, `{}`, nil)
		you8 := pendingRow(runID, types.ApprovalEgressDomain, `{"host":"h","mode":"wait_for_review"}`, nil)
		pending := []types.ApprovalRequest{admin8, you8}
		s.projectHolds(pending)
		got := s.attention(memberReq, run, pending, false)
		if got == nil || got.By != types.AttentionYou || got.Pending != 2 {
			t.Fatalf("attention = %+v, want {approval you pending=2}", got)
		}

		// In the ADMIN view, rule 6/7's `by` is unconditionally "owner" (H-3:
		// Sign-in/revive is a User-view-only action), so a security operator
		// looking at a MEMBER's run sees a genuine conflict: rule 7's reauth
		// is "owner" (nobody but the run's own owner clears a reauth), but
		// the operator CAN decide the plain tool_call (rule 8, operator
		// bypass) — "you". Rule 8's "you" must still win the "first whose by
		// is you" scan even though rule 7 sorts before it.
		adminReq := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil).WithContext(adminCtx())
		reauth7 := pendingRow(runID, types.ApprovalCredentialReauth, `{"credential_source":"aws_sso"}`, nil)
		pending = []types.ApprovalRequest{reauth7, admin8, you8}
		s.projectHolds(pending)
		got = s.attention(adminReq, run, pending, true)
		if got == nil || got.Kind != types.AttentionApproval || got.By != types.AttentionYou {
			t.Fatalf("Admin view: attention = %+v, want the you-candidate (approval you) to win over reauth's owner", got)
		}
	})
}

func ptrTime(t time.Time) *time.Time { return &t }

// fakeRunsFilteredStore is store.RunsFilteredPager backed by authzStore's own
// run map — an in-memory stand-in for the SQL predicates ListRunsFiltered
// applies, narrow enough for handleMeAttention's own tests (owner scope +
// the "active" status only; #1197 L1b's /me/attention never asks for more).
type fakeRunsFilteredStore struct{ *authzStore }

func (s *fakeRunsFilteredStore) ListRunsFiltered(_ context.Context, f store.RunFilter, _ store.Page) ([]types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []types.AgentRun
	for _, run := range s.runs {
		if f.Owner != "" && run.CreatedBy != f.Owner {
			continue
		}
		live := !run.State.IsTerminal() && run.LostReason != types.LostEnded
		if !live {
			continue
		}
		out = append(out, run)
	}
	return out, nil
}

func (s *fakeRunsFilteredStore) CountHiddenRuns(context.Context, store.RunFilter) (int, int, error) {
	return 0, 0, nil
}

var _ store.RunsFilteredPager = (*fakeRunsFilteredStore)(nil)

// meAttentionFixture wires a real server (router included) for
// GET /me/attention's own tests.
func meAttentionFixture(t *testing.T) (*Server, *authzStore, *authzApprovals) {
	t.Helper()
	ast := newAuthzStore()
	fst := &fakeRunsFilteredStore{ast}
	aap := newAuthzApprovals(ast)
	h := newHarness(t)
	cfg := baseTestConfig(h, fst)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Approvals = aap
	return New(cfg), ast, aap
}

func meAttentionOf(t *testing.T, w *httptest.ResponseRecorder) meAttention {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("GET /me/attention: status = %d, body=%s", w.Code, w.Body.String())
	}
	var got meAttention
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	return got
}

// TestMeAttentionCounts: needs_you equals the count of by=="you" over the
// same scope GET /runs?view= would give, and pending_approvals equals the
// scoped PENDING count GET /approvals?state=PENDING scopes today.
func TestMeAttentionCounts(t *testing.T) {
	srv, ast, aap := meAttentionFixture(t)
	memberSess := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)

	ownRun := uuid.New()
	foreignRun := uuid.New()
	ast.mu.Lock()
	ast.runs[ownRun] = types.AgentRun{ID: ownRun, CreatedBy: "sub-member", State: types.RunRunning}
	ast.runs[foreignRun] = types.AgentRun{ID: foreignRun, CreatedBy: "sub-someone-else", State: types.RunRunning}
	ast.mu.Unlock()

	// A decidable request (the member's own egress, no grant needed since
	// capEgressHost is unenforced by default) -> counts toward needs_you.
	aap.mu.Lock()
	aap.byID[uuid.New()] = pendingRow(ownRun, types.ApprovalEgressDomain, `{"host":"h","mode":"wait_for_review"}`, nil)
	// An admin-only held row on the SAME run -> does NOT count (H-3a).
	aap.byID[uuid.New()] = pendingRow(ownRun, types.ApprovalToolCall, `{}`, nil)
	// A foreign run's own pending approval must never be visible to this member at all.
	aap.byID[uuid.New()] = pendingRow(foreignRun, types.ApprovalEgressDomain, `{"host":"h","mode":"wait_for_review"}`, nil)
	aap.mu.Unlock()

	w := doSSO(t, srv, http.MethodGet, "/api/v1/me/attention?view=user", memberSess, "")
	got := meAttentionOf(t, w)
	if got.NeedsYou != 1 {
		t.Fatalf("needs_you = %d, want 1 (only the member's own decidable egress)", got.NeedsYou)
	}
	if got.PendingApprovals != 2 {
		t.Fatalf("pending_approvals = %d, want 2 (both of the member's OWN run's pending rows, scoped like GET /approvals)", got.PendingApprovals)
	}
}

// TestMeAttentionAdminOnlyRowIsZeroForTheMember is F9's second required
// H-3a test in isolation: a member whose run is held ENTIRELY on rows only
// an admin can decide has needs_you==0.
func TestMeAttentionAdminOnlyRowIsZeroForTheMember(t *testing.T) {
	srv, ast, aap := meAttentionFixture(t)
	memberSess := ssoSession(t, "sub-member2", "member2@corp.example", oidc.RoleUser)
	runID := uuid.New()
	ast.mu.Lock()
	ast.runs[runID] = types.AgentRun{ID: runID, CreatedBy: "sub-member2", State: types.RunRunning}
	ast.mu.Unlock()
	aap.mu.Lock()
	aap.byID[uuid.New()] = pendingRow(runID, types.ApprovalPushContent, `{}`, nil)
	aap.mu.Unlock()

	w := doSSO(t, srv, http.MethodGet, "/api/v1/me/attention?view=user", memberSess, "")
	got := meAttentionOf(t, w)
	if got.NeedsYou != 0 {
		t.Fatalf("needs_you = %d, want 0 (the held row is admin-only)", got.NeedsYou)
	}
	if got.PendingApprovals != 1 {
		t.Fatalf("pending_approvals = %d, want 1", got.PendingApprovals)
	}
}
