// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDecideApproval_MemberOnMissingApproval_ReasonLiteral (#656 M2) pins the
// LITERAL wire reason for authorizeUserDecision's member-tier 404
// (approvals.go, the tool_call/credential decide path — a plain member is
// never told an approval exists until BOTH its kind and its run's ownership
// clear), not just the Go constant.
func TestDecideApproval_MemberOnMissingApproval_ReasonLiteral(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, nil)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Approvals = h.approvals
	srv := New(cfg)
	member := ssoSession(t, "sub-m2-member", "member@corp.example", oidc.RoleUser)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/approvals/"+uuid.NewString()+"/approve", member, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("member deciding a nonexistent approval: status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if got := errorReason(w); got != "approval_not_found" {
		t.Errorf("reason = %q, want the literal \"approval_not_found\"; body=%s", got, w.Body.String())
	}
}

// TestKillRun_AlreadyTerminal_ReasonLiteral (#656 M2) pins the LITERAL wire
// reason on POST /runs/{id}/kill against an already-COMPLETED run, not just
// the Go constant — a silent rename of reasons.go's value must fail this.
func TestKillRun_AlreadyTerminal_ReasonLiteral(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	st := &raceStore{run: types.AgentRun{ID: runID}, state: types.RunCompleted}
	cfg := baseTestConfig(h, st)
	srv := New(cfg)

	w := do(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/kill", adminToken, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("kill an already-completed run: status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	if got := errorReason(w); got != "run_kill_already_terminal" {
		t.Errorf("reason = %q, want the literal \"run_kill_already_terminal\"; body=%s", got, w.Body.String())
	}
}

// TestCreateRun_CloudSTSRequiresSPIRE_ReasonLiteral (#656 M2) pins the LITERAL
// wire reason on POST /runs when the resolved policy's ceiling names a
// cloud_sts grant and the wired identity provider refuses it (invariant 5),
// not just the Go constant. It uses the harness's own embedded identity
// provider, which already implements grantChecker's CheckGrants this way
// (internal/identity/embedded's own ErrRequiresSPIRE) — no fake needed.
func TestCreateRun_CloudSTSRequiresSPIRE_ReasonLiteral(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, nil)
	cfg.DefaultPolicy = types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		AllowedDomains:      []string{"api.anthropic.com"},
		EligibleGrants:      []types.GrantSpec{{Kind: types.GrantCloudSTS}},
	}
	srv := New(cfg)

	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, `{"agent":"claude-code","task":"t"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create with a cloud_sts ceiling and no SPIRE provider: status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	if got := errorReason(w); got != "run_grants_require_spire" {
		t.Errorf("reason = %q, want the literal \"run_grants_require_spire\"; body=%s", got, w.Body.String())
	}
}
