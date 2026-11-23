// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// GET /runs/{id}/clones against a real Postgres: the four stored states, pending, and a run that never cloned.
// Guarded by WARDYN_TEST_PG (throwawayPGPool); skipped cleanly when unset.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const clonesOwner = "sub-clones-owner"

// clonesPG is a server over a real database and the cookie of the member who owns every run it makes.
type clonesPG struct {
	srv   *Server
	pg    store.PG
	owner *http.Cookie
}

func newClonesPG(t *testing.T) *clonesPG {
	t.Helper()
	pool := throwawayPGPool(t)
	pg := store.NewPG(pool)
	h := newHarness(t)
	h.srv.cfg.Store = pg
	h.srv.cfg.Audit = store.Recorder{Pool: pool} // the audit table itself, CHECK included
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	h.srv.router = h.srv.routes()
	return &clonesPG{srv: h.srv, pg: pg, owner: ssoSession(t, clonesOwner, "owner@corp.example", oidc.RoleUser)}
}

// run persists the owner's run in state, with its policy recorded asking for repos (nil: a run with no repositories).
func (c *clonesPG) run(t *testing.T, state types.RunState, repos []types.WorkspaceRepo) types.AgentRun {
	t.Helper()
	now := time.Now().UTC()
	id := uuid.New()
	run, err := c.pg.CreateRun(t.Context(), types.AgentRun{
		ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: clonesOwner, Agent: "claude-code", Task: "clones",
		ConfinementClass: types.CC2, State: state, SPIFFEID: "spiffe://wardyn.test/agent-run/" + id.String(), RunnerTarget: "docker",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	c.srv.recordAudit(t.Context(), c.srv.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.policy.resolve", run.ID.String(), "success",
		mustJSON(effectivePolicyDatum{RunPolicySpec: types.RunPolicySpec{WorkspaceRepos: repos}})))
	return run
}

func (c *clonesPG) put(t *testing.T, run types.AgentRun, state, repos string) {
	t.Helper()
	if _, err := c.pg.PutRunCloneStatus(t.Context(), store.RunCloneStatus{RunID: run.ID, State: state, Repos: json.RawMessage(repos), ObservedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("put clone status: %v", err)
	}
}

func (c *clonesPG) get(t *testing.T, cookie *http.Cookie, run types.AgentRun) (int, runClonesResponse) {
	t.Helper()
	w := doSSO(t, c.srv, http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/clones", cookie, "")
	var resp runClonesResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v (%s)", err, w.Body.String())
		}
	}
	return w.Code, resp
}

func (c *clonesPG) want(t *testing.T, run types.AgentRun, state string, repos ...runRepoClone) {
	t.Helper()
	code, resp := c.get(t, c.owner, run)
	if code != http.StatusOK || resp.State != state || resp.RunID != run.ID || len(resp.Repos) != len(repos) {
		t.Fatalf("GET clones = %d %+v, want state %q with %d repos", code, resp, state, len(repos))
	}
	for i, r := range repos {
		if resp.Repos[i] != r {
			t.Errorf("repo %d = %+v, want %+v", i, resp.Repos[i], r)
		}
	}
}

var clonesAsked = []types.WorkspaceRepo{{Repo: "acme/alpha"}, {Repo: "acme/beta"}}

func TestRunClones_Handler_PG(t *testing.T) {
	c := newClonesPG(t)
	alpha := runRepoClone{Repo: "acme/alpha", Dest: "/home/agent/work/alpha", Outcome: "failed", Cause: "clone_failed"}
	beta := runRepoClone{Repo: "acme/beta", Dest: "/home/agent/work/beta", Outcome: "ok"}

	run := c.run(t, types.RunRunning, clonesAsked)
	c.want(t, run, clonesStatePending) // no row
	c.put(t, run, store.CloneStateCloning, `[]`)
	c.want(t, run, clonesStatePending) // the launcher's marker, the run still going

	reported := c.run(t, types.RunRunning, clonesAsked)
	raw, _ := json.Marshal([]runRepoClone{alpha, beta})
	c.put(t, reported, store.CloneStateReported, string(raw))
	c.want(t, reported, clonesStateReported, alpha, beta)

	notReported := c.run(t, types.RunRunning, clonesAsked)
	raw, _ = json.Marshal(notReportedClones([]cloneExpect{{"/home/agent/work/alpha", "acme/alpha"}}))
	c.put(t, notReported, store.CloneStateNotReported, string(raw))
	c.want(t, notReported, clonesStateNotReported, runRepoClone{Repo: "acme/alpha", Dest: "/home/agent/work/alpha", Outcome: "not_reported"})

	none := c.run(t, types.RunRunning, nil)
	c.put(t, none, store.CloneStateNone, `[]`)
	c.want(t, none, clonesStateNone)

	// Another member's run is the run's own 404.
	other := ssoSession(t, "sub-clones-other", "other@corp.example", oidc.RoleUser)
	if code, _ := c.get(t, other, reported); code != http.StatusNotFound {
		t.Errorf("another member's GET = %d, want 404", code)
	}
}

// A run that never reached the clone step reads none, with no repos. The check fails if the handler answers
// not_reported for a terminal run that has no row.
func TestRunClones_NoneForRunsThatNeverCloned_PG(t *testing.T) {
	c := newClonesPG(t)
	for name, run := range map[string]types.AgentRun{
		"a terminal run from before the upgrade (no row)": c.run(t, types.RunCompleted, clonesAsked),
		"a terminal run with no repositories":             c.run(t, types.RunCompleted, nil),
		"a run that failed while queued (no sandbox)":     c.run(t, types.RunFailed, clonesAsked),
		"a run that ended before any sweep reached it":    c.run(t, types.RunKilled, clonesAsked),
	} {
		code, resp := c.get(t, c.owner, run)
		if code != http.StatusOK || resp.State != clonesStateNone || len(resp.Repos) != 0 {
			t.Errorf("%s: GET clones = %d %+v, want none with no repos", name, code, resp)
		}
	}
}

// It reached the clone step and ended before the sweep wrote an outcome: not_reported, one entry for each repository
// the stored request asked for.
func TestRunClones_EndedWhileCloningIsNotReported_PG(t *testing.T) {
	c := newClonesPG(t)
	run := c.run(t, types.RunKilled, clonesAsked)
	c.put(t, run, store.CloneStateCloning, `[]`)
	c.want(t, run, clonesStateNotReported,
		runRepoClone{Repo: "acme/alpha", Dest: "/home/agent/work/alpha", Outcome: "not_reported"},
		runRepoClone{Repo: "acme/beta", Dest: "/home/agent/work/beta", Outcome: "not_reported"})

	repoless := c.run(t, types.RunKilled, nil)
	c.put(t, repoless, store.CloneStateCloning, `[]`)
	c.want(t, repoless, clonesStateNone)
}
