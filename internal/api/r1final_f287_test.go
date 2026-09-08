// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// F287: redactWorkspaceForRead reached 2 of the 4 routes getWorkspaceReadable
// feeds. GET /workspaces/{id}/build answered a plain member with
// {"image":"registry.corp.internal/base:1"} and GET /workspaces/{id}/env-as-code
// with a Dockerfile whose first line was `FROM registry.corp.internal/base:1` —
// both 200 — while GET /workspaces/{id} on the same row in the same session
// returned "base_image":{"kind":"registry"} with the image blanked.
//
// The two routes get different answers because the leak has different shapes.
// /build carries ONE authored field beside its useful ones, so it is projected.
// /env-as-code renders the operator's whole authored environment (the FROM line,
// the site-config artifact redirects /site-config is admin-only for, the scanned
// setup commands) and has no useful residue once those are gone, so the tier
// moves to owner-or-super — where its own write twin already sat.
//
// The pin walks the getter's WHOLE consumer set, not the two routes the original
// finding named, so a fifth consumer cannot be added silently.

// f287Sessions are the two non-full readers of an operator-owned workspace.
func f287Sessions(t *testing.T) map[string]*http.Cookie {
	t.Helper()
	return map[string]*http.Cookie{
		"plain member":   ssoSession(t, "sub-plain-member", "m@corp.example", oidc.RoleMember),
		"security admin": ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin),
	}
}

// TestF287_EveryReadableRouteProjectsTheAuthoredBaseImage is the consumer-set
// sweep: NO route fed by getWorkspaceReadable may hand a non-full reader the
// operator's authored base image.
func TestF287_EveryReadableRouteProjectsTheAuthoredBaseImage(t *testing.T) {
	for name, session := range f287Sessions(t) {
		t.Run(name, func(t *testing.T) {
			srv, st := newTopologyWorkspaceServer(t, "")
			id := st.ws.ID.String()
			for _, path := range []string{
				"/api/v1/workspaces",
				"/api/v1/workspaces/" + id,
				"/api/v1/workspaces/" + id + "/build",
				"/api/v1/workspaces/" + id + "/observed-egress",
			} {
				w := doSSO(t, srv, http.MethodGet, path, session, "")
				if w.Code != http.StatusOK {
					t.Fatalf("GET %s = %d, want 200: %s", path, w.Code, w.Body.String())
				}
				if strings.Contains(w.Body.String(), r3TopologyImage) {
					t.Errorf("GET %s leaked the operator's authored base image %q to a %s, while GET /workspaces/{id} blanks it on the same row in the same session\nbody=%s",
						path, r3TopologyImage, name, w.Body.String())
				}
				if strings.Contains(w.Body.String(), r3TopologyHostPath) {
					t.Errorf("GET %s leaked the local_dir HOST PATH %q to a %s\nbody=%s", path, r3TopologyHostPath, name, w.Body.String())
				}
			}

			// The fourth consumer answers by REFUSING rather than projecting.
			w := doSSO(t, srv, http.MethodGet, "/api/v1/workspaces/"+id+"/env-as-code", session, "")
			if w.Code != http.StatusForbidden {
				t.Fatalf("GET .../env-as-code as a %s = %d, want the 403 its operatorOnly write twin gives: %s", name, w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), r3TopologyImage) {
				t.Errorf("the refusal itself carried the base image: %s", w.Body.String())
			}
		})
	}
}

// TestF287_BuildKeepsWhatTheMemberNeeds is the counterfactual: the projection
// must not be satisfied by emptying the response. The BUILT image is the one the
// member's own run executes and stays, exactly as image_ref stays on
// GET /workspaces/{id}.
func TestF287_BuildKeepsWhatTheMemberNeeds(t *testing.T) {
	const built = "ghcr.io/acme/ws-payments:built"
	srv, st := newTopologyWorkspaceServer(t, "")
	st.ws.ImageRef = built
	st.ws.BuiltProfileHash = "" // force the honest "none" arm, which names the AUTHORED image
	member := ssoSession(t, "sub-plain-member", "m@corp.example", oidc.RoleMember)

	w := doSSO(t, srv, http.MethodGet, "/api/v1/workspaces/"+st.ws.ID.String()+"/build", member, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET .../build = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, r3TopologyImage) {
		t.Fatalf("the authored base image survived: %s", body)
	}
	// The member still learns the honest state and WHY a run would be refused —
	// over-redaction here turns a clear refusal into an opaque one.
	if !strings.Contains(body, `"state"`) || !strings.Contains(body, "image builder is not wired") {
		t.Errorf("the build state/detail went with the image; a member launching against this workspace needs both\nbody=%s", body)
	}
}

// TestF287_OwnerAndSuperStillSeeEverything: the projection is a READER rule.
func TestF287_OwnerAndSuperStillSeeEverything(t *testing.T) {
	const owner = "sub-ws-owner"
	for _, tc := range []struct {
		name    string
		ownedBy string
		session *http.Cookie
	}{
		{"the workspace's owner", owner, ssoSession(t, owner, "owner@corp.example", oidc.RoleMember)},
		{"a super admin", "", ssoSession(t, "admin-1", "admin@corp.example", oidc.RoleAdmin)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st := newTopologyWorkspaceServer(t, tc.ownedBy)
			id := st.ws.ID.String()
			w := doSSO(t, srv, http.MethodGet, "/api/v1/workspaces/"+id+"/build", tc.session, "")
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), r3TopologyImage) {
				t.Errorf("GET .../build = %d without the authored base image; the full tier is not redacted: %s", w.Code, w.Body.String())
			}
			w = doSSO(t, srv, http.MethodGet, "/api/v1/workspaces/"+id+"/env-as-code", tc.session, "")
			if w.Code != http.StatusOK {
				t.Errorf("GET .../env-as-code = %d, want 200 for the full tier: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), r3TopologyImage) {
				t.Errorf("the full tier's env-as-code lost its FROM line: %s", w.Body.String())
			}
		})
	}
}

var _ = types.Workspace{}
