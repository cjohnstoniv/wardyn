// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runWarnStore is capStore (capability rows) plus the run methods
// handleCreateRun needs to reach its 201 with no runner and no Postgres —
// CreateGrant only matters for a request whose EligibleGrants actually
// survives narrowing (a member's own-key grant, say); the narrowing-only
// tests below never reach it.
type runWarnStore struct {
	*capStore
	created types.AgentRun
}

func (s *runWarnStore) ListRuns(context.Context) ([]types.AgentRun, error) { return nil, nil }

func (s *runWarnStore) ListWorkspaces(context.Context) ([]types.Workspace, error) { return nil, nil }

func (s *runWarnStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, nil
}

func (s *runWarnStore) SetRunImage(context.Context, uuid.UUID, string) error { return nil }

func (s *runWarnStore) CreateGrant(_ context.Context, g types.CredentialGrant) (types.CredentialGrant, error) {
	return g, nil
}

func (s *runWarnStore) CreateRun(_ context.Context, run types.AgentRun) (types.AgentRun, error) {
	s.created = run
	return run, nil
}

// TestCreateRun_SurfacesMemberNarrowingWarnings: a member launching an inline
// policy whose egress host they do not hold gets a 201 — the run is narrowed,
// never refused — carrying a warning that NAMES the dropped host.
//
// This is the wire the console depends on: it launches without preflighting, so
// before this the drop was invisible everywhere except `wardyn run --dry-run`,
// while the permissions copy and OPERATIONS both promised "a warning naming it".
func TestCreateRun_SurfacesMemberNarrowingWarnings(t *testing.T) {
	h := newHarness(t)
	st := &runWarnStore{capStore: &capStore{enf: map[string]bool{capEgressHost: true}}}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	// The operator ceiling carries both hosts, so composer.Clamp keeps them and
	// the ONLY thing that can drop one is the member's own capability set.
	cfg.DefaultPolicy = types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		AllowedDomains:      []string{"api.anthropic.com", "evil.example.com"},
	}
	srv := New(cfg)

	body := `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com","evil.example.com"]}}`
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs",
		ssoSession(t, "sub-warn-member", "dev@corp.example", oidc.RoleMember), body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (a drop narrows, it does not refuse): %s", w.Code, w.Body.String())
	}
	var got createRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	var named bool
	for _, warn := range got.Warnings {
		if strings.Contains(warn, "evil.example.com") {
			named = true
		}
	}
	if !named {
		t.Fatalf("warnings = %v, want one naming the dropped host evil.example.com", got.Warnings)
	}
	// The narrowing itself (that the host really is gone from the resolved
	// spec) is capability_seams_test.go's; this test's whole job is that the
	// caller HEARS about it.
	if st.created.ID == (types.AgentRun{}).ID {
		t.Fatal("no run was persisted, so the 201 above proved nothing")
	}
}

// collisionStore answers the workspace-collision question BOTH ways and counts
// which way it was asked: the targeted store read and the unbounded ListRuns
// fallback. Both return the same collision, so a test that gets the right
// warning from the wrong read still fails — which is the whole point, because
// the defect here is a performance one and produces identical output.
type collisionStore struct {
	store.Store
	runs          []types.AgentRun
	listRunsCalls int
	activeCalls   int
	activePath    string
}

func (s *collisionStore) ListRuns(context.Context) ([]types.AgentRun, error) {
	s.listRunsCalls++
	return s.runs, nil
}

func (s *collisionStore) ActiveRunsAtWorkspacePath(_ context.Context, path string) ([]types.AgentRun, error) {
	s.activeCalls++
	s.activePath = path
	var out []types.AgentRun
	for _, r := range s.runs {
		if r.WorkspacePath == path && !r.State.IsTerminal() {
			out = append(out, r)
		}
	}
	return out, nil
}

// TestWorkspaceCollisionAsksTheQuestionItMeans pins the READ, not the sentence.
//
// warnWorkspaceCollision is advisory and never blocks a launch, and it ran on
// EVERY run create — loading every run in the deployment (ListRuns is
// documented "all runs in reverse creation order (unbounded)") through a Seq
// Scan plus a full sort of agent_runs, a table nothing prunes and no retention
// policy bounds, to decide whether to print one sentence that usually is not
// printed. The predicate is two columns; Postgres can answer it with a WHERE.
//
// THE OUTPUT IS IDENTICAL EITHER WAY, which is exactly why this needs a test
// that watches the read: every existing assertion about the warning's text
// passes on both implementations, so nothing stood between the fix and a
// silent revert to the full scan.
func TestWorkspaceCollisionAsksTheQuestionItMeans(t *testing.T) {
	const path = "/srv/shared-workspace"
	mine, other, done, elsewhere := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	st := &collisionStore{runs: []types.AgentRun{
		{ID: other, WorkspacePath: path, State: types.RunRunning},
		{ID: done, WorkspacePath: path, State: types.RunCompleted},
		{ID: elsewhere, WorkspacePath: "/srv/other", State: types.RunRunning},
		{ID: mine, WorkspacePath: path, State: types.RunPending},
	}}
	srv := New(Config{Store: st, Audit: &recRecorder{}, RunnerTarget: "docker"})

	// The caller here holds no verified human (the admin-token / local-mode
	// shape), so isSecurityOperator is true and every colliding run is visible
	// to them — this test is about the READ, and F336's ownership filter is
	// pinned separately in r3_workspace_collision_test.go.
	warnings := srv.warnWorkspaceCollision(collisionRequest(), mine, path)

	if st.activeCalls != 1 {
		t.Errorf("ActiveRunsAtWorkspacePath called %d times, want exactly 1 — the create path must ask "+
			"the question it means", st.activeCalls)
	}
	if st.listRunsCalls != 0 {
		t.Errorf("ListRuns called %d times — that is the UNBOUNDED read of every run in the deployment, "+
			"on every single run create", st.listRunsCalls)
	}
	if st.activePath != path {
		t.Errorf("the read was scoped to %q, want the run's own workspace path %q", st.activePath, path)
	}

	// AND THE ANSWER IS UNCHANGED, in all three directions the filter decides:
	// the other live run collides, the finished one does not, the run being
	// created is not its own collision, and a run elsewhere is irrelevant.
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one collision warning", warnings)
	}
	if !strings.Contains(warnings[0], other.String()) {
		t.Errorf("warning = %q, want it to name the colliding run %s", warnings[0], other)
	}
	for _, id := range []uuid.UUID{mine, done, elsewhere} {
		if strings.Contains(warnings[0], id.String()) {
			t.Errorf("warning = %q names %s, which is not a live collision on this path", warnings[0], id)
		}
	}

	// NO COLLISION reads nothing beyond the one scoped query and warns nothing.
	clean := &collisionStore{runs: st.runs}
	srvClean := New(Config{Store: clean, Audit: &recRecorder{}, RunnerTarget: "docker"})
	if w := srvClean.warnWorkspaceCollision(collisionRequest(), uuid.New(), "/srv/untouched"); len(w) != 0 {
		t.Errorf("warnings = %v for a path nothing runs on, want none", w)
	}
	if clean.listRunsCalls != 0 {
		t.Errorf("ListRuns called %d times on the no-collision path", clean.listRunsCalls)
	}
}
