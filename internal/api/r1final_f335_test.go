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

// the run-create path never authorized the SELECTED workspace against the
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

// TestForeignMemberWorkspaceNotLaunchable is door 1.
func TestForeignMemberWorkspaceNotLaunchable(t *testing.T) {
	// ticket: F335
	for _, tc := range []struct{ name, sub, email, role string }{
		{"plain member", ownerOtherSub, "other@corp.example", oidc.RoleUser},
		{"security admin", "sub-sec-admin", "sec@corp.example", oidc.RoleSecurityAdmin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, project := memberProjectRoot(t)
			srv, st, fr := userDispatchHarness(t, runner.UserMountPolicy{Roots: []string{root}})
			foreign := memberOwnedWorkspace(st, ownerMemberSub, project)
			// A security admin launches through a token: an SSO session in the
			// Admin view cannot launch at all (refuseAdminViewLaunch).
			launch := func(body string) *httptest.ResponseRecorder {
				if tc.role == oidc.RoleUser {
					return doSSO(t, srv, http.MethodPost, "/api/v1/runs", ssoSession(t, tc.sub, tc.email, tc.role), body)
				}
				return do(t, srv, http.MethodPost, "/api/v1/runs", st.humanToken(tc.sub, tc.role), body)
			}

			body := func(id string) string {
				return `{"agent":"claude-code","task":"do the thing","workspace_id":"` + id + `"}`
			}
			got := launch(body(foreign.String()))
			missing := launch(body(uuid.New().String()))

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

// TestForeignMemberSourceRefusedOnResolvedSpec is door 2: the same refusal
// on the RESOLVED spec, which is what a hand-authored policy naming the path
// directly walks through. Driven at seedAndAdmitWorkspace, the chokepoint
// create AND preflight share, so neither can preview or launch what the other
// refuses.
func TestForeignMemberSourceRefusedOnResolvedSpec(t *testing.T) {
	// ticket: F335
	root, project := memberProjectRoot(t)
	const repo = "acme/secret-proj"

	// refuse builds a fresh harness (never sharing state across scenarios) and
	// returns seedAndAdmitWorkspace's refusal for the SAME source (a local_dir
	// path or a repo, per kind), either owned by another member (foreign=true)
	// or never onboarded at all (foreign=false). gate mirrors
	// seedAndAdmitWorkspace's own two callers: true is handleCreateRun
	// (POST /runs), false is handlePreflightRun (POST /runs/preflight) — this
	// IS the chokepoint both routes share, per this test's own doc comment
	// above.
	refuse := func(t *testing.T, kind string, foreign, gate bool) *httptest.ResponseRecorder {
		t.Helper()
		srv, st, _ := userDispatchHarness(t, runner.UserMountPolicy{Roots: []string{root}})
		spec := types.RunPolicySpec{MinConfinementClass: types.CC2}
		switch kind {
		case "mount":
			if foreign {
				memberOwnedWorkspace(st, ownerMemberSub, project) // the OTHER member's onboarded dir
			}
			spec.WorkspaceMounts = []types.WorkspaceMount{{Source: project, Target: composerWorkspaceTarget}}
		case "repo":
			if foreign {
				st.put(types.Workspace{
					OwnedBy: ownerMemberSub,
					Status:  types.WorkspaceScanned,
					Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: repo}},
				}) // the OTHER member's onboarded repo
			}
			spec.WorkspaceRepos = []types.WorkspaceRepo{{Repo: repo}}
		default:
			t.Fatalf("unknown kind %q", kind)
		}
		req := createRunRequest{Agent: "claude-code"}
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).
			WithContext(operatorCtx(ownerOtherSub, "other@corp.example", oidc.RoleUser))
		w := httptest.NewRecorder()
		if _, ok := srv.seedAndAdmitWorkspace(r.Context(), w, r, &spec, &req, gate); ok {
			t.Fatalf("ADMITTED (kind=%s foreign=%v gate=%v): a run held a source it should not have", kind, foreign, gate)
		}
		return w
	}

	for _, route := range []struct {
		name string
		gate bool
	}{
		{"create", true},     // POST /runs
		{"preflight", false}, // POST /runs/preflight
	} {
		// kind="mount" pins authorizeSpecWorkspaceSources' MOUNT arm
		// (runs_create.go:217); kind="repo" pins its REPO arm
		// (runs_create.go:223) — the round-2 review's N2: the exact arm the
		// round-1 HTTP exploit went through (inline workspace_repos), which
		// this test did not cover before.
		for _, kind := range []string{"mount", "repo"} {
			t.Run(route.name+"/"+kind, func(t *testing.T) {
				foreign := refuse(t, kind, true, route.gate)
				neverOnboarded := refuse(t, kind, false, route.gate)
				if foreign.Code != http.StatusUnprocessableEntity {
					t.Errorf("foreign refusal code = %d, want 422; body=%s", foreign.Code, foreign.Body.String())
				}
				wantSentence := "is not an onboarded local directory"
				if kind == "repo" {
					wantSentence = "is not an onboarded repository"
				}
				if !strings.Contains(foreign.Body.String(), wantSentence) {
					t.Errorf("foreign refusal body = %s, want the not-onboarded sentence %q", foreign.Body.String(), wantSentence)
				}
				// #656 H1: both scenarios name the IDENTICAL source, so status,
				// message AND reason (the whole JSON body) must be byte-identical —
				// any difference is a cross-member existence oracle: "another
				// member owns it" told apart from "nobody onboarded it" is exactly
				// what this refusal must never disclose, on the wire class as much
				// as the sentence.
				if foreign.Code != neverOnboarded.Code || foreign.Body.String() != neverOnboarded.Body.String() {
					t.Errorf("foreign vs never-onboarded must answer identically: foreign=%d %s; neverOnboarded=%d %s",
						foreign.Code, foreign.Body.String(), neverOnboarded.Code, neverOnboarded.Body.String())
				}
			})
		}
	}
}

// TestOwnAndOperatorWorkspacesStillLaunch is the counterfactual that stops
// the gate above from being satisfied by refusing everyone: the owner's own
// workspace and an OPERATOR-owned one both still reach the runner.
func TestOwnAndOperatorWorkspacesStillLaunch(t *testing.T) {
	// ticket: F335
	for _, tc := range []struct{ name, owner string }{
		{"own workspace", ownerMemberSub},
		{"operator-owned workspace", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, project := memberProjectRoot(t)
			srv, st, fr := userDispatchHarness(t, runner.UserMountPolicy{Roots: []string{root}})
			member := ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleUser)
			createMemberRun(t, srv, fr, member, memberOwnedWorkspace(st, tc.owner, project))
		})
	}
}
