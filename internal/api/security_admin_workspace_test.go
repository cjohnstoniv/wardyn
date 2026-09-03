// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// security_admin_workspace_test.go asks the question the tier's own test
// doctrine stops short of: not "which tier is this route on", but "what can
// that tier DO once the router lets it through".
//
// TestAuthzMatrix and TestSecurityAdminRouteTier both probe every
// classAdmin/classSecurity route with buildPath(pattern, "x1") — a deliberately
// bogus, non-UUID id — and assert only via assertNotBlocked, documented as
// passing on any 2xx/4xx/5xx a handler reaches once authorization let it
// through. For an {id}-bearing route that means parseIDParam 400s before the
// handler's own authorization runs, so the entire assertion is "the router
// middleware did not 401/403". classOwner compensates by seeding an
// owned/foreign pair; classSecurity got neither that nor a feature test —
// grep RoleSecurityAdmin across the workspace and record test files returns
// nothing.
//
// That gap is not theoretical: it is how a route can sit on the wrong tier,
// gated perfectly consistently, while nothing asks what it then reaches.
//
// So this file seeds a REAL, FOREIGN, member-owned workspace and drives every
// classSecurity route that names one, with a security_admin session, asserting
// what the handler does with it.
package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// securityAdminWorkspaceExpectation states, per route, what a security_admin
// gets when the {id} names a workspace owned by SOMEONE ELSE — a member.
//
// reachesForeign is the load-bearing field. An ownership-scoped workspace route
// answers the byte-identical 404 getWorkspaceAuthorized writes
// (denyForeignWorkspace, helpers.go); a route that only checks EXISTENCE
// (getWorkspaceOr404, or no lookup at all) does not. That 404 is therefore the
// one observable that separates "this tier is scoped to what it owns" from
// "this tier acts on anything", and it is what every entry below pins.
type securityAdminWorkspaceExpectation struct {
	// body is a request body that survives the handler's own shape validation,
	// so the probe reaches the workspace lookup rather than stopping at a 400.
	body string
	// reachesForeign: the handler acts on a foreign member-owned workspace
	// rather than answering the ownership 404.
	reachesForeign bool
	// why is printed on failure — a reader who reddens this test needs the
	// argument, not just the expectation.
	why string
	// wrote, when set, is checked against the store afterwards: proof the
	// handler did not merely avoid the 404 but actually CHANGED the foreign
	// workspace. Only set where the write is the point of the route.
	wrote func(t *testing.T, ws types.Workspace)
}

func securityAdminWorkspaceExpectations() map[string]securityAdminWorkspaceExpectation {
	return map[string]securityAdminWorkspaceExpectation{
		// The org allow/denylist IS this tier's job — routes.go's securityOps
		// rationale names "hold the org allow/denylist" and "promote a
		// workspace's observed egress" as what the tier is FOR. A security
		// admin who could only edit egress on workspaces they personally own
		// would hold no org-wide list at all.
		"PUT /api/v1/workspaces/{id}/approved-egress": {
			body:           `{"domains":["approved.example"]}`,
			reachesForeign: true,
			why: "the securityOps rationale names holding the org allow/denylist as the tier's purpose; " +
				"scoping it to owned workspaces would make the tier meaningless",
			wrote: func(t *testing.T, ws types.Workspace) {
				t.Helper()
				if len(ws.ApprovedEgress) != 1 || ws.ApprovedEgress[0] != "approved.example" {
					t.Errorf("ApprovedEgress = %v, want the write to have landed on the foreign workspace", ws.ApprovedEgress)
				}
			},
		},
		"PUT /api/v1/workspaces/{id}/denied-egress": {
			body:           `{"domains":["denied.example"]}`,
			reachesForeign: true,
			why:            "the denylist half of the same org-wide list; deny is the direction the tier exists to assert",
			wrote: func(t *testing.T, ws types.Workspace) {
				t.Helper()
				if len(ws.DeniedEgress) != 1 || ws.DeniedEgress[0] != "denied.example" {
					t.Errorf("DeniedEgress = %v, want the write to have landed on the foreign workspace", ws.DeniedEgress)
				}
			},
		},
		"POST /api/v1/workspaces/{id}/record/{task}/promote-egress": {
			// {task} is filled with the same value as {id}, so no such recording
			// exists and the handler answers its own 422. That is past the
			// workspace lookup, which is what this file measures.
			body:           `{}`,
			reachesForeign: true,
			why:            "promoting a workspace's observed egress is named in the securityOps rationale",
		},

		// OPEN QUESTION — RECORDED, NOT ENDORSED.
		//
		// handleRecordWorkspace uses getWorkspaceOr404 (existence only), so a
		// security_admin reaching here LAUNCHES A RECORDING RUN inside another
		// human's workspace — which is reach INTO a run, the one thing the
		// securityOps rationale says the tier must never have ("never reach
		// INTO a run, never credential material, never the host"). The policy
		// lane is re-tiering this route to operatorOnly for exactly that
		// reason.
		//
		// This entry pins TODAY's behaviour so the change is VISIBLE rather
		// than silent. Two futures, both fine:
		//   - the re-tier lands: the route leaves classSecurity, drops out of
		//     the derived set below, and this entry is reported as stale.
		//   - someone instead adds an ownership check in the handler: this
		//     assertion goes red, and the fix is to flip reachesForeign to
		//     false — a deliberate edit, which is the point.
		"POST /api/v1/workspaces/{id}/record": {
			body:           `{"name":"probe session"}`,
			reachesForeign: true,
			why: "RECORDED, NOT ENDORSED: this route currently admits a security_admin over any member's workspace " +
				"(getWorkspaceOr404, existence only). If you just scoped it, flip reachesForeign to false here",
		},
	}
}

// TestSecurityAdminOnForeignWorkspace is the compensating arm classSecurity
// never had.
//
// The route set is DERIVED from routeMatrix rather than hard-coded, so a
// re-tiering elsewhere changes what this test covers without editing it — and a
// NEW classSecurity route naming a workspace fails until someone states what it
// should do with a foreign one. That completeness check is the part that
// outlives the four routes below.
func TestSecurityAdminOnForeignWorkspace(t *testing.T) {
	expectations := securityAdminWorkspaceExpectations()

	// Derive: every classSecurity route whose path names a workspace {id}.
	derived := map[string]bool{}
	for key, rc := range routeMatrix {
		if rc.class != classSecurity {
			continue
		}
		if _, pattern, ok := strings.Cut(key, " "); ok && strings.HasPrefix(pattern, "/api/v1/workspaces/{id}") {
			derived[key] = true
		}
	}
	if len(derived) == 0 {
		t.Fatal("no classSecurity workspace routes found in routeMatrix — either they all moved tier " +
			"(delete this test with the reasoning) or the derivation broke and this file is asserting nothing")
	}

	// COMPLETENESS, both ways.
	for key := range derived {
		if _, ok := expectations[key]; !ok {
			t.Errorf("classSecurity route %q names a workspace but has no stated expectation. "+
				"What may a security_admin do to a workspace a MEMBER owns here? Add an entry — "+
				"the tier's router classification says nothing about what its handler reaches", key)
		}
	}
	for key := range expectations {
		if !derived[key] {
			// A note, not a failure: the tier split is edited by more than one
			// lane, and a route leaving classSecurity is the fix landing, not a
			// regression. Delete the entry when you see this.
			t.Logf("NOTE: expectation for %q is stale — that route is no longer a classSecurity workspace route", key)
		}
	}

	const memberSub = "sub-ws-owner"
	secSess := ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin)
	memberSess := ssoSession(t, memberSub, "wsowner@corp.example", oidc.RoleMember)
	adminSess := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)

	for key := range derived {
		exp, ok := expectations[key]
		if !ok {
			continue // already reported above
		}
		method, pattern, _ := strings.Cut(key, " ")
		t.Run(key, func(t *testing.T) {
			srv, ast, _, _ := newAuthzMatrixServer(t)

			// A workspace owned by a MEMBER — not by the caller, not by an
			// operator. This is the fixture the whole finding is about.
			wsID := uuid.New()
			ast.mu.Lock()
			ast.workspaces[wsID] = types.Workspace{
				ID: wsID, Name: "foreign-ws", OwnedBy: memberSub, Status: types.WorkspaceScanned,
				Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
			}
			ast.mu.Unlock()
			p := buildPath(pattern, wsID.String())

			// VACUITY CONTROL, per route. Every assertion below reads a
			// NON-404 as "the handler acted on a foreign workspace" — which is
			// worthless unless this route's lookup can produce a 404 at all,
			// and unless the probe genuinely reaches it. So first drive the
			// SAME method and body at a workspace id that does not exist: a 404
			// there proves the request got past parseIDParam, past the body
			// decode, and into the workspace lookup, and that the lookup's
			// not-found answer is observable in the status.
			missing := buildPath(pattern, uuid.New().String())
			if mw := doSSO(t, srv, method, missing, secSess, exp.body); mw.Code != http.StatusNotFound {
				t.Fatalf("a NON-EXISTENT workspace id gave %d, want 404 — this probe never reaches the workspace "+
					"lookup, so the foreign-workspace assertion below would pass vacuously; body=%s",
					mw.Code, mw.Body.String())
			}

			w := doSSO(t, srv, method, p, secSess, exp.body)
			t.Logf("security_admin -> status %d body=%s", w.Code, strings.TrimSpace(w.Body.String()))

			// The ownership 404 is the observable that separates a scoped route
			// from an unscoped one — nothing else about the status is asserted,
			// because a handler's own downstream 422/500 is not this file's
			// business.
			gotForeign404 := w.Code == http.StatusNotFound
			if exp.reachesForeign && gotForeign404 {
				t.Fatalf("security_admin got 404 on a foreign member-owned workspace (body=%s) — the route is "+
					"ownership-scoped, but the expectation says it should reach: %s", w.Body.String(), exp.why)
			}
			if !exp.reachesForeign && !gotForeign404 {
				t.Fatalf("security_admin reached a foreign member-owned workspace (status=%d body=%s), want the "+
					"ownership 404: %s", w.Code, w.Body.String(), exp.why)
			}
			// Never 401/403 either — the tier is on this route by construction.
			if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
				t.Fatalf("security_admin was refused at the gate (%d) on a classSecurity route; "+
					"routeMatrix and the router disagree", w.Code)
			}

			if exp.wrote != nil {
				ast.mu.Lock()
				got := ast.workspaces[wsID]
				ast.mu.Unlock()
				exp.wrote(t, got)
			}

			// CONTROLS, so a green above cannot mean "this route admits
			// everyone".
			//
			// The workspace's own OWNER is a member, and a member must still be
			// refused at the tier gate — being the owner does not open a
			// security-tier route.
			ownerW := doSSO(t, srv, method, p, memberSess, exp.body)
			if ownerW.Code != http.StatusForbidden {
				t.Errorf("the workspace's OWNING member got %d on a classSecurity route, want 403 — "+
					"ownership must not open a tier route; body=%s", ownerW.Code, ownerW.Body.String())
			}
			// And a super admin reaches whatever the security admin reached:
			// the tiers overlap on this surface (they deliberately do not nest),
			// so a route the security tier can act on must not be closed to the
			// admin tier.
			adminW := doSSO(t, srv, method, p, adminSess, exp.body)
			if adminW.Code == http.StatusUnauthorized || adminW.Code == http.StatusForbidden {
				t.Errorf("admin was refused (%d) on a route the security_admin reached; body=%s",
					adminW.Code, adminW.Body.String())
			}
			if (adminW.Code == http.StatusNotFound) != gotForeign404 {
				t.Errorf("admin (%d) and security_admin (%d) disagree about reaching a foreign workspace — "+
					"the ownership rule must not depend on WHICH admin tier is asking", adminW.Code, w.Code)
			}
		})
	}
}

// TestSecurityAdminForeignWorkspaceProbeIsNotVacuous guards the guard. The
// assertions above hinge on the ownership 404 being DISTINGUISHABLE, so this
// pins that the same fixture and the same helper produce that 404 on a route
// that IS ownership-scoped. Without it, a change that made every workspace
// lookup return 200 would leave the test above green and meaningless.
func TestSecurityAdminForeignWorkspaceProbeIsNotVacuous(t *testing.T) {
	srv, ast, _, _ := newAuthzMatrixServer(t)
	const memberSub = "sub-ws-owner"
	wsID := uuid.New()
	ast.mu.Lock()
	ast.workspaces[wsID] = types.Workspace{
		ID: wsID, Name: "foreign-ws", OwnedBy: memberSub, Status: types.WorkspaceScanned,
	}
	ast.mu.Unlock()

	// GET /workspaces/{id} is classOwner: getWorkspaceReadable scopes it, so a
	// DIFFERENT member gets the byte-identical 404 this file's assertions read.
	other := ssoSession(t, "sub-someone-else", "else@corp.example", oidc.RoleMember)
	w := doSSO(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/workspaces/%s", wsID), other, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("an ownership-scoped route gave %d for a foreign workspace, want 404 — the 404 the sibling test "+
			"treats as 'scoped' is not actually produced, so those assertions prove nothing; body=%s", w.Code, w.Body.String())
	}
}
