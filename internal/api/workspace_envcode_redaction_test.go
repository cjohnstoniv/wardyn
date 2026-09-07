// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// envcodeCorpRegistry is the operator's corporate mirror: topology, and exactly
// the class of datum R1 narrowed GET /site-config to admin-only to withhold.
const envcodeCorpRegistry = "https://nexus.corp.internal/repository/npm"

// envcodeRedirectStore is the topology fixture with the field the EMITTER
// actually reads.
//
// It matters that this is a separate double. r3TopologyStore's site-config sets
// ArtifactOverrides, and artifactBaseURLs (artifact_redirect.go:140) reads
// EgressRedirects — so on that fixture the artifact bases are always nil and the
// leak F288 names cannot occur, whatever the route does. A pin written against
// it would be green for a reason that has nothing to do with the property.
type envcodeRedirectStore struct{ *r3TopologyStore }

func (envcodeRedirectStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: "https://registry.npmjs.org", To: envcodeCorpRegistry, Ecosystem: "npm"},
	}}, nil
}

func newEnvcodeRedirectServer(t *testing.T, ownedBy string) (*Server, string) {
	t.Helper()
	srv, st := newTopologyWorkspaceServer(t, ownedBy)
	srv.cfg.Store = envcodeRedirectStore{st}
	srv.router = srv.routes()
	return srv, st.ws.ID.String()
}

// TestEnvAsCodeWithholdsTheArtifactRegistryFromNonFullReaders is F288.
//
// GET /workspaces/{id}/env-as-code folds the operator's corporate
// artifact-registry base URLs (site_config.egress_redirects[].to) into the
// emitted per-ecosystem config, and the route was fed by getWorkspaceReadable —
// so any authenticated member reading an operator-owned workspace got
// https://nexus.corp.internal/... back, 200, while GET /site-config answered
// that same session 403. One disclosure class, two answers.
//
// The tier moved rather than the field being projected, because the whole point
// of the response is that it is COMMITTABLE: there is no per-field projection
// that leaves it useful. That move landed with F287; what did not land is
// anything that exercises THIS datum. The F287 sweep asserts the base image and
// the local_dir host path, on a fixture whose site-config carries no
// EgressRedirects at all — so the artifact-registry half of the reason the tier
// moved was pinned by nothing.
func TestEnvAsCodeWithholdsTheArtifactRegistryFromNonFullReaders(t *testing.T) {
	// THE POSITIVE CONTROL FIRST, and it is load-bearing twice over: it proves
	// the fixture really does emit the corporate base (so the refusals below are
	// refusing something that exists), and it proves the tier move did not close
	// the leak by breaking the feature. An env-as-code that emitted no artifact
	// config would pass every assertion in this test for the wrong reason.
	t.Run("a super admin still gets the corporate mirror", func(t *testing.T) {
		srv, id := newEnvcodeRedirectServer(t, "")
		session := ssoSession(t, "admin-1", "admin@corp.example", oidc.RoleAdmin)
		w := doSSO(t, srv, http.MethodGet, "/api/v1/workspaces/"+id+"/env-as-code", session, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET .../env-as-code as a super admin = %d, want 200: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), envcodeCorpRegistry) {
			t.Fatalf("the emitted env-as-code carries no artifact-registry base — the fixture does not exercise "+
				"the datum this test is about, so its refusals below would prove nothing\nbody=%s", w.Body.String())
		}
	})

	// …and the readers the finding names. A security admin is included because
	// the tier that decides this route is workspaceReadFull, not "is an
	// operator": the security tier reads plenty of operator surfaces and this is
	// deliberately not one of them.
	for name, session := range map[string]*http.Cookie{
		"plain member":   ssoSession(t, "sub-plain-member", "m@corp.example", oidc.RoleMember),
		"security admin": ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin),
	} {
		t.Run(name+" is refused, and the refusal carries nothing", func(t *testing.T) {
			srv, id := newEnvcodeRedirectServer(t, "")
			w := doSSO(t, srv, http.MethodGet, "/api/v1/workspaces/"+id+"/env-as-code", session, "")
			if w.Code != http.StatusForbidden {
				t.Fatalf("GET .../env-as-code as a %s = %d, want the 403 its operatorOnly write twin gives: %s",
					name, w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), envcodeCorpRegistry) {
				t.Errorf("the emitted config handed a %s the operator's corporate artifact registry %q, while "+
					"GET /site-config answers that same session 403 — one disclosure class with two answers\nbody=%s",
					name, envcodeCorpRegistry, w.Body.String())
			}
			// Named separately from the code check because a route that answered
			// 200-with-an-empty-body would satisfy neither, and a refusal that
			// echoed the host in its message would satisfy only the first.
			if strings.Contains(w.Body.String(), "nexus.corp.internal") {
				t.Errorf("the REFUSAL itself named the corporate registry host: %s", w.Body.String())
			}
		})
	}

	// THE OWNER of a member-owned workspace is the other half of
	// workspaceReadFull, and they are admitted: this is a tier rule, not an
	// operator rule. Without this arm the fix would be indistinguishable from
	// "nobody but a super admin may export", which is a different (and wrong)
	// tier.
	t.Run("the workspace's own member owner still gets it", func(t *testing.T) {
		const owner = "sub-ws-owner"
		srv, id := newEnvcodeRedirectServer(t, owner)
		session := ssoSession(t, owner, "owner@corp.example", oidc.RoleMember)
		w := doSSO(t, srv, http.MethodGet, "/api/v1/workspaces/"+id+"/env-as-code", session, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET .../env-as-code as the owning member = %d, want 200: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), envcodeCorpRegistry) {
			t.Errorf("the owner's export lost the corporate mirror: an exported workspace that pulls from the "+
				"public registry is the feature not working\nbody=%s", w.Body.String())
		}
	})
}
