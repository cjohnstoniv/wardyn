// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The run doors and the audit marker under WARDYN_GOVERN_ADMIN_RUNS. With the
// switch on, a governed admin's OWN run captured a profile, so the super-admin
// exemption from deny_interactive and deny_ui_apps holds only for an
// operator-owned (break-glass) run.

const governDoorAdmin = "alice"

// profileTokenStore is profileListStore with one admin-role personal token.
type profileTokenStore struct {
	*profileListStore
	raw string
	row types.APIToken
}

func (s *profileTokenStore) GetAPITokenByRaw(_ context.Context, raw string) (types.APIToken, error) {
	if raw != s.raw {
		return types.APIToken{}, store.ErrNotFound
	}
	return s.row, nil
}

func (s *profileTokenStore) TouchAPIToken(context.Context, uuid.UUID, time.Time) error { return nil }

func governAdminTokenRow() (string, types.APIToken) {
	return apiTokenPrefix + "door", types.APIToken{
		ID: uuid.New(), Principal: governDoorAdmin, Email: governDoorAdmin + "@corp.example", Role: oidc.RoleAdmin,
		UserType: types.UserTypeStandard, Groups: []string{"eng"}, GroupsTruncated: new(bool), Name: "ci",
	}
}

func TestGovernAdminRuns_TerminalAttachDoors(t *testing.T) {
	profile := types.GovernanceProfile{ID: uuid.New(), Name: "ci", Limits: types.GovernanceLimits{DenyInteractive: true}}
	run := func(createdBy string, operatorOwned bool) types.AgentRun {
		return types.AgentRun{ID: uuid.New(), CreatedBy: createdBy, OperatorOwned: operatorOwned, State: types.RunRunning,
			SandboxRef: "sbx-1", Task: "make test", GovernanceProfileID: &profile.ID}
	}
	raw, row := governAdminTokenRow()
	setup := func(t *testing.T, on bool, r types.AgentRun) (*Server, *profileTokenStore) {
		h := newHarness(t)
		st := &profileTokenStore{profileListStore: &profileListStore{authzStore: newAuthzStore(), profiles: []types.GovernanceProfile{profile}}, raw: raw, row: row}
		st.authzStore.runs[r.ID] = r
		cfg := baseTestConfig(h, st)
		cfg.Runner = &fakeRunner{}
		cfg.OIDC = &oidc.Authenticator{}
		cfg.GovernAdminRuns = on
		return New(cfg), st
	}
	refused := func(w *httptest.ResponseRecorder) bool {
		var body errorBody
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code == http.StatusForbidden && body.Reason == string(authz.ReasonGovernanceProfile)
	}
	// Past the profile door a plain GET (no WebSocket handshake) is answered 426.
	const attached = http.StatusUpgradeRequired
	path := func(r types.AgentRun) string { return "/api/v1/runs/" + r.ID.String() + "/attach" }
	ticketAttach := func(t *testing.T, srv *Server, st *profileTokenStore, r types.AgentRun, principal string) *httptest.ResponseRecorder {
		tok, err := mintAttachTicket(context.Background(), st, r.ID, types.ActorHuman, principal, oidc.RoleAdmin, time.Now())
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		return do(t, srv, http.MethodGet, path(r)+"?ticket="+tok, "", "")
	}
	session := ssoSession(t, governDoorAdmin, governDoorAdmin+"@corp.example", oidc.RoleAdmin)

	t.Run("ticket lane", func(t *testing.T) {
		own := run(governDoorAdmin, false)
		srv, st := setup(t, true, own)
		if w := ticketAttach(t, srv, st, own, governDoorAdmin); !refused(w) {
			t.Errorf("a governed admin's own run, switch on: %d %s, want 403 governance_profile", w.Code, w.Body.String())
		}
		breakGlass := run("root", true)
		srv, st = setup(t, true, breakGlass)
		if w := ticketAttach(t, srv, st, breakGlass, "root"); w.Code != attached {
			t.Errorf("an operator-owned run, switch on: %d %s, want %d", w.Code, w.Body.String(), attached)
		}
		srv, st = setup(t, false, own)
		if w := ticketAttach(t, srv, st, own, governDoorAdmin); w.Code != attached {
			t.Errorf("a super admin's own run, switch off: %d %s, want %d (today's exemption)", w.Code, w.Body.String(), attached)
		}
	})

	t.Run("no-ticket lane", func(t *testing.T) {
		own := run(governDoorAdmin, false)
		for _, tc := range []struct {
			name string
			on   bool
			run  types.AgentRun
			do   func(t *testing.T, srv *Server, r types.AgentRun) *httptest.ResponseRecorder
			want func(w *httptest.ResponseRecorder) bool
		}{
			{"SSO cookie, own run, switch on", true, own, cookieAttach(path, session), refused},
			{"admin-role bearer, own run, switch on", true, own, bearerAttach(path, raw), refused},
			{"SSO cookie, own run, switch off", false, own, cookieAttach(path, session), attachedOK},
			{"admin-role bearer, own run, switch off", false, own, bearerAttach(path, raw), attachedOK},
			{"SSO cookie, operator-owned run, switch on", true, run("root", true), cookieAttach(path, session), attachedOK},
			// The admin token and local mode stay break-glass.
			{"admin token, operator-owned run, switch on", true, run(adminTokenPrincipal, true), bearerAttach(path, adminToken), attachedOK},
		} {
			t.Run(tc.name, func(t *testing.T) {
				srv, _ := setup(t, tc.on, tc.run)
				if w := tc.do(t, srv, tc.run); !tc.want(w) {
					t.Errorf("attach = %d %s", w.Code, w.Body.String())
				}
			})
		}
	})
}

func attachedOK(w *httptest.ResponseRecorder) bool { return w.Code == http.StatusUpgradeRequired }

func cookieAttach(path func(types.AgentRun) string, c *http.Cookie) func(*testing.T, *Server, types.AgentRun) *httptest.ResponseRecorder {
	return func(t *testing.T, srv *Server, r types.AgentRun) *httptest.ResponseRecorder {
		return doSSO(t, srv, http.MethodGet, path(r), c, "")
	}
}

func bearerAttach(path func(types.AgentRun) string, bearer string) func(*testing.T, *Server, types.AgentRun) *httptest.ResponseRecorder {
	return func(t *testing.T, srv *Server, r types.AgentRun) *httptest.ResponseRecorder {
		return do(t, srv, http.MethodGet, path(r), bearer, "")
	}
}

func TestGovernAdminRuns_SSHDoor(t *testing.T) {
	profile := types.GovernanceProfile{ID: uuid.New(), Name: "ci", Limits: types.GovernanceLimits{DenyInteractive: true}}
	now := time.Now()
	priv, pub := mustSSHKeypair(t)
	build := func(on bool) (*sshTestHarness, uuid.UUID, uuid.UUID) {
		st := newSSHMemStore()
		st.profiles = []types.GovernanceProfile{profile}
		own, breakGlass := uuid.New(), uuid.New()
		st.putRun(types.AgentRun{ID: own, CreatedBy: "root@example.com", State: types.RunRunning, SandboxRef: "sbx-1", GovernanceProfileID: &profile.ID})
		st.putRun(types.AgentRun{ID: breakGlass, CreatedBy: "alice@example.com", OperatorOwned: true, State: types.RunRunning, SandboxRef: "sbx-2", GovernanceProfileID: &profile.ID})
		st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: "root@example.com",
			PublicKey: string(ssh.MarshalAuthorizedKey(pub)), Role: oidc.RoleAdmin, RoleCheckedAt: &now})
		return newSSHTestHarness(t, st, &sshFakeRunner{}, func(c *Config) { c.GovernAdminRuns = on }), own, breakGlass
	}

	t.Run("switch on: a governed admin's own run is refused, with the profile named", func(t *testing.T) {
		h, own, _ := build(true)
		if _, err := sshDial(t, h, own.String(), priv); err == nil {
			t.Fatal("a governed admin's ssh into their own deny_interactive run succeeded, want refused")
		}
		ev := waitForAudit(t, h.audit, own, "ssh.authenticate", "failure")
		if ev == nil || !strings.Contains(string(ev.Data), `governance profile \"ci\" denies interactive sessions`) {
			t.Fatalf("audit = %v, want the profile named; events=%s", ev, auditDump(h.audit.snapshot(), own))
		}
	})
	t.Run("switch on: an operator-owned run is still reachable", func(t *testing.T) {
		h, _, breakGlass := build(true)
		c, err := sshDial(t, h, breakGlass.String(), priv)
		if err != nil {
			t.Fatalf("admin ssh into an operator-owned run refused: %v", err)
		}
		_ = c.Close()
	})
	t.Run("switch off: a super admin's own run is exempt, as today", func(t *testing.T) {
		h, own, _ := build(false)
		c, err := sshDial(t, h, own.String(), priv)
		if err != nil {
			t.Fatalf("super admin ssh into their own run refused with the switch off: %v", err)
		}
		_ = c.Close()
	})
}

func TestGovernAdminRuns_UIGatewayDoor(t *testing.T) {
	profile := types.GovernanceProfile{ID: uuid.New(), Name: "no-ui", Limits: types.GovernanceLimits{DenyUIApps: true}}
	enter := func(t *testing.T, on, operatorOwned bool) *httptest.ResponseRecorder {
		h := newUIHarness(t, okBackend())
		h.srv.cfg.GovernAdminRuns = on
		h.store.profiles = []types.GovernanceProfile{profile}
		h.run.GovernanceProfileID = &profile.ID
		h.run.OperatorOwned = operatorOwned
		h.store.putRun(h.run)
		return h.enter(url.Values{"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {h.ticket(h.run.ID, h.owner, oidc.RoleAdmin)}})
	}
	if w := enter(t, true, false); w.Code != http.StatusForbidden {
		t.Errorf("a governed admin's own run, switch on: %d, want 403", w.Code)
	}
	if w := enter(t, true, true); w.Code != http.StatusFound {
		t.Errorf("an operator-owned run, switch on: %d, want 302", w.Code)
	}
	if w := enter(t, false, false); w.Code != http.StatusFound {
		t.Errorf("a super admin's own run, switch off: %d, want 302 (today's exemption)", w.Code)
	}
}

// governCreateServer is govEscapeFixture's create-and-dispatch server with the
// switch and the deployment shape under the test's hand.
func governCreateServer(t *testing.T, on bool, shape func(*Config)) (*Server, *govEscapeStore, *recRecorder) {
	t.Helper()
	h := newHarness(t)
	st := newGovEscapeStore(&capStore{})
	audit := &recRecorder{}
	cfg := baseTestConfig(h, st)
	cfg.Audit = audit
	cfg.Broker = h.broker
	cfg.Runner = &fakeRunner{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{govCorpSecret: []byte("v")}}
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = govDeployment()
	cfg.GovernAdminRuns = on
	if shape != nil {
		shape(&cfg)
	}
	return New(cfg), st, audit
}

// governCreateRow launches a run and returns the decoded run.create success row.
func governCreateRow(t *testing.T, srv *Server, st *govEscapeStore, audit *recRecorder, send func() *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	w := send()
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
	}
	st.mu.Lock()
	var runID uuid.UUID
	for id := range st.runs {
		runID = id
	}
	st.mu.Unlock()
	ev := waitForRecAudit(t, audit, runID, "run.create", "success")
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("run.create data: %v", err)
	}
	return data
}

func TestGovernAdminRuns_CreateAuditMarker(t *testing.T) {
	const body = `{"agent":"claude-code","task":"t"}`
	adminTokenLaunch := func(srv *Server) func() *httptest.ResponseRecorder {
		return func() *httptest.ResponseRecorder {
			return do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
		}
	}
	raw, row := governAdminTokenRow()
	personalTokenLaunch := func(srv *Server, st *govEscapeStore) func() *httptest.ResponseRecorder {
		st.token, st.tokenRaw = &row, raw
		return func() *httptest.ResponseRecorder { return do(t, srv, http.MethodPost, "/api/v1/runs", raw, body) }
	}
	localLaunch := func(srv *Server) func() *httptest.ResponseRecorder {
		return func() *httptest.ResponseRecorder {
			r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			r.Host, r.RemoteAddr = "127.0.0.1", "127.0.0.1:54321"
			w := httptest.NewRecorder()
			panicFails(t, srv.Handler()).ServeHTTP(w, r)
			return w
		}
	}
	local := func(c *Config) { c.OIDC, c.LocalMode, c.LocalOperator = nil, true, "local:tester" }

	t.Run("the admin token is marked with the switch on", func(t *testing.T) {
		srv, st, audit := governCreateServer(t, true, nil)
		if got := governCreateRow(t, srv, st, audit, adminTokenLaunch(srv))["governance_exempt"]; got != true {
			t.Errorf("governance_exempt = %v, want true", got)
		}
	})
	t.Run("local mode is marked with the switch on", func(t *testing.T) {
		srv, st, audit := governCreateServer(t, true, local)
		if got := governCreateRow(t, srv, st, audit, localLaunch(srv))["governance_exempt"]; got != true {
			t.Errorf("governance_exempt = %v, want true", got)
		}
	})
	t.Run("an admin-role personal token is governed, so unmarked", func(t *testing.T) {
		srv, st, audit := governCreateServer(t, true, nil)
		data := governCreateRow(t, srv, st, audit, personalTokenLaunch(srv, st))
		if _, present := data["governance_exempt"]; present {
			t.Errorf("run.create carries governance_exempt for a governed launch: %v", data)
		}
	})
	t.Run("no row carries the key with the switch off", func(t *testing.T) {
		for name, mk := range map[string]func() (*Server, *govEscapeStore, *recRecorder, func() *httptest.ResponseRecorder){
			"admin token": func() (*Server, *govEscapeStore, *recRecorder, func() *httptest.ResponseRecorder) {
				srv, st, audit := governCreateServer(t, false, nil)
				return srv, st, audit, adminTokenLaunch(srv)
			},
			"local mode": func() (*Server, *govEscapeStore, *recRecorder, func() *httptest.ResponseRecorder) {
				srv, st, audit := governCreateServer(t, false, local)
				return srv, st, audit, localLaunch(srv)
			},
			"personal token": func() (*Server, *govEscapeStore, *recRecorder, func() *httptest.ResponseRecorder) {
				srv, st, audit := governCreateServer(t, false, nil)
				return srv, st, audit, personalTokenLaunch(srv, st)
			},
		} {
			srv, st, audit, send := mk()
			if _, present := governCreateRow(t, srv, st, audit, send)["governance_exempt"]; present {
				t.Errorf("%s: run.create carries governance_exempt with the switch off", name)
			}
		}
	})
}

// authzTokenStore is the matrix store with one admin-role personal token, so a
// `wdn_` bearer authenticates as its owner (the matrix otherwise presents only
// cookies and the admin token).
type authzTokenStore struct {
	*authzStore
	raw string
	row types.APIToken
}

func (s authzTokenStore) GetAPITokenByRaw(_ context.Context, raw string) (types.APIToken, error) {
	if raw != s.raw {
		return types.APIToken{}, store.ErrNotFound
	}
	return s.row, nil
}

// TestAuthzMatrixGovernAdminRuns: the switch changes who stands outside
// governance at run time and nothing about route admission. Every gated route
// answers an SSO admin session and an admin-role personal token the same with
// the switch on as with it off. This proves admission only: the per-site table
// in TestGovernAdminRuns_EverySwapSite is the governance proof.
func TestAuthzMatrixGovernAdminRuns(t *testing.T) {
	raw, row := governAdminTokenRow()
	session := ssoSession(t, row.Principal, row.Email, oidc.RoleAdmin)
	keys := make([]string, 0, len(routeMatrix))
	for key, rc := range routeMatrix {
		switch rc.class {
		case classAdmin, classSecurity, classMember:
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)

	// One fresh server per (switch, credential), each probed in the same sorted
	// order, so a route that mutates state sees the same history everywhere.
	probe := func(on bool, viaToken bool) map[string]int {
		srv, _, _, _ := newAuthzMatrixServer(t, func(c *Config) {
			c.GovernAdminRuns = on
			c.Store = authzTokenStore{authzStore: c.Store.(*authzStore), raw: raw, row: row}
		})
		codes := map[string]int{}
		for _, key := range keys {
			method, pattern, _ := strings.Cut(key, " ")
			rc := routeMatrix[key]
			p := buildPath(pattern, "x1")
			if rc.class == classMember {
				p = buildPath(pattern, uuid.NewSHA1(uuid.NameSpaceURL, []byte(key)).String())
			}
			var w *httptest.ResponseRecorder
			if viaToken {
				w = do(t, srv, method, p, raw, bodyFor(method, rc))
			} else {
				w = doSSO(t, srv, method, p, session, bodyFor(method, rc))
			}
			codes[key] = w.Code
		}
		return codes
	}
	for _, viaToken := range []bool{false, true} {
		name := map[bool]string{false: "SSO admin session", true: "admin-role personal token"}[viaToken]
		t.Run(name, func(t *testing.T) {
			off, on := probe(false, viaToken), probe(true, viaToken)
			if len(on) < 50 {
				t.Fatalf("probed %d routes, want the whole admin and member surface", len(on))
			}
			for _, key := range keys {
				// The one route the switch does refuse: Record Mode answers a
				// governed admin recording_governed (record_govern_admin_test.go
				// proves the body and the audit row).
				if key == "POST /api/v1/workspaces/{id}/record" {
					if on[key] != http.StatusForbidden || off[key] == http.StatusForbidden {
						t.Errorf("%s: switch on = %d, switch off = %d, want 403 only with the switch on", key, on[key], off[key])
					}
					continue
				}
				if on[key] != off[key] {
					t.Errorf("%s: switch on = %d, switch off = %d, want identical route admission", key, on[key], off[key])
				}
				// Some routes are session-only (/me/view), so a token's 401 there
				// is today's answer and equality above is the whole claim.
				if !viaToken && on[key] == http.StatusUnauthorized {
					t.Errorf("%s: 401 for a signed-in admin", key)
				}
			}
		})
	}
}
