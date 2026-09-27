// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mayDecideStore lets one case in the matrix below control the egress_host
// capability answer — every other case rides authzStore's own honest-empty
// (no grants, unenforced -> allow) state, exactly like the rest of this
// package's fixtures.
type mayDecideStore struct {
	*authzStore
	grants []types.CapabilityGrant
	enf    map[string]bool
}

func (s *mayDecideStore) ListCapabilityGrantsFor(_ context.Context, users, groups []string, _ string) ([]types.CapabilityGrant, error) {
	var out []types.CapabilityGrant
	for _, g := range s.grants {
		switch g.SubjectType {
		case types.CapabilitySubjectAll:
			out = append(out, g)
		case types.CapabilitySubjectUser:
			for _, u := range users {
				if u == g.Subject {
					out = append(out, g)
				}
			}
		case types.CapabilitySubjectGroup:
			for _, gr := range groups {
				if gr == g.Subject {
					out = append(out, g)
				}
			}
		}
	}
	return out, nil
}

func (s *mayDecideStore) GetCapabilityEnforcement(context.Context) (map[string]bool, error) {
	return s.enf, nil
}

var _ store.Store = (*mayDecideStore)(nil)

// mdFixture is one matrix row's harness: a server plus the run/approval ids it
// seeded, and enough of the raw pieces (session cookie or bearer, whether the
// call is local-mode) to fire the SAME identity at both mayDecide (called
// directly, with a hand-built context — the same test-helper shape
// capAllowed's own matrix uses) and the real POST (fired through the router).
type mdFixture struct {
	srv *Server
	ap  types.ApprovalRequest
	run types.AgentRun
}

// newMDFixture builds a server (authzStore + authzApprovals) with one run
// (created by createdBy) and one PENDING approval of kind on it, plus grants/
// enf for the ONE case that needs a controlled capability answer.
func newMDFixture(t *testing.T, kind types.ApprovalKind, scope json.RawMessage, grantID *uuid.UUID, createdBy string, grants []types.CapabilityGrant, enf map[string]bool, configure ...func(*Config)) *mdFixture {
	t.Helper()
	ast := newAuthzStore()
	var st store.Store = ast
	if grants != nil || enf != nil {
		st = &mayDecideStore{authzStore: ast, grants: grants, enf: enf}
	}
	aap := newAuthzApprovals(ast)
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Approvals = aap
	for _, c := range configure {
		c(&cfg)
	}
	srv := New(cfg)

	runID := uuid.New()
	ast.mu.Lock()
	ast.runs[runID] = types.AgentRun{ID: runID, CreatedBy: createdBy, State: types.RunRunning}
	ast.mu.Unlock()

	apID := uuid.New()
	ap := types.ApprovalRequest{
		ID: apID, RunID: runID, Kind: kind, GrantID: grantID,
		RequestedScope: scope, State: types.ApprovalPending, RequestedAt: time.Now().UTC(),
	}
	aap.mu.Lock()
	aap.byID[apID] = ap
	aap.mu.Unlock()

	return &mdFixture{srv: srv, ap: ap, run: ast.runs[runID]}
}

func (f *mdFixture) path(verb string) string { return "/api/v1/approvals/" + f.ap.ID.String() + "/" + verb }

// mdCase is one row of TestMayDecideAgreesWithDecide's matrix.
type mdCase struct {
	name string
	// build returns the fixture, the context mayDecide's r should carry, and a
	// closure that fires the real POST with the equivalent identity.
	build func(t *testing.T) (*mdFixture, context.Context, func(verb string) *httptest.ResponseRecorder)
	verbs []string // which verbs this row pins — both by default
}

// TestMayDecideAgreesWithDecide is #1197 L1b's F2 pin: mayDecide must answer
// exactly what a real POST /approvals/{id}/{approve,deny} would (200 vs any
// non-2xx), over role x kind x ownership x four-eyes x local-mode x capability
// grant. A hand-matched mirror of authorizeUserDecision/requireSecondHuman
// alone would miss any gate decide() has that those two helpers do not name
// (F2's own finding) — driving the SAME identity through the real handler is
// what catches that class of drift.
func TestMayDecideAgreesWithDecide(t *testing.T) {
	adminSub := "sub-admin-md"
	memberSub := "sub-member-md"
	grantID := uuid.New()
	adoScope := json.RawMessage(`{"lane":"azure_devops","grant_id":"` + grantID.String() + `","capability":"pr_create"}`)
	egressScope := func(host string) json.RawMessage { return json.RawMessage(`{"host":"` + host + `"}`) }

	cases := []mdCase{
		{
			name:  "security operator (admin) decides a plain tool_call on a foreign run",
			verbs: []string{"deny"}, // approve would need a live grant to actually resolve; the VERDICT (may it be decided) is the same either way
			build: func(t *testing.T) (*mdFixture, context.Context, func(string) *httptest.ResponseRecorder) {
				f := newMDFixture(t, types.ApprovalToolCall, json.RawMessage(`{}`), nil, "sub-someone-else", nil, nil)
				sess := ssoSession(t, adminSub, "admin@corp.example", oidc.RoleAdmin)
				ctx := withOIDCRole(withOIDCEmail(withOIDCHuman(context.Background(), adminSub), "admin@corp.example"), oidc.RoleAdmin)
				return f, ctx, func(verb string) *httptest.ResponseRecorder {
					return doSSO(t, f.srv, http.MethodPost, f.path(verb), sess, `{"reason":"t"}`)
				}
			},
		},
		{
			name: "credential_reauth is decidable by NOBODY, security operator included",
			build: func(t *testing.T) (*mdFixture, context.Context, func(string) *httptest.ResponseRecorder) {
				f := newMDFixture(t, types.ApprovalCredentialReauth, json.RawMessage(`{}`), nil, adminSub, nil, nil)
				sess := ssoSession(t, adminSub, "admin@corp.example", oidc.RoleAdmin)
				ctx := withOIDCRole(withOIDCEmail(withOIDCHuman(context.Background(), adminSub), "admin@corp.example"), oidc.RoleAdmin)
				return f, ctx, func(verb string) *httptest.ResponseRecorder {
					return doSSO(t, f.srv, http.MethodPost, f.path(verb), sess, `{"reason":"t"}`)
				}
			},
		},
		{
			name: "a member decides egress_domain on their OWN run — no grant, unenforced: allowed",
			build: func(t *testing.T) (*mdFixture, context.Context, func(string) *httptest.ResponseRecorder) {
				f := newMDFixture(t, types.ApprovalEgressDomain, egressScope("registry.npmjs.org"), nil, memberSub, nil, nil)
				sess := ssoSession(t, memberSub, "member@corp.example", oidc.RoleUser)
				ctx := withOIDCRole(withOIDCEmail(withOIDCHuman(context.Background(), memberSub), "member@corp.example"), oidc.RoleUser)
				return f, ctx, func(verb string) *httptest.ResponseRecorder {
					return doSSO(t, f.srv, http.MethodPost, f.path(verb), sess, `{"reason":"t"}`)
				}
			},
		},
		{
			name: "a member decides egress_domain on their OWN run — the host is DENIED by grant: refused",
			build: func(t *testing.T) (*mdFixture, context.Context, func(string) *httptest.ResponseRecorder) {
				deny := types.CapabilityGrant{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: memberSub, Capability: capEgressHost, Value: "registry.npmjs.org", Effect: types.CapabilityDeny}
				f := newMDFixture(t, types.ApprovalEgressDomain, egressScope("registry.npmjs.org"), nil, memberSub,
					[]types.CapabilityGrant{deny}, map[string]bool{capEgressHost: true})
				sess := ssoSession(t, memberSub, "member@corp.example", oidc.RoleUser)
				ctx := withOIDCRole(withOIDCEmail(withOIDCHuman(context.Background(), memberSub), "member@corp.example"), oidc.RoleUser)
				return f, ctx, func(verb string) *httptest.ResponseRecorder {
					return doSSO(t, f.srv, http.MethodPost, f.path(verb), sess, `{"reason":"t"}`)
				}
			},
		},
		{
			name: "a member decides egress_domain on a FOREIGN run: refused (404, no existence oracle)",
			build: func(t *testing.T) (*mdFixture, context.Context, func(string) *httptest.ResponseRecorder) {
				f := newMDFixture(t, types.ApprovalEgressDomain, egressScope("registry.npmjs.org"), nil, "sub-someone-else", nil, nil)
				sess := ssoSession(t, memberSub, "member@corp.example", oidc.RoleUser)
				ctx := withOIDCRole(withOIDCEmail(withOIDCHuman(context.Background(), memberSub), "member@corp.example"), oidc.RoleUser)
				return f, ctx, func(verb string) *httptest.ResponseRecorder {
					return doSSO(t, f.srv, http.MethodPost, f.path(verb), sess, `{"reason":"t"}`)
				}
			},
		},
		{
			name: "a member decides a plain tool_call on their own run: refused (admin-only, no ownership carve-out)",
			build: func(t *testing.T) (*mdFixture, context.Context, func(string) *httptest.ResponseRecorder) {
				f := newMDFixture(t, types.ApprovalToolCall, json.RawMessage(`{}`), nil, memberSub, nil, nil)
				sess := ssoSession(t, memberSub, "member@corp.example", oidc.RoleUser)
				ctx := withOIDCRole(withOIDCEmail(withOIDCHuman(context.Background(), memberSub), "member@corp.example"), oidc.RoleUser)
				return f, ctx, func(verb string) *httptest.ResponseRecorder {
					return doSSO(t, f.srv, http.MethodPost, f.path(verb), sess, `{"reason":"t"}`)
				}
			},
		},
		{
			name:  "a member DENIES their own Azure DevOps escalation: allowed (ownership is the whole member rule)",
			verbs: []string{"deny"}, // approve would also need adoEscalationWithinCeiling's live config
			build: func(t *testing.T) (*mdFixture, context.Context, func(string) *httptest.ResponseRecorder) {
				f := newMDFixture(t, types.ApprovalToolCall, adoScope, &grantID, memberSub, nil, nil)
				sess := ssoSession(t, memberSub, "member@corp.example", oidc.RoleUser)
				ctx := withOIDCRole(withOIDCEmail(withOIDCHuman(context.Background(), memberSub), "member@corp.example"), oidc.RoleUser)
				return f, ctx, func(verb string) *httptest.ResponseRecorder {
					return doSSO(t, f.srv, http.MethodPost, f.path(verb), sess, `{"reason":"t"}`)
				}
			},
		},
		{
			name:  "a member denies a FOREIGN Azure DevOps escalation: refused (404)",
			verbs: []string{"deny"},
			build: func(t *testing.T) (*mdFixture, context.Context, func(string) *httptest.ResponseRecorder) {
				f := newMDFixture(t, types.ApprovalToolCall, adoScope, &grantID, "sub-someone-else", nil, nil)
				sess := ssoSession(t, memberSub, "member@corp.example", oidc.RoleUser)
				ctx := withOIDCRole(withOIDCEmail(withOIDCHuman(context.Background(), memberSub), "member@corp.example"), oidc.RoleUser)
				return f, ctx, func(verb string) *httptest.ResponseRecorder {
					return doSSO(t, f.srv, http.MethodPost, f.path(verb), sess, `{"reason":"t"}`)
				}
			},
		},
		{
			name: "security operator (a session-verified admin, not the token) decides their OWN egress under four-eyes: refused (403)",
			build: func(t *testing.T) (*mdFixture, context.Context, func(string) *httptest.ResponseRecorder) {
				t.Setenv(envEgressSecondHuman, "1")
				f := newMDFixture(t, types.ApprovalEgressDomain, egressScope("registry.npmjs.org"), nil, adminSub, nil, nil)
				sess := ssoSession(t, adminSub, "admin@corp.example", oidc.RoleAdmin)
				ctx := withOIDCRole(withOIDCEmail(withOIDCHuman(context.Background(), adminSub), "admin@corp.example"), oidc.RoleAdmin)
				return f, ctx, func(verb string) *httptest.ResponseRecorder {
					return doSSO(t, f.srv, http.MethodPost, f.path(verb), sess, `{"reason":"t"}`)
				}
			},
		},
		{
			name: "the admin-bearer-token caller decides egress under four-eyes, even on its own run: allowed (documented break-glass)",
			build: func(t *testing.T) (*mdFixture, context.Context, func(string) *httptest.ResponseRecorder) {
				t.Setenv(envEgressSecondHuman, "1")
				f := newMDFixture(t, types.ApprovalEgressDomain, egressScope("registry.npmjs.org"), nil, adminTokenPrincipal, nil, nil)
				ctx := context.Background() // no verified human at all -> actorFromRequest's admin-token fallback
				return f, ctx, func(verb string) *httptest.ResponseRecorder {
					return do(t, f.srv, http.MethodPost, f.path(verb), adminToken, `{"reason":"t"}`)
				}
			},
		},
		{
			name: "local mode refuses the four-eyes switch outright: 503, unconditionally",
			build: func(t *testing.T) (*mdFixture, context.Context, func(string) *httptest.ResponseRecorder) {
				t.Setenv(envEgressSecondHuman, "1")
				f := newMDFixture(t, types.ApprovalEgressDomain, egressScope("registry.npmjs.org"), nil, "local:alice", nil, nil,
					func(c *Config) { c.LocalMode = true; c.LocalOperator = "local:alice" })
				ctx := withLocalPrincipal(context.Background(), "local:alice")
				return f, ctx, func(verb string) *httptest.ResponseRecorder {
					return do(t, f.srv, http.MethodPost, f.path(verb), "", `{"reason":"t"}`)
				}
			},
		},
		{
			name: "local mode, four-eyes OFF: the local operator decides their own egress: allowed",
			build: func(t *testing.T) (*mdFixture, context.Context, func(string) *httptest.ResponseRecorder) {
				f := newMDFixture(t, types.ApprovalEgressDomain, egressScope("registry.npmjs.org"), nil, "local:alice", nil, nil,
					func(c *Config) { c.LocalMode = true; c.LocalOperator = "local:alice" })
				ctx := withLocalPrincipal(context.Background(), "local:alice")
				return f, ctx, func(verb string) *httptest.ResponseRecorder {
					return do(t, f.srv, http.MethodPost, f.path(verb), "", `{"reason":"t"}`)
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			verbs := c.verbs
			if verbs == nil {
				verbs = []string{"approve", "deny"}
			}
			for _, verb := range verbs {
				t.Run(verb, func(t *testing.T) {
					f, ctx, fire := c.build(t)
					req := httptest.NewRequest(http.MethodPost, f.path(verb), nil).WithContext(ctx)
					got := f.srv.mayDecide(req, f.ap, f.run)
					w := fire(verb)
					want := w.Code == http.StatusOK
					if got != want {
						t.Fatalf("mayDecide = %v, but POST %s answered %d (want-200=%v); body=%s",
							got, verb, w.Code, want, w.Body.String())
					}
				})
			}
		})
	}
}

// TestGetApprovalsProjectsHold pins the ONE projection point (F5/F9's spec):
// a PENDING row on GET /approvals carries held/held_until; a decided row
// carries neither, regardless of what its hold once was.
func TestGetApprovalsProjectsHold(t *testing.T) {
	ast := newAuthzStore()
	aap := newAuthzApprovals(ast)
	h := newHarness(t)
	cfg := baseTestConfig(h, ast)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Approvals = aap
	srv := New(cfg)
	admin := ssoSession(t, "sub-admin-proj", "admin@corp.example", oidc.RoleAdmin)

	runID := uuid.New()
	ast.mu.Lock()
	ast.runs[runID] = types.AgentRun{ID: runID, CreatedBy: "sub-someone", State: types.RunRunning}
	ast.mu.Unlock()

	heldID := uuid.New()
	decidedID := uuid.New()
	aap.mu.Lock()
	aap.byID[heldID] = types.ApprovalRequest{
		ID: heldID, RunID: runID, Kind: types.ApprovalEgressDomain,
		RequestedScope: json.RawMessage(`{"host":"h","mode":"wait_for_review"}`),
		State:          types.ApprovalPending, RequestedAt: time.Now().UTC(),
	}
	aap.byID[decidedID] = types.ApprovalRequest{
		ID: decidedID, RunID: runID, Kind: types.ApprovalEgressDomain,
		RequestedScope: json.RawMessage(`{"host":"h2","mode":"wait_for_review"}`),
		State:          types.ApprovalApproved, RequestedAt: time.Now().UTC(), DecidedBy: "sub-admin-proj",
	}
	aap.mu.Unlock()

	w := doSSO(t, srv, http.MethodGet, "/api/v1/approvals?state=&run_id="+runID.String(), admin, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /approvals: status = %d, body=%s", w.Code, w.Body.String())
	}
	var rows []types.ApprovalRequest
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	var gotHeld, gotDecided *types.ApprovalRequest
	for i := range rows {
		switch rows[i].ID {
		case heldID:
			gotHeld = &rows[i]
		case decidedID:
			gotDecided = &rows[i]
		}
	}
	if gotHeld == nil || !gotHeld.Held || gotHeld.HeldUntil == nil {
		t.Fatalf("PENDING row = %+v, want held=true with a held_until", gotHeld)
	}
	if gotDecided == nil || gotDecided.Held || gotDecided.HeldUntil != nil {
		t.Fatalf("APPROVED row = %+v, want held=false, held_until=nil", gotDecided)
	}
}
