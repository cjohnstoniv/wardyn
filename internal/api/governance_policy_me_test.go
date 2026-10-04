// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// siteCountingStore counts GetSiteConfig, the read /me must not add.
type siteCountingStore struct {
	*govEscapeStore
	reads atomic.Int32
}

func (s *siteCountingStore) GetSiteConfig(ctx context.Context) (types.SiteConfig, error) {
	s.reads.Add(1)
	return s.govEscapeStore.GetSiteConfig(ctx)
}

// meContact is GET /me's governance_contact, as a raw message so absent, null
// and an object stay distinct.
func meContact(t *testing.T, srv *Server, cookie *http.Cookie, bearer string) (json.RawMessage, bool) {
	t.Helper()
	var w *httptest.ResponseRecorder
	if cookie != nil {
		w = doSSO(t, srv, http.MethodGet, "/api/v1/me", cookie, "")
	} else {
		w = do(t, srv, http.MethodGet, "/api/v1/me", bearer, "")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("GET /me = %d, want 200: %s", w.Code, w.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /me: %v", err)
	}
	raw, ok := body["governance_contact"]
	return raw, ok
}

func TestMeGovernanceContact(t *testing.T) {
	prof := taggedProfile("walled", "alpha", types.GovernanceLimits{DenyInteractive: true})
	policyHelp := policyContactFor("deploy")
	build := func(cs *capStore) (*Server, *siteCountingStore) {
		srv, st, _ := govEscapeFixture(t, cs)
		st.siteConfig = types.SiteConfig{PolicyHelp: policyHelp}
		counting := &siteCountingStore{govEscapeStore: st}
		srv.cfg.Store = counting
		return srv, counting
	}
	member := govSession(t, "sub-walled", []string{"eng"}, false)

	t.Run("a member under a profile gets that profile and its contact", func(t *testing.T) {
		srv, _ := build(assignedStore(prof))
		raw, ok := meContact(t, srv, member, "")
		var got policyref.Ref
		if !ok || json.Unmarshal(raw, &got) != nil || got != profileRefFor("walled", "alpha") {
			t.Errorf("governance_contact = %s (present %v), want %+v", raw, ok, profileRefFor("walled", "alpha"))
		}
	})

	t.Run("a member no profile binds gets the bare deployment arm, without a site-config read", func(t *testing.T) {
		srv, st := build(&capStore{})
		before := st.reads.Load()
		raw, _ := meContact(t, srv, member, "")
		if string(raw) != `{"source":"deployment"}` {
			t.Errorf("governance_contact = %s, want only the deployment source: policy_help is for refusals and run detail", raw)
		}
		// The handler's own reads are today's; the contact adds none.
		perMe := st.reads.Load() - before
		before = st.reads.Load()
		_ = srv.meGovernanceContact(httptest.NewRequest(http.MethodGet, "/api/v1/me", nil).
			WithContext(govMemberCtx([]string{"eng"}, false)))
		if added := st.reads.Load() - before; added != 0 {
			t.Errorf("meGovernanceContact made %d GetSiteConfig calls, want 0 (a whole /me makes %d)", added, perMe)
		}
	})

	t.Run("the profile arm reads no site config either", func(t *testing.T) {
		srv, st := build(assignedStore(prof))
		before := st.reads.Load()
		_ = srv.meGovernanceContact(httptest.NewRequest(http.MethodGet, "/api/v1/me", nil).
			WithContext(govMemberCtx([]string{"eng"}, false)))
		if added := st.reads.Load() - before; added != 0 {
			t.Errorf("meGovernanceContact made %d GetSiteConfig calls, want 0", added)
		}
	})

	t.Run("an operator gets null", func(t *testing.T) {
		srv, _ := build(assignedStore(prof))
		raw, ok := meContact(t, srv, nil, adminToken)
		if !ok || string(raw) != "null" {
			t.Errorf("governance_contact = %s (present %v), want null", raw, ok)
		}
	})

	t.Run("a resolver error is a 200 with null, never a 500", func(t *testing.T) {
		srv, _ := build(&capStore{govErr: errors.New("pg: connection refused")})
		raw, ok := meContact(t, srv, member, "")
		if !ok || string(raw) != "null" {
			t.Errorf("governance_contact = %s (present %v), want null", raw, ok)
		}
	})

	t.Run("a stale group snapshot is a 200 with null", func(t *testing.T) {
		srv, _ := build(&capStore{govProfile: prof, govTier: types.CapabilitySubjectGroup, govHasGroupTier: true})
		raw, ok := meContact(t, srv, govSession(t, "sub-walled", []string{"eng"}, true), "")
		if !ok || string(raw) != "null" {
			t.Errorf("governance_contact = %s (present %v), want null", raw, ok)
		}
	})
}

// profileMatrixStore answers each member's own profile, so two members under two
// profiles can walk the whole router. It sits over authzStore, the store the
// route matrix is built on, whose every other answer is the empty one.
type profileMatrixStore struct {
	*authzStore
	bySub map[string]*types.GovernanceProfile
}

func (s *profileMatrixStore) ResolveGovernanceProfile(_ context.Context, users, _ []string, _ string) (*types.GovernanceProfile, types.CapabilitySubjectType, error) {
	for _, u := range users {
		if p := s.bySub[u]; p != nil {
			return p, types.CapabilitySubjectUser, nil
		}
	}
	return nil, "", store.ErrNotFound
}

func (s *profileMatrixStore) ListGovernanceProfiles(context.Context) ([]types.GovernanceProfile, error) {
	var out []types.GovernanceProfile
	for _, p := range s.bySub {
		out = append(out, *p)
	}
	return out, nil
}

// TestNeitherMemberSeesTheOtherProfile walks every route the router registers
// (chi.Walk) as two members under two profiles, each against their own run and
// the other's, and fails if any answer to one member carries anything that
// belongs to the other member's profile: its name, its owner, its address or its
// request link. The same walk proves each member does get their own on /me, on
// refusals and on their run's detail, so an empty answer cannot pass.
func TestNeitherMemberSeesTheOtherProfile(t *testing.T) {
	const subA, subB = "sub-alpha-member", "sub-beta-member"
	profA := taggedProfile("alpha-profile", "alpha", types.GovernanceLimits{
		DenyTaskModeExec: true, DenyInteractive: true, DenyUIApps: true, MaxConcurrentRuns: 1})
	profB := taggedProfile("beta-profile", "beta", types.GovernanceLimits{
		DenyTaskModeExec: true, DenyInteractive: true, DenyUIApps: true, MaxConcurrentRuns: 1})
	// The markers are the profile-specific strings. "alpha" and "beta" alone
	// would collide with "alphabet"-style words in unrelated bodies; each of
	// these is a value only the profile's own contact or name could produce.
	markers := map[string][]string{
		subA: {"alpha-profile", "alpha Owner", "alpha@alpha.example", "alpha.example/request", "ask the alpha team"},
		subB: {"beta-profile", "beta Owner", "beta@beta.example", "beta.example/request", "ask the beta team"},
	}
	other := map[string]string{subA: subB, subB: subA}

	srv, ast, _, _ := newAuthzMatrixServer(t, func(cfg *Config) {
		cfg.Store = &profileMatrixStore{authzStore: cfg.Store.(*authzStore),
			bySub: map[string]*types.GovernanceProfile{subA: profA, subB: profB}}
	})
	profileIDs := map[string]*uuid.UUID{subA: &profA.ID, subB: &profB.ID}
	runs := map[string]uuid.UUID{}
	for _, sub := range []string{subA, subB} {
		id := uuid.New()
		runs[sub] = id
		ast.mu.Lock()
		ast.runs[id] = types.AgentRun{ID: id, CreatedBy: sub, State: types.RunRunning, Agent: "claude-code",
			GovernanceProfileID: profileIDs[sub]}
		ast.mu.Unlock()
	}
	cookies := map[string]*http.Cookie{
		subA: ssoSession(t, subA, subA+"@corp.example", oidc.RoleUser),
		subB: ssoSession(t, subB, subB+"@corp.example", oidc.RoleUser),
	}

	var routes [][2]string
	if err := chi.Walk(srv.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes = append(routes, [2]string{method, route})
		return nil
	}); err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	if len(routes) < 100 {
		t.Fatalf("walked %d routes, want the whole router", len(routes))
	}

	seen := map[string]map[string]bool{subA: {}, subB: {}} // own markers seen, per member
	probe := func(sub, method, path, body string) {
		w := doSSO(t, srv, method, path, cookies[sub], body)
		got := w.Body.String()
		for _, m := range markers[other[sub]] {
			if strings.Contains(got, m) {
				t.Errorf("%s %s as %s: body carries %q, which belongs to the other member's profile: %s", method, path, sub, m, got)
			}
		}
		for _, m := range markers[sub] {
			if strings.Contains(got, m) {
				seen[sub][path] = true
			}
		}
	}
	for _, rt := range routes {
		method, route := rt[0], rt[1]
		body := bodyFor(method, routeMatrix[method+" "+route])
		for _, sub := range []string{subA, subB} {
			for _, owner := range []string{subA, subB} {
				probe(sub, method, buildPath(route, runs[owner].String()), body)
			}
		}
	}

	// The refusals, through POST /runs: exec, a task-less (interactive) create
	// and a plain create at the one-run quota.
	for _, sub := range []string{subA, subB} {
		for _, body := range []string{
			`{"agent":"claude-code","task":"echo hi","task_mode":"exec"}`,
			`{"agent":"claude-code"}`,
			`{"agent":"claude-code","task":"t"}`,
		} {
			probe(sub, http.MethodPost, "/api/v1/runs", body)
		}
	}

	// The positive controls: each member does see their own profile where the
	// deny-f2 contract says they do. Without these an empty walk passes.
	for _, sub := range []string{subA, subB} {
		for what, path := range map[string]string{
			"GET /me": "/api/v1/me", "GET /runs/{id}": "/api/v1/runs/" + runs[sub].String(), "a POST /runs refusal": "/api/v1/runs",
		} {
			if !seen[sub][path] {
				t.Errorf("%s never saw their own profile on %s", sub, what)
			}
		}
	}
}
