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
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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

// TestSecurityAdminForeignWorkspaceFieldByField is F246.
//
// The finding is an ASYMMETRY on one stated axis: routes.go refuses to widen
// GET /sources to this tier because a local_dir source carries "the /srv NFS
// path ... credential material and the host, two of the three axes this tier is
// DEFINED never to reach" — while GET /workspaces/{id} answered the same
// session 200 with that same path in it.
//
// The resolution is not to widen /sources but to hold BOTH to the one rule: the
// tier never reaches the host axis, so a route either withholds it (the
// workspace reads, at workspaceReadSecurity) or stays narrowed (those four,
// which serve whole documents nobody projects). This pins the withholding FIELD
// BY FIELD rather than by substring, because "the path is absent from the body"
// and "the path field is blank" are different claims and only the second is the
// projection actually working.
func TestSecurityAdminForeignWorkspaceFieldByField(t *testing.T) {
	srv, st := newTopologyWorkspaceServer(t, "sub-ws-owner")
	sec := ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin)

	// THE ASYMMETRY IS REAL, and this is the half that makes the rule load-
	// bearing rather than decorative: the same session, the same datum, two
	// answers.
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/sources", sec, ""); w.Code != http.StatusForbidden {
		t.Fatalf("security_admin GET /sources = %d, want 403 — this test is about the tier that route refuses", w.Code)
	}
	w := doSSO(t, srv, http.MethodGet, "/api/v1/workspaces/"+st.ws.ID.String(), sec, "")
	if w.Code != http.StatusOK {
		t.Fatalf("security_admin GET /workspaces/{id} = %d, want 200 — the tier governs this workspace's egress "+
			"and may read it; body=%s", w.Code, w.Body.String())
	}

	var got types.Workspace
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}

	// ── the HOST axis, field by field ──
	for i, src := range got.Sources {
		if src.Type == types.WorkspaceSourceTypeLocalDir && src.Path != "" {
			t.Errorf("sources[%d].path = %q, want blank — this is the exact datum /sources answers this tier 403 for",
				i, src.Path)
		}
	}
	if got.Source != "" {
		t.Errorf("source = %q, want blank — the derived single-source mirror carries the same host path", got.Source)
	}
	if got.BaseImage != nil && got.BaseImage.Image != "" {
		t.Errorf("base_image.image = %q, want blank — the operator's authored registry coordinate is the same class "+
			"/base-images answers this tier 403 for", got.BaseImage.Image)
	}
	for key := range got.Requirements {
		if typ, _, ok := types.SplitRequirementKey(key); ok && (typ == "secret" || typ == "write") {
			t.Errorf("requirements has %q — a secret: key names a stored credential and a write: key is the host "+
				"path just blanked above", key)
		}
	}

	// ── what the tier KEEPS, because withholding it would break the decision
	// this tier is widened to make ──
	if got.Name != "payments" {
		t.Errorf("name = %q, want the workspace to remain identifiable", got.Name)
	}
	if got.OwnedBy == "" {
		t.Errorf("owned_by is blank — the tier deciding this workspace's egress has to know whose it is")
	}
	var sawEgressReq bool
	for key := range got.Requirements {
		if typ, _, ok := types.SplitRequirementKey(key); ok && typ == "egress" {
			sawEgressReq = true
		}
	}
	if !sawEgressReq {
		t.Errorf("requirements has no egress: key — egress is the INPUT to the decision this tier is widened for, "+
			"and it cannot decide blind (requirements=%v)", got.Requirements)
	}

	// ── the scanned profile republishes both axes under its own keys ──
	var profile map[string]json.RawMessage
	if len(got.Profile) > 0 {
		if err := json.Unmarshal(got.Profile, &profile); err != nil {
			t.Fatalf("decode profile: %v", err)
		}
	}
	for _, k := range profileHostAxisKeys {
		if _, ok := profile[k]; ok {
			t.Errorf("profile still carries %q — the scan result travels in this same document and republishes the "+
				"host axis under its own keys", k)
		}
	}
	for _, k := range profileEgressAxisKeys {
		if _, ok := profile[k]; !ok {
			t.Errorf("profile lost %q — the egress axis is what this tier reads the profile FOR", k)
		}
	}
}

// TestSecurityTierNoteStatesOneRule is F246's other half: routes.go states the
// axis this tier never reaches, and a reader comparing "/sources is 403" with
// "/workspaces/{id} is 200" needs the note to say WHY both are true at once.
// Otherwise the next reader resolves the asymmetry the other way and widens the
// four narrowed reads.
func TestSecurityTierNoteStatesOneRule(t *testing.T) {
	src, err := os.ReadFile("routes.go")
	if err != nil {
		t.Fatalf("read routes.go: %v", err)
	}
	note := string(src)
	i := strings.Index(note, "WHY THE OPERATOR-TOPOLOGY READS ARE NOT HERE")
	if i < 0 {
		t.Fatal("routes.go no longer carries the operator-topology tier note this pin is keyed to")
	}
	j := strings.Index(note[i:], "securityOps := r.With(")
	if j < 0 {
		t.Fatal("could not bound the tier note")
	}
	block := note[i : i+j]
	for _, want := range []struct{ frag, why string }{
		{"workspaceReadSecurity", "name the mechanism that lets a member-class workspace read coexist with these four narrowed ones"},
		{"/workspaces", "the asymmetry a reader will notice is with the workspace read; a note that never mentions it invites widening these four instead"},
	} {
		if !strings.Contains(block, want.frag) {
			t.Errorf("the operator-topology tier note does not mention %q — %s\nnote=%s", want.frag, want.why, block)
		}
	}
}
