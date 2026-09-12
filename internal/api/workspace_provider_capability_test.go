// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The `workspace_provider` capability kind (0.7.2) at EVERY door a member can
// bring a repository through.
//
// A table over doors rather than a test per door, because the kind is worth
// nothing if one of them answers differently: six doors, six legs each. Three
// of the six route around denyMemberRequest entirely, and three of those clone
// SERVER-SIDE (scan, build) or re-point what will be cloned (update), which is
// why "a member may only reach their own workspace" is not the same answer as
// "a member may bring work from this provider".

const (
	capProviderRowID = "corp-github"
	capProviderRepo  = "acme/app" // -> https://github.com/acme/app.git
	// capProviderUnclaimed is on a host NO row claims — the only shape the
	// capability really has nothing to say about. Note "other/app" is NOT that
	// shape: it resolves to github.com, which the github row claims kind-wide,
	// so it IS keyed (and then refused by admission).
	capProviderUnclaimed = "https://gitlab.example.com/team/app.git"
)

// capProviderSite is a deployment with ONE enabled github row whose base URL
// admits capProviderRepo.
func capProviderSite() types.SiteConfig {
	return providersConfig([]types.GitProvider{githubRow(capProviderRowID, false, "https://github.com/acme")})
}

// capProviderDisabledSite is the same row turned OFF. A present-but-disabled row
// is the admin saying "off": admitRepoURL refuses through it while still naming
// it, so the capability check must still run — keying the gate on the admission
// bit instead of on the row would skip it here.
func capProviderDisabledSite() types.SiteConfig {
	return providersConfig([]types.GitProvider{githubRow(capProviderRowID, true, "https://github.com/acme")})
}

func capProviderEnforced() map[string]bool { return map[string]bool{capWorkspaceProvider: true} }

func capProviderAllow(sub string) []types.CapabilityGrant {
	return []types.CapabilityGrant{
		grant(types.CapabilitySubjectUser, sub, capWorkspaceProvider, capProviderRowID, types.CapabilityAllow),
	}
}

func capProviderDeny(sub string) []types.CapabilityGrant {
	return []types.CapabilityGrant{
		grant(types.CapabilitySubjectUser, sub, capWorkspaceProvider, capProviderRowID, types.CapabilityDeny),
	}
}

// assertProviderDenied is the one shared assertion: the 403, exactly one audited
// reason, and the DISCLOSURE rule — the body names the provider KIND and neither
// the row id nor any base URL, because GET /workspace-providers is a
// security-tier door for exactly that reason.
func assertProviderDenied(t *testing.T, srv *Server, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "github") {
		t.Errorf("body = %s, want the provider KIND named", body)
	}
	for _, leak := range []string{"https://github.com/acme", capProviderRowID} {
		if strings.Contains(body, leak) {
			t.Errorf("body = %s, must not disclose %q to a member", body, leak)
		}
	}
	reasons := auditReasons(t, srv, "authz.denied")
	if !slices.Contains(reasons, "capability_"+capWorkspaceProvider) {
		t.Errorf("authz.denied reasons = %v, want capability_workspace_provider", reasons)
	}
	if n := len(reasons); n != 1 {
		t.Errorf("authz.denied rows = %d (%v), want exactly one — a refused door audits once", n, reasons)
	}
}

func assertNotRefused(t *testing.T, w *httptest.ResponseRecorder, why string) {
	t.Helper()
	if w.Code == http.StatusForbidden {
		t.Fatalf("status = 403 (%s); %s", w.Body.String(), why)
	}
}

// ─── the doors ────────────────────────────────────────────────────────────────

// providerDoor is one way a repository reaches a clone. fire drives ONE request
// through it under the given capability rows and site config, as the given
// caller, and hands back the server (for its audit trail) and the response.
type providerDoor struct {
	name string
	// sub is the member fire's `member` session authenticates as — the subject a
	// grant has to be written against for this door's fixture.
	sub      string
	member   func(t *testing.T) *http.Cookie
	operator func(t *testing.T) *http.Cookie
	fire     func(t *testing.T, cs *capStore, sc types.SiteConfig, session *http.Cookie) (*Server, *httptest.ResponseRecorder)
}

func providerRunDoor(name, body string, ws []types.Workspace) providerDoor {
	return providerDoor{
		name:   name,
		sub:    govMemberSub,
		member: func(t *testing.T) *http.Cookie { return govSession(t, govMemberSub, []string{"eng"}, false) },
		operator: func(t *testing.T) *http.Cookie {
			return ssoSession(t, "sub-prov-admin", "admin@corp.example", oidc.RoleAdmin)
		},
		fire: func(t *testing.T, cs *capStore, sc types.SiteConfig, session *http.Cookie) (*Server, *httptest.ResponseRecorder) {
			t.Helper()
			srv, st, _ := govEscapeFixture(t, cs)
			st.siteConfig, st.workspaces = sc, ws
			return srv, doSSO(t, srv, http.MethodPost, "/api/v1/runs", session, body)
		},
	}
}

func providerWorkspaceDoor(name string, fire func(t *testing.T, srv *Server, st *ownerStore, session *http.Cookie) *httptest.ResponseRecorder) providerDoor {
	return providerDoor{
		name: name,
		sub:  ownerMemberSub,
		member: func(t *testing.T) *http.Cookie {
			return ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleMember)
		},
		operator: func(t *testing.T) *http.Cookie {
			return ssoSession(t, "sub-owner-admin", "admin@corp.example", oidc.RoleAdmin)
		},
		fire: func(t *testing.T, cs *capStore, sc types.SiteConfig, session *http.Cookie) (*Server, *httptest.ResponseRecorder) {
			t.Helper()
			srv, st, _ := ownerHarness(t, runner.MemberMountPolicy{})
			st.caps, st.siteConfig = cs, sc
			return srv, fire(t, srv, st, session)
		},
	}
}

// repoWorkspace is a stored workspace whose one source is the repo on the row —
// the shape the scan and build doors read their locators from.
func repoWorkspace(st *ownerStore) uuid.UUID {
	return st.put(types.Workspace{
		OwnedBy: ownerMemberSub,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: capProviderRepo}},
	})
}

func providerDoors() []providerDoor {
	onboarded := []types.Workspace{{
		ID: uuid.New(), Name: "app", OwnedBy: govMemberSub,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: capProviderRepo}},
	}}
	const wsBody = `{"name":"app","sources":[{"type":"repo","source":"` + capProviderRepo + `"}]}`
	return []providerDoor{
		// The LEGACY single `repo` field, which reaches neither denyMemberRequest
		// nor the resolved-spec gate and is nonetheless cloned by the sandbox,
		// broker-minted for and unioned into the run's egress.
		providerRunDoor("POST /runs (legacy repo field)",
			`{"agent":"claude-code","task":"t","repo":"`+capProviderRepo+`"}`, nil),
		// The RESOLVED spec — the un-bypassable gate a hand-authored inline
		// policy naming the repo directly also passes through.
		providerRunDoor("POST /runs (resolved spec)",
			`{"agent":"claude-code","task":"t","inline_policy":{"workspace_repos":[{"repo":"`+
				capProviderRepo+`","target":"/home/agent/work/app"}]}}`, onboarded),
		providerWorkspaceDoor("POST /workspaces", func(t *testing.T, srv *Server, _ *ownerStore, session *http.Cookie) *httptest.ResponseRecorder {
			return doSSO(t, srv, http.MethodPost, "/api/v1/workspaces", session, wsBody)
		}),
		// An EDIT is how a member moves a workspace they were allowed to create
		// onto a provider they hold nothing for. The stored workspace starts
		// ephemeral-only, so the repo arrives with the body.
		providerWorkspaceDoor("PUT /workspaces/{id}", func(t *testing.T, srv *Server, st *ownerStore, session *http.Cookie) *httptest.ResponseRecorder {
			id := st.put(types.Workspace{OwnedBy: ownerMemberSub})
			return doSSO(t, srv, http.MethodPut, "/api/v1/workspaces/"+id.String(), session, wsBody)
		}),
		// A scan is a SERVER-SIDE clone (launchSourceScanRun, repoCloneURL of
		// each attached source's locator), reachable by the member owner.
		providerWorkspaceDoor("POST /workspaces/{id}/scan", func(t *testing.T, srv *Server, st *ownerStore, session *http.Cookie) *httptest.ResponseRecorder {
			return doSSO(t, srv, http.MethodPost, "/api/v1/workspaces/"+repoWorkspace(st).String()+"/scan", session, "")
		}),
		// So is the wizard's Build step (resolveWorkspaceImage ->
		// repoOwnDevcontainerURL -> envbuilder).
		providerWorkspaceDoor("POST /workspaces/{id}/build", func(t *testing.T, srv *Server, st *ownerStore, session *http.Cookie) *httptest.ResponseRecorder {
			return doSSO(t, srv, http.MethodPost, "/api/v1/workspaces/"+repoWorkspace(st).String()+"/build", session, "")
		}),
	}
}

// TestCapabilityWorkspaceProviderAtEveryDoor is the kind's whole contract, run
// at every door that can reach a clone.
func TestCapabilityWorkspaceProviderAtEveryDoor(t *testing.T) {
	for _, door := range providerDoors() {
		t.Run(door.name, func(t *testing.T) {
			t.Run("enforced, no grant: 403", func(t *testing.T) {
				srv, w := door.fire(t, &capStore{enf: capProviderEnforced()}, capProviderSite(), door.member(t))
				assertProviderDenied(t, srv, w)
			})

			// The NARROWING leg. capGranted refuses on !enforced, which would
			// refuse every member run on every deployment that has not enforced
			// the kind — i.e. all of them on upgrade day.
			t.Run("unenforced, no grant: not refused", func(t *testing.T) {
				_, w := door.fire(t, &capStore{}, capProviderSite(), door.member(t))
				assertNotRefused(t, w, "an unenforced narrowing kind must stay allowed")
			})

			// Deny sits ABOVE the enforcement switch — the adoption on-ramp, and
			// the one leg that makes a deny row worth writing before anyone
			// flips a switch.
			t.Run("unenforced, a DENY row: 403", func(t *testing.T) {
				srv, w := door.fire(t, &capStore{grants: capProviderDeny(door.sub)}, capProviderSite(), door.member(t))
				assertProviderDenied(t, srv, w)
			})

			t.Run("enforced, holding the row id: not refused", func(t *testing.T) {
				_, w := door.fire(t, &capStore{enf: capProviderEnforced(), grants: capProviderAllow(door.sub)},
					capProviderSite(), door.member(t))
				assertNotRefused(t, w, "a member holding the row's id may bring work from it")
			})

			// THE UPGRADE PIN: no provider rows is legacy open mode, and must be
			// byte-identical to 0.7.1.
			t.Run("no provider rows: not refused", func(t *testing.T) {
				_, w := door.fire(t, &capStore{enf: capProviderEnforced()}, types.SiteConfig{}, door.member(t))
				assertNotRefused(t, w, "legacy open mode must behave exactly as 0.7.1 did")
			})

			t.Run("operator: exempt", func(t *testing.T) {
				_, w := door.fire(t, &capStore{enf: capProviderEnforced()}, capProviderSite(), door.operator(t))
				assertNotRefused(t, w, "a capability bounds the tier below the one writing the grants")
			})

			// A row that CLAIMS the host and refuses anyway still names itself, so
			// denyMemberWorkspaceProviders keys on it (see that helper's own doc:
			// keying on providerFor's admission BIT would skip exactly this case).
			// At the DOOR, though, ADMISSION reaches it first (0.7.2, A3) and
			// refuses it outright — a disabled row is the admin saying "off",
			// which binds operators too, so it is never merely a missing grant.
			// The refusal a member sees is therefore the admission sentence, and
			// no authz.denied row is written: nobody was denied by a capability.
			t.Run("a DISABLED row is refused by ADMISSION, ahead of the capability", func(t *testing.T) {
				srv, w := door.fire(t, &capStore{enf: capProviderEnforced()}, capProviderDisabledSite(), door.member(t))
				if w.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
				}
				if body := w.Body.String(); !strings.Contains(body, admitMember) {
					t.Errorf("body = %s, want the admission refusal %q", body, admitMember)
				}
				if r := auditReasons(t, srv, "authz.denied"); len(r) != 0 {
					t.Errorf("authz.denied reasons = %v; admission refused before any capability did", r)
				}
			})
		})
	}
}

// TestCapabilityWorkspaceProviderUnclaimedHostIsANoOp: a repository on a host NO
// row claims has no row for a grant to name, so the capability has nothing to
// say about it — with or without a legacy scm_hosts entry keeping it launchable.
// Refusing here would be refusing for the WRONG reason: whether such a
// repository is admissible at all is the admission verdict's question, and that
// one binds operators too.
//
// The boundary is CLAIM, not admission. A repo inside a CLAIMED host that the
// row's base paths refuse (github.com/other/* against a row scoped to
// github.com/acme — the case org-path scoping exists for) IS keyed by the row
// that refused it, which is what the disabled-row leg of the door table pins.
func TestCapabilityWorkspaceProviderUnclaimedHostIsANoOp(t *testing.T) {
	body := `{"agent":"claude-code","task":"t","repo":"` + capProviderUnclaimed + `"}`
	for _, tc := range []struct {
		name string
		sc   types.SiteConfig
		// admitted says whether ADMISSION (0.7.2, A3) lets it through. The
		// capability is a no-op EITHER WAY — that is this test's whole claim —
		// but only the legacy-listed host is launchable, and conflating the two
		// questions is what this separation exists to prevent.
		admitted bool
	}{
		{"unclaimed host", capProviderSite(), false},
		{"unclaimed host still on the legacy scm_hosts list", providersConfig(
			[]types.GitProvider{githubRow(capProviderRowID, false, "https://github.com/acme")},
			"gitlab.example.com"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			door := providerRunDoor("POST /runs (legacy repo field)", body, nil)
			srv, w := door.fire(t, &capStore{enf: capProviderEnforced()}, tc.sc, door.member(t))
			if r := auditReasons(t, srv, "authz.denied"); slices.Contains(r, "capability_"+capWorkspaceProvider) {
				t.Errorf("authz.denied reasons = %v; a host no row claims must not be refused by the capability", r)
			}
			if tc.admitted {
				assertNotRefused(t, w, "the capability gate is a no-op for a host no row claims")
			}
		})
	}
}

// TestCapabilityWorkspaceProviderRefusalCostsNoState: the two doors that would
// otherwise write or launch must refuse BEFORE they do. A refusal that
// half-onboards a workspace, or leaves a re-pointed source behind, is worse than
// no gate at all.
func TestCapabilityWorkspaceProviderRefusalCostsNoState(t *testing.T) {
	member := ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleMember)
	const wsBody = `{"name":"app","sources":[{"type":"repo","source":"` + capProviderRepo + `"}]}`

	t.Run("POST /workspaces writes nothing", func(t *testing.T) {
		srv, st, _ := ownerHarness(t, runner.MemberMountPolicy{})
		st.caps, st.siteConfig = &capStore{enf: capProviderEnforced()}, capProviderSite()
		w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces", member, wsBody)
		assertProviderDenied(t, srv, w)
		if all, _ := st.ListWorkspaces(t.Context()); len(all) != 0 {
			t.Errorf("workspaces = %+v, want none — the refusal must precede the write", all)
		}
	})

	t.Run("PUT /workspaces/{id} leaves the stored sources alone", func(t *testing.T) {
		srv, st, _ := ownerHarness(t, runner.MemberMountPolicy{})
		st.caps, st.siteConfig = &capStore{enf: capProviderEnforced()}, capProviderSite()
		id := st.put(types.Workspace{OwnedBy: ownerMemberSub})
		before, _ := st.GetWorkspace(t.Context(), id)
		w := doSSO(t, srv, http.MethodPut, "/api/v1/workspaces/"+id.String(), member, wsBody)
		assertProviderDenied(t, srv, w)
		after, err := st.GetWorkspace(t.Context(), id)
		if err != nil {
			t.Fatalf("get workspace: %v", err)
		}
		if !slices.EqualFunc(before.Sources, after.Sources, workspaceSourceContentEqual) {
			t.Errorf("sources = %+v, want the pre-edit %+v — the refusal must precede the write", after.Sources, before.Sources)
		}
	})

	// The same PUT from a member who HOLDS the row lands, which is what makes
	// the assertion above mean "refused" rather than "this fixture never writes".
	t.Run("PUT /workspaces/{id} with the grant does re-point them", func(t *testing.T) {
		srv, st, _ := ownerHarness(t, runner.MemberMountPolicy{})
		st.caps = &capStore{enf: capProviderEnforced(), grants: capProviderAllow(ownerMemberSub)}
		st.siteConfig = capProviderSite()
		id := st.put(types.Workspace{OwnedBy: ownerMemberSub})
		w := doSSO(t, srv, http.MethodPut, "/api/v1/workspaces/"+id.String(), member, wsBody)
		assertNotRefused(t, w, "a member holding the row's id may re-point onto it")
		after, err := st.GetWorkspace(t.Context(), id)
		if err != nil {
			t.Fatalf("get workspace: %v", err)
		}
		if len(after.Sources) != 1 || after.Sources[0].Source != capProviderRepo {
			t.Errorf("sources = %+v, want the edit applied", after.Sources)
		}
	})
}
