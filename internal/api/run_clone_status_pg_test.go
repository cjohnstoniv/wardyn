// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// GET /runs/{id}/clones against a real Postgres: the four stored states, pending, and a run that never cloned.
// Guarded by WARDYN_TEST_PG (throwawayPGPool); skipped cleanly when unset.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
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

// cloneReviveRunner is a runner that replaces a proxy, starts a sandbox and answers the launcher's state file.
type cloneReviveRunner struct {
	*pgReviveRunner
	file    string
	started atomic.Int32
}

func (r *cloneReviveRunner) StartSandbox(context.Context, string) error {
	r.started.Add(1)
	return nil
}

func (r *cloneReviveRunner) ExecStream(_ context.Context, _ string, _ runner.ExecSpec) (*runner.ExecSession, error) {
	return textPane(r.file, 0)(runner.ExecSpec{})
}

// reviveWithCloneRow stores a lost run that asked for two repositories, with a final clone row in which the first
// failed, and revives it as its owner.
func reviveWithCloneRow(t *testing.T, lost types.LostReason, launcherFile string) (*harness, types.AgentRun, *cloneReviveRunner) {
	t.Helper()
	h, _ := newRunOwnerPGHarness(t)
	ctx := t.Context()
	pg := h.srv.cfg.Store.(store.PG)
	h.srv.cfg.Audit = store.Recorder{Pool: pg.Pool} // the revive reads the run's policy from the audit table
	const owner = "sub-pg-clones-revive"
	run, err := pg.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: owner, Agent: "claude-code",
		ConfinementClass: types.CC1, State: types.RunRunning, RunnerTarget: "docker", Task: "t"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := pg.SetSandboxRef(ctx, run.ID, "wardyn-agent-"+run.ID.String()); err != nil {
		t.Fatalf("SetSandboxRef: %v", err)
	}
	if ok, err := pg.MarkRunLost(ctx, run.ID, lost, time.Now(), 0); err != nil || !ok {
		t.Fatalf("MarkRunLost: %v %v", ok, err)
	}
	h.srv.recordAudit(ctx, h.srv.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.policy.resolve", run.ID.String(), "success",
		mustJSON(effectivePolicyDatum{RunPolicySpec: types.RunPolicySpec{WorkspaceRepos: clonesAsked}})))
	raw, _ := json.Marshal([]runRepoClone{{Repo: "acme/alpha", Dest: "/home/agent/work/alpha", Outcome: "failed", Cause: "clone_failed"},
		{Repo: "acme/beta", Dest: "/home/agent/work/beta", Outcome: "ok"}})
	if _, err := pg.PutRunCloneStatus(ctx, store.RunCloneStatus{RunID: run.ID, State: store.CloneStateReported, Repos: raw, ObservedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	cfg, err := runner.BuildProxyConfig(run.ID, runner.ProxyConfig{
		RunToken: "old", ControlPlaneURL: "http://127.0.0.1:8081", Policy: types.RunPolicySpec{AllowedDomains: []string{"api.example.com"}},
	}, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	h.srv.cfg.RunConfigKey = make([]byte, 32)
	if err := h.srv.storeRunProxyConfig(ctx, run.ID, cfg); err != nil {
		t.Fatalf("store the run's proxy config: %v", err)
	}
	rn := &cloneReviveRunner{pgReviveRunner: &pgReviveRunner{fakeRunner: &fakeRunner{}}, file: launcherFile}
	h.srv.cfg.Runner = rn
	h.srv.cfg.ControlPlaneURL = "http://127.0.0.1:8080"
	w := doSSO(t, h.srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/revive", ssoSession(t, owner, ownerEmail, oidc.RoleUser), "")
	if w.Code != http.StatusOK || rn.replaced != 1 || strings.Contains(w.Body.String(), `"ok":false`) {
		t.Fatalf("revive = %d %s (replaced %d); want 200 and one new proxy", w.Code, w.Body.String(), rn.replaced)
	}
	return h, run, rn
}

// A revive that starts the agent again re-runs the launcher's clones, so the first start's outcome is forgotten and
// the run reads pending until this start's file is read. It fails if the clear is removed: the first start's
// "failed" would still be the answer after the repository cloned. A proxy-only revive starts no launcher, so it
// keeps the row.
func TestRunClones_ReviveClearsTheRowAndRereads_PG(t *testing.T) {
	t.Run("the agent starts again", func(t *testing.T) {
		file := cloneFile(true, cloneEntry("/home/agent/work/alpha", "ok", ""), cloneEntry("/home/agent/work/beta", "ok", ""))
		h, run, rn := reviveWithCloneRow(t, types.LostReboot, file)
		if rn.started.Load() != 1 {
			t.Fatalf("the agent was started %d times, want 1", rn.started.Load())
		}
		cs := h.srv.cfg.Store.(store.RunCloneStatusStore)
		if _, err := cs.GetRunCloneStatus(t.Context(), run.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("after the revive: err %v, want the row gone", err)
		}
		owner := ssoSession(t, "sub-pg-clones-revive", ownerEmail, oidc.RoleUser)
		get := func() runClonesResponse {
			w := doSSO(t, h.srv, http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/clones", owner, "")
			var resp runClonesResponse
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &resp) != nil {
				t.Fatalf("GET clones = %d %s", w.Code, w.Body.String())
			}
			return resp
		}
		if got := get(); got.State != clonesStatePending {
			t.Fatalf("GET clones = %+v, want pending", got)
		}
		listed, err := cs.ListCloneStatusRuns(t.Context())
		if err != nil || !slices.ContainsFunc(listed, func(r types.AgentRun) bool { return r.ID == run.ID }) {
			t.Fatalf("the revived run is not listed for the sweep: %v, %v", listed, err)
		}
		fresh, err := h.srv.cfg.Store.GetRun(t.Context(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		h.srv.readCloneRuns(cs, h.srv.cloneSweepMemory(), []cloneRead{{ctx: t.Context(), run: fresh}}, 0)
		got := get()
		if got.State != clonesStateReported || len(got.Repos) != 2 || got.Repos[0].Outcome != "ok" {
			t.Errorf("GET clones = %+v, want reported with the first repository ok", got)
		}
	})
	t.Run("a proxy-only revive", func(t *testing.T) {
		h, run, rn := reviveWithCloneRow(t, types.LostOutage, cloneFile(true))
		if rn.started.Load() != 0 {
			t.Fatalf("the agent was started %d times, want none", rn.started.Load())
		}
		row, err := h.srv.cfg.Store.(store.RunCloneStatusStore).GetRunCloneStatus(t.Context(), run.ID)
		if err != nil || row.State != store.CloneStateReported {
			t.Errorf("row %+v, err %v; want the reported row kept", row, err)
		}
	})
}
