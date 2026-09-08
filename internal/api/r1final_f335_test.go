// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// F335: the run-create path never authorized the SELECTED workspace against the
// CALLER. Two doors reached the same room and both are pinned here:
//
//  1. req.workspace_id — a second plain member, and a security_admin (a tier
//     defined never to reach into a run, credential material or the host), each
//     turned another member's workspace id into a host bind inside a sandbox
//     they own, 201.
//  2. the RESOLVED spec — the member-mount re-check ran against the workspace
//     OWNER's roots, so a run holding a foreign member's local_dir was admitted
//     no matter whose roots the caller has.
//
// Door 1 asserts 404 PARITY, never a bare "it's a 404": the foreign answer must
// be byte-identical to the truly-missing one on the SAME route in the SAME
// session, or the status itself is the existence oracle denyForeignWorkspace
// exists to close.

// TestF335_ForeignMemberWorkspaceNotLaunchable is door 1.
func TestF335_ForeignMemberWorkspaceNotLaunchable(t *testing.T) {
	for _, tc := range []struct{ name, sub, email, role string }{
		{"plain member", ownerOtherSub, "other@corp.example", oidc.RoleMember},
		{"security admin", "sub-sec-admin", "sec@corp.example", oidc.RoleSecurityAdmin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, project := memberProjectRoot(t)
			srv, st, fr := memberDispatchHarness(t, runner.MemberMountPolicy{Roots: []string{root}})
			foreign := memberOwnedWorkspace(st, ownerMemberSub, project)
			caller := ssoSession(t, tc.sub, tc.email, tc.role)

			body := func(id string) string {
				return `{"agent":"claude-code","task":"do the thing","workspace_id":"` + id + `"}`
			}
			got := doSSO(t, srv, http.MethodPost, "/api/v1/runs", caller, body(foreign.String()))
			missing := doSSO(t, srv, http.MethodPost, "/api/v1/runs", caller, body(uuid.New().String()))

			if got.Code != http.StatusNotFound {
				t.Fatalf("POST /runs naming another member's workspace: code = %d, want 404; body=%s", got.Code, got.Body.String())
			}
			if got.Code != missing.Code || got.Body.String() != missing.Body.String() {
				t.Errorf("foreign answer %d %s != missing answer %d %s — the difference is an existence oracle across members",
					got.Code, got.Body.String(), missing.Code, missing.Body.String())
			}
			if fr.createCalls != 0 {
				t.Errorf("CreateSandbox calls = %d, want 0 — a foreign member's host directory reached a sandbox the caller owns", fr.createCalls)
			}
		})
	}
}

// TestF335_ForeignMemberSourceRefusedOnResolvedSpec is door 2: the same refusal
// on the RESOLVED spec, which is what a hand-authored policy naming the path
// directly walks through. Driven at seedAndAdmitWorkspace, the chokepoint
// create AND preflight share, so neither can preview or launch what the other
// refuses.
func TestF335_ForeignMemberSourceRefusedOnResolvedSpec(t *testing.T) {
	root, project := memberProjectRoot(t)
	srv, st, _ := memberDispatchHarness(t, runner.MemberMountPolicy{Roots: []string{root}})
	memberOwnedWorkspace(st, ownerMemberSub, project) // the OTHER member's onboarded dir

	spec := types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		WorkspaceMounts:     []types.WorkspaceMount{{Source: project, Target: composerWorkspaceTarget}},
	}
	req := createRunRequest{Agent: "claude-code"}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).
		WithContext(operatorCtx(ownerOtherSub, "other@corp.example", oidc.RoleMember))
	w := httptest.NewRecorder()

	if _, ok := srv.seedAndAdmitWorkspace(r.Context(), w, r, &spec, &req); ok {
		t.Fatalf("ADMITTED: a run held another member's onboarded dir %q; the member-mount re-check gates it against the OWNER's roots, so per-principal roots constrain the caller not at all", project)
	}
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("refusal code = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	// The refusal must not distinguish "another member owns it" from "nobody
	// onboarded it" — that difference is the same existence oracle door 1 closes.
	if !strings.Contains(w.Body.String(), "is not an onboarded local directory") {
		t.Errorf("refusal body = %s, want the byte-identical not-onboarded sentence", w.Body.String())
	}
}

// TestF335_OwnAndOperatorWorkspacesStillLaunch is the counterfactual that stops
// the gate above from being satisfied by refusing everyone: the owner's own
// workspace and an OPERATOR-owned one both still reach the runner.
func TestF335_OwnAndOperatorWorkspacesStillLaunch(t *testing.T) {
	for _, tc := range []struct{ name, owner string }{
		{"own workspace", ownerMemberSub},
		{"operator-owned workspace", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, project := memberProjectRoot(t)
			srv, st, fr := memberDispatchHarness(t, runner.MemberMountPolicy{Roots: []string{root}})
			member := ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleMember)
			createMemberRun(t, srv, fr, member, memberOwnedWorkspace(st, tc.owner, project))
		})
	}
}
