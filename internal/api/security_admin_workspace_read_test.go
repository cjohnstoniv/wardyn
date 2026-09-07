// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The workspace access rule used to be spelled with three different predicates,
// and a security_admin got three different answers about ONE member-owned row:
// the LIST handed it over (isSecurityOperator), the direct READ answered the
// foreign-workspace 404 (ownsWorkspaceOrAdmin -> isOperator), and the EGRESS
// WRITE succeeded (securityOps, no ownership check at all).
//
// The read was the odd one out, and it was the harmful one: the tier could write
// a workspace's denylist but could not read /observed-egress, the observed
// traffic that is the INPUT to that decision.
package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type wsReadStore struct {
	store.Store
	ws types.Workspace
}

func (s *wsReadStore) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	return []types.Workspace{s.ws}, nil
}
func (s *wsReadStore) GetWorkspace(_ context.Context, id uuid.UUID) (types.Workspace, error) {
	if id == s.ws.ID {
		return s.ws, nil
	}
	return types.Workspace{}, store.ErrNotFound
}
func (s *wsReadStore) SetWorkspaceApprovedEgress(_ context.Context, id uuid.UUID, d []string) (types.Workspace, error) {
	if id != s.ws.ID {
		return types.Workspace{}, store.ErrNotFound
	}
	s.ws.ApprovedEgress = d
	return s.ws, nil
}
func (s *wsReadStore) UpdateWorkspace(_ context.Context, id uuid.UUID, ws types.Workspace) (types.Workspace, error) {
	if id != s.ws.ID {
		return types.Workspace{}, store.ErrNotFound
	}
	s.ws.Name = ws.Name
	return s.ws, nil
}

// workspaceReadRoutes is the set DERIVED from routeMatrix rather than listed:
// every classMember GET under /workspaces/{id}. getWorkspaceReadable's four call
// sites are exactly these, and deriving means a FIFTH read route arriving on the
// old predicate fails here instead of passing quietly.
func workspaceReadRoutes() []string {
	var out []string
	for key, rc := range routeMatrix {
		if rc.class != classMember {
			continue
		}
		method, pattern, ok := strings.Cut(key, " ")
		if !ok || method != http.MethodGet {
			continue
		}
		if strings.HasPrefix(pattern, "/api/v1/workspaces/{id}") {
			out = append(out, key)
		}
	}
	return out
}

func newWorkspaceReadServer(t *testing.T, ownedBy string) (*Server, *wsReadStore) {
	t.Helper()
	st := &wsReadStore{ws: types.Workspace{
		ID: uuid.New(), Name: "alices-ws", OwnedBy: ownedBy, Status: types.WorkspaceScanned,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
	}}
	h := newHarness(t)
	h.srv.cfg.Store = st
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	h.srv.router = h.srv.routes()
	return h.srv, st
}

// TestSecurityAdminReadsForeignWorkspace pins the widened READ, and pins the
// WRITE boundary in the SAME test — a test that only proved the read now works
// would not notice if the split accidentally widened the write.
//
// Counterfactual: point getWorkspaceReadable back at ownsWorkspaceOrAdmin and
// every read arm below reddens WITH THE 404, while the write arms stay green —
// which is exactly the three-predicate state this closes.
func TestSecurityAdminReadsForeignWorkspace(t *testing.T) {
	const memberSub = "sub-ws-owner"
	routes := workspaceReadRoutes()
	// EXACT SET, not a count: getWorkspaceReadable serves these three at member
	// class, and GET .../env-as-code — its fourth consumer until F287 — moved to
	// owner-or-super because its emitted files render the operator's authored
	// environment whole. It is covered below on its own terms, so a route
	// leaving OR joining classMember still fails here rather than quietly
	// shrinking what this test claims.
	wantRoutes := []string{
		"GET /api/v1/workspaces/{id}",
		"GET /api/v1/workspaces/{id}/build",
		"GET /api/v1/workspaces/{id}/observed-egress",
	}
	slices.Sort(routes)
	if !slices.Equal(routes, wantRoutes) {
		t.Fatalf("derived classMember workspace GET routes %v, want %v — the getter's member-class consumer set moved; "+
			"decide the new route's tier and update this list with it", routes, wantRoutes)
	}

	sec := ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin)
	member := ssoSession(t, memberSub, "owner@corp.example", oidc.RoleMember)
	other := ssoSession(t, "sub-someone-else", "else@corp.example", oidc.RoleMember)

	for _, key := range routes {
		method, pattern, _ := strings.Cut(key, " ")
		t.Run(key, func(t *testing.T) {
			srv, st := newWorkspaceReadServer(t, memberSub)
			p := buildPath(pattern, st.ws.ID.String())

			// VACUITY CONTROL: a non-existent id must 404 on this route, or a
			// non-404 below would prove nothing about ownership.
			if w := doSSO(t, srv, method, buildPath(pattern, uuid.New().String()), sec, ""); w.Code != http.StatusNotFound {
				t.Fatalf("a non-existent workspace id gave %d, want 404 — this probe never reaches the workspace "+
					"lookup, so the assertion below is vacuous; body=%s", w.Code, w.Body.String())
			}

			// THE CHANGE: the tier that governs this workspace's egress can now
			// read it.
			if w := doSSO(t, srv, method, p, sec, ""); w.Code == http.StatusNotFound {
				t.Errorf("security_admin got the foreign-workspace 404 reading a row it may already list AND "+
					"rewrite the egress of — the tier is acting blind on its own stated purpose; body=%s", w.Body.String())
			}

			// THE BOUND, unchanged: another MEMBER still gets the byte-identical
			// 404. The read widened to a TIER, not to everyone.
			if w := doSSO(t, srv, method, p, other, ""); w.Code != http.StatusNotFound {
				t.Errorf("a foreign MEMBER got %d, want the byte-identical 404 — the read widened past the admin "+
					"tiers; body=%s", w.Code, w.Body.String())
			}
			// And the owner still reads their own.
			if w := doSSO(t, srv, method, p, member, ""); w.Code == http.StatusNotFound {
				t.Errorf("the workspace's OWNER was 404'd on their own row; body=%s", w.Body.String())
			}
		})
	}

	// THE GETTER'S FOURTH CONSUMER, on its own terms (F287). GET
	// .../env-as-code left classMember because its emitted files render the
	// operator's authored environment whole — the FROM line naming the internal
	// registry coordinate the workspace reads blank, the site-config artifact
	// redirects /site-config is admin-only for, the scanned setup commands. The
	// tier that governs this workspace's EGRESS is not the tier that reads its
	// build recipe.
	t.Run("GET env-as-code did not widen with the read", func(t *testing.T) {
		srv, st := newWorkspaceReadServer(t, memberSub)
		p := fmt.Sprintf("/api/v1/workspaces/%s/env-as-code", st.ws.ID)
		for who, session := range map[string]*http.Cookie{"security_admin": sec, "a foreign member": other} {
			if w := doSSO(t, srv, http.MethodGet, p, session, ""); w.Code == http.StatusOK {
				t.Errorf("%s read the operator's committable environment (200) — this route moved to owner-or-super "+
					"with its operatorOnly write twin; body=%s", who, w.Body.String())
			}
		}
		// And the OWNER keeps it: the move is a tier, not a shutdown.
		if w := doSSO(t, srv, http.MethodGet, p, member, ""); w.Code == http.StatusNotFound || w.Code == http.StatusForbidden {
			t.Errorf("the workspace's OWNER lost their own env-as-code (%d); body=%s", w.Code, w.Body.String())
		}
	})

	// THE WRITE PREDICATE, in the same test and deliberately so.
	t.Run("the write tier did NOT widen with the read", func(t *testing.T) {
		srv, st := newWorkspaceReadServer(t, memberSub)
		p := fmt.Sprintf("/api/v1/workspaces/%s", st.ws.ID)

		// PUT /workspaces/{id} is classOwner via getWorkspaceAuthorized
		// (ownsWorkspaceOrAdmin -> isOperator). A security_admin must STILL be
		// refused: they may govern this workspace's egress, not rename it.
		w := doSSO(t, srv, http.MethodPut, p, sec, `{"name":"renamed-by-sec"}`)
		if w.Code != http.StatusNotFound && w.Code != http.StatusForbidden {
			t.Fatalf("security_admin PUT /workspaces/{id} = %d, want the refusal — the read split widened the "+
				"WRITE predicate too; body=%s", w.Code, w.Body.String())
		}
		if st.ws.Name != "alices-ws" {
			t.Fatalf("the workspace was RENAMED by a security_admin (name=%q) — getWorkspaceAuthorized no longer "+
				"holds the super-admin line", st.ws.Name)
		}

		// The egress write it SHOULD have stays working, so this arm cannot pass
		// by refusing everything.
		if w := doSSO(t, srv, http.MethodPut, p+"/approved-egress", sec, `{"domains":["x.example"]}`); w.Code != http.StatusOK {
			t.Fatalf("security_admin approved-egress = %d, want 200 — the tier lost the write it is FOR; body=%s",
				w.Code, w.Body.String())
		}
	})
}
