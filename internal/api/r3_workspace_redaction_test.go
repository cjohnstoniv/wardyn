// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// r3TopologyStore serves ONE workspace carrying every field class R1 declared
// member-forbidden: a local_dir HOST PATH, an internal registry image, a stored
// secret NAME and an internal egress host.
type r3TopologyStore struct {
	store.Store
	ws types.Workspace
}

func (s *r3TopologyStore) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	return []types.Workspace{s.ws}, nil
}

// ListRuns completes the double for the observed-egress read, which scans the
// runs that referenced this workspace. Empty: the route's own ownsRunOrAdmin
// filter is pinned elsewhere; here it is one of the four consumers the
// redaction sweep must cover.
func (s *r3TopologyStore) ListRuns(context.Context) ([]types.AgentRun, error) { return nil, nil }

// GetSiteConfig completes the double for the env-as-code generation, which
// folds the operator's artifact-registry redirects into the emitted files.
func (s *r3TopologyStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: "https://registry.npmjs.org", To: "https://nexus.corp.internal/npm", Ecosystem: "npm"},
	}}, nil
}

func (s *r3TopologyStore) GetWorkspace(_ context.Context, id uuid.UUID) (types.Workspace, error) {
	if id == s.ws.ID {
		return s.ws, nil
	}
	return types.Workspace{}, store.ErrNotFound
}

const (
	r3TopologyHostPath = "/srv/nfs-prod/payments"
	r3TopologyImage    = "registry.corp.internal/base:1"
	r3TopologySecret   = "acme-prod-db-password"
	r3TopologyEgress   = "artifacts.corp.internal"
	// The SCANNED PROFILE republishes the same three axes under its own keys —
	// the residue F245's first fix left: redactWorkspaceForRead projected
	// Sources/BaseImage/Requirements and never touched Workspace.Profile, which
	// travels in the SAME response.
	r3TopologyLeakPath  = "/srv/nfs-prod/payments/config/prod.env"
	r3TopologySuggested = "candidate.corp.internal"
)

// r3TopologyProfile is a scanned internal/workspacescan profile carrying one
// datum of each member-forbidden class: a stored secret NAME, host paths, and
// internal egress hosts.
const r3TopologyProfile = `{"languages":["go"],"confidence":"high","source":"deterministic",` +
	`"egress_domains":["` + r3TopologyEgress + `"],` +
	`"suggested_egress":["` + r3TopologySuggested + `"],` +
	`"required_secrets":[{"name":"` + r3TopologySecret + `"}],` +
	`"secret_files_present":["` + r3TopologyLeakPath + `"],` +
	`"leak_findings":[{"path":"` + r3TopologyLeakPath + `","kind":"aws_key","line":3}]}`

func newTopologyWorkspaceServer(t *testing.T, ownedBy string) (*Server, *r3TopologyStore) {
	t.Helper()
	st := &r3TopologyStore{ws: types.Workspace{
		ID: uuid.New(), Name: "payments", OwnedBy: ownedBy, Status: types.WorkspaceScanned,
		Kind: types.WorkspaceKindLocalDir, Source: r3TopologyHostPath,
		Sources: []types.WorkspaceSource{{
			Type: types.WorkspaceSourceTypeLocalDir, Path: r3TopologyHostPath,
			Target: "/home/agent/work", Writable: true,
		}},
		BaseImage: &types.WorkspaceBaseImage{Kind: "registry", Image: r3TopologyImage},
		Requirements: map[string]types.WorkspaceRequirement{
			"secret:" + r3TopologySecret:      {},
			"egress:" + r3TopologyEgress:      {},
			"write:" + r3TopologyHostPath:     {},
			"integration:git_host:github.com": {},
		},
		Profile: json.RawMessage(r3TopologyProfile),
	}}
	st.ws.EffectiveRequirements = st.ws.Requirements
	h := newHarness(t)
	h.srv.cfg.Store = st
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	h.srv.router = h.srv.routes()
	return h.srv, st
}

// TestWorkspaceReadRedaction is F245 (the member arm) and F246 (the security
// arm).
//
// R1 narrowed GET /site-config, /sources, /sources/{id} and /base-images to
// admin-only precisely because those documents carry an upstream-proxy password
// ref, a database-password requirement key and the /srv NFS path of a local_dir
// source. GET /workspaces{,/{id}} served the SAME class of datum to a plain
// member, 200 OK, while that identical session was 403'd on all three of the
// others — one disclosure class with two answers.
//
// Projection rather than a tier move, because unlike those four routes these
// have real member callers (the console's workspace list and detail are how a
// member picks what to run).
func TestWorkspaceReadRedaction(t *testing.T) {
	const owner = "sub-ws-owner"
	leaks := []string{r3TopologyHostPath, r3TopologyImage, r3TopologySecret}

	type arm struct {
		name    string
		session *http.Cookie
		// forbidden strings that must NOT appear in the body
		forbidden []string
		// required strings that MUST still appear
		required []string
	}
	for _, a := range []arm{
		{
			name:      "a plain member reading an operator-owned workspace",
			session:   ssoSession(t, "sub-plain-member", "m@corp.example", oidc.RoleMember),
			forbidden: append(append([]string{}, leaks...), r3TopologyEgress),
			// The row is still USEFUL: a member picks a workspace by name.
			required: []string{"payments"},
		},
		{
			name:    "a security admin reading a foreign member-owned workspace",
			session: ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin),
			// The HOST axis goes; the EGRESS axis stays, because rewriting this
			// workspace's approved/denied egress is what this tier is widened
			// FOR and it cannot decide blind.
			forbidden: leaks,
			required:  []string{"payments", r3TopologyEgress},
		},
	} {
		t.Run(a.name, func(t *testing.T) {
			// The member arm reads an OPERATOR-owned row (OwnedBy ""), which is
			// the shape a member can legitimately reach; the security arm reads
			// a MEMBER-owned row, which is the shape F246 names.
			ownedBy := ""
			if strings.Contains(a.name, "security admin") {
				ownedBy = owner
			}
			srv, st := newTopologyWorkspaceServer(t, ownedBy)
			for _, path := range []string{"/api/v1/workspaces", "/api/v1/workspaces/" + st.ws.ID.String()} {
				w := doSSO(t, srv, http.MethodGet, path, a.session, "")
				if w.Code != http.StatusOK {
					t.Fatalf("GET %s = %d, want 200; body=%s", path, w.Code, w.Body.String())
				}
				body := w.Body.String()
				for _, bad := range a.forbidden {
					if strings.Contains(body, bad) {
						t.Errorf("GET %s leaked %q — this is the disclosure class R1 declared member-forbidden "+
							"and narrowed /sources, /base-images and /site-config for.\nbody=%s", path, bad, body)
					}
				}
				for _, want := range a.required {
					if !strings.Contains(body, want) {
						t.Errorf("GET %s dropped %q, which this tier needs; over-redaction breaks the product "+
							"the projection exists to preserve.\nbody=%s", path, want, body)
					}
				}
			}
		})
	}

	// THE OWNER AND THE SUPER ADMIN STILL SEE EVERYTHING. Without this arm the
	// test above is satisfied by simply deleting the fields.
	t.Run("the owner and a super admin see the whole document", func(t *testing.T) {
		for _, c := range []struct {
			name    string
			session *http.Cookie
		}{
			{"the workspace's owner", ssoSession(t, owner, "owner@corp.example", oidc.RoleMember)},
			{"a super admin", ssoSession(t, "sub-super", "super@corp.example", oidc.RoleAdmin)},
		} {
			t.Run(c.name, func(t *testing.T) {
				srv, st := newTopologyWorkspaceServer(t, owner)
				w := doSSO(t, srv, http.MethodGet, "/api/v1/workspaces/"+st.ws.ID.String(), c.session, "")
				if w.Code != http.StatusOK {
					t.Fatalf("= %d, want 200; body=%s", w.Code, w.Body.String())
				}
				for _, want := range append(append([]string{}, leaks...), r3TopologyEgress) {
					if !strings.Contains(w.Body.String(), want) {
						t.Errorf("%s no longer sees %q — the owner authored these paths and the super admin owns "+
							"the topology; redaction must not reach either.\nbody=%s", c.name, want, w.Body.String())
					}
				}
			})
		}
	})

	// THE PLACEMENT, pinned structurally: redaction happens at the RESPONSE, not
	// inside getWorkspaceReadable. workspace_build.go and workspace_envcode.go
	// take the same struct from that getter and resolve images and mount paths
	// with it, so a future "just redact in the getter" would fix a read by
	// breaking a build. This arm fails if that ever happens.
	t.Run("the getter still returns the real document", func(t *testing.T) {
		srv, st := newTopologyWorkspaceServer(t, "")
		r, err := http.NewRequest(http.MethodGet, "/api/v1/workspaces/"+st.ws.ID.String(), nil)
		if err != nil {
			t.Fatal(err)
		}
		r = r.WithContext(operatorCtx("sub-plain-member", "m@corp.example", oidc.RoleMember))
		ws, ok := srv.getWorkspaceReadable(httptest.NewRecorder(), r, st.ws.ID)
		if !ok {
			t.Fatal("getWorkspaceReadable refused an operator-owned workspace for a member")
		}
		if len(ws.Sources) == 0 || ws.Sources[0].Path != r3TopologyHostPath {
			t.Errorf("getWorkspaceReadable returned a redacted document (sources=%v); the build and env-as-code "+
				"paths consume this struct and need the real host path — redaction belongs at the response",
				ws.Sources)
		}
		if ws.BaseImage == nil || ws.BaseImage.Image != r3TopologyImage {
			t.Errorf("getWorkspaceReadable blanked base_image.image; resolveWorkspaceImage needs it")
		}
	})
}
