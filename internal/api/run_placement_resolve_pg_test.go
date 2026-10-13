// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The runner pool fold against a real Postgres (D-117): the pool a run resolves to, the candidates that
// pool offers the launching person, and the binding the run row carries. Guarded by WARDYN_TEST_PG: skipped
// cleanly when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// poolRunner is a deployment with two executors that differ (both enforce CC1-CC3 until a test narrows one) and
// registered runners whose advertised facts a test sets. It records where it was told to create.
type poolRunner struct {
	*fakeRunner
	execFacts   map[string]runner.TargetFacts
	runnerFacts map[string]runner.TargetFacts
	// silent executors are run by the deployment but cannot report their facts.
	silent     map[string]bool
	onExecutor []string
}

func (p *poolRunner) Executors() []string {
	out := make([]string, 0, len(p.execFacts)+len(p.silent))
	for name := range p.execFacts {
		out = append(out, name)
	}
	for name := range p.silent {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

func (p *poolRunner) ExecutorFacts(_ context.Context, name string) (runner.TargetFacts, bool) {
	f, ok := p.execFacts[name]
	return f, ok
}

func (p *poolRunner) RunnerFacts(id string) (runner.TargetFacts, bool) {
	f, ok := p.runnerFacts[id]
	return f, ok
}

func (p *poolRunner) CreateSandboxOn(_ context.Context, executor string, spec runner.SandboxSpec) (runner.Sandbox, error) {
	p.fakeRunner.mu.Lock()
	p.onExecutor = append(p.onExecutor, executor)
	p.fakeRunner.lastSpec = spec
	p.fakeRunner.mu.Unlock()
	return runner.Sandbox{Ref: "fake-" + spec.RunID.String(), Driver: executor, EnforcedClass: spec.ConfinementClass}, nil
}

type placementEnv struct {
	*poolEnv
	runner *poolRunner
	online map[uuid.UUID]bool
}

func newPlacementEnv(t *testing.T) *placementEnv {
	t.Helper()
	e := &placementEnv{poolEnv: newPoolEnv(t, false), online: map[uuid.UUID]bool{}}
	e.runner = &poolRunner{fakeRunner: &fakeRunner{},
		execFacts: map[string]runner.TargetFacts{
			"alpha": {Classes: []types.ConfinementClass{types.CC1, types.CC2, types.CC3}},
			"beta":  {Classes: []types.ConfinementClass{types.CC1, types.CC2, types.CC3}},
		},
		runnerFacts: map[string]runner.TargetFacts{}}
	e.srv.cfg.Runner = e.runner
	e.srv.cfg.RunnerOnline = func(id uuid.UUID) bool { return e.online[id] }
	e.srv.cfg.AgentImages = composeAgentImages
	e.ensureRemoteRunners()
	if _, err := e.pg.PutSiteConfig(context.Background(), types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	return e
}

// laptop is a claimed runner of owner that is online and advertises the given classes and size.
// laptop is an online runner of owner reporting classes, whose owner declared a per-run limit of cpu and
// mem (roomy totals); 0 for either declares no limits at all.
func (e *placementEnv) laptop(owner string, cpu, mem int64, classes ...types.ConfinementClass) uuid.UUID {
	e.t.Helper()
	id := e.claimedRunner(owner)
	e.online[id] = true
	e.runner.runnerFacts[id.String()] = runner.TargetFacts{Classes: classes, CapacityAdvertised: true, CPUMillisMax: cpu, MemoryMiBMax: mem}
	q, args := `UPDATE runners SET run_cpu_millis = $2, run_memory_mib = $3 WHERE id = $1`, []any{id, cpu, mem}
	if cpu == 0 || mem == 0 {
		q, args = `UPDATE runners SET total_cpu_millis = NULL, total_memory_mib = NULL, total_disk_mib = NULL,
			run_cpu_millis = NULL, run_memory_mib = NULL, run_disk_mib = NULL WHERE id = $1`, []any{id}
	}
	if _, err := e.pool.Exec(context.Background(), q, args...); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *placementEnv) addToPool(c *http.Cookie, pool, runnerID uuid.UUID) {
	e.t.Helper()
	if w := e.call(c, http.MethodPut, addRunnerPath(pool, runnerID), ""); w.Code >= 300 {
		e.t.Fatalf("add runner = %d %s", w.Code, w.Body)
	}
}

func (e *placementEnv) addExecutors(pool uuid.UUID, names ...string) {
	e.t.Helper()
	for _, n := range names {
		if w := e.admin(http.MethodPut, "/api/v1/runner-pools/"+pool.String()+"/remote-runners/"+n, ""); w.Code != http.StatusOK {
			e.t.Fatalf("add executor %s = %d %s", n, w.Code, w.Body)
		}
	}
}

func (e *placementEnv) orgDefaults(body string) {
	e.t.Helper()
	if w := e.admin(http.MethodPut, "/api/v1/runner-pool-defaults", body); w.Code != http.StatusOK {
		e.t.Fatalf("org defaults = %d %s", w.Code, w.Body)
	}
}

func (e *placementEnv) personDefaults(c *http.Cookie, body string) {
	e.t.Helper()
	if w := e.call(c, http.MethodPut, "/api/v1/me/runner-pool-defaults", body); w.Code != http.StatusOK {
		e.t.Fatalf("personal defaults = %d %s", w.Code, w.Body)
	}
}

const placementRunBody = `"agent":"claude-code","repo":"acme/widgets","task":"t"`

func runBody(extra string) string {
	if extra == "" {
		return `{` + placementRunBody + `}`
	}
	return `{` + placementRunBody + `,` + extra + `}`
}

var placementDoors = []string{"/api/v1/runs", "/api/v1/runs/preflight", "/api/v1/runs/policy-preview"}

// answers posts one body to every door as c (nil: the admin token) and returns each answer.
func (e *placementEnv) answers(c *http.Cookie, body string) map[string]*httptest.ResponseRecorder {
	e.t.Helper()
	out := map[string]*httptest.ResponseRecorder{}
	for _, door := range placementDoors {
		if c == nil {
			out[door] = e.admin(http.MethodPost, door, body)
		} else {
			out[door] = e.call(c, http.MethodPost, door, body)
		}
	}
	return out
}

// sameEverywhere requires the three doors to answer one status, reason and sentence.
func sameEverywhere(t *testing.T, name string, got map[string]*httptest.ResponseRecorder, status int, reason string) string {
	t.Helper()
	var first string
	for _, door := range placementDoors {
		w := got[door]
		if w.Code != status || wireReason(t, w) != reason {
			t.Errorf("%s %s = %d %s, want %d %s", name, door, w.Code, w.Body, status, reason)
		}
		if first == "" {
			first = w.Body.String()
		} else if w.Body.String() != first {
			t.Errorf("%s %s answered %s, another door answered %s", name, door, w.Body, first)
		}
	}
	return first
}

// DONE WHEN 1: create, preflight and preview answer every pool reason identically. The step that does
// it is pinned in TestPreflightMirrorsLaunchGates; runner_pool_stale is the one reason only a launch can
// meet (a pool that moved between the read and the insert), and runner_pools_unavailable needs a store
// that cannot hold pools (TestPlacement_StoreWithoutPoolsKeepsUnpooledBehaviour).
func TestPG_PoolReasonsAreTheSameAtEveryDoor(t *testing.T) {
	e := newPlacementEnv(t)
	remote := e.createPool("Remote", types.RunnerPoolOrganization)
	e.addExecutors(remote.ID, "alpha", "beta")
	dead := e.createPool("Switched off", types.RunnerPoolOrganization)
	e.addExecutors(dead.ID, "alpha")
	if w := e.admin(http.MethodPut, "/api/v1/runner-pools/"+dead.ID.String(), fmt.Sprintf(`{"revision":%d,"state":"disabled"}`, e.revision(dead.ID))); w.Code != http.StatusOK {
		t.Fatalf("disable = %d %s", w.Code, w.Body)
	}
	empty := e.createPool("Empty remote", types.RunnerPoolOrganization)
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	handled := map[runnerpool.Reason]bool{runnerpool.ReasonStale: true, runnerpool.ReasonPoolsUnavailable: true}

	check := func(name string, c *http.Cookie, extra string, status int, reason runnerpool.Reason) {
		t.Helper()
		handled[reason] = true
		sameEverywhere(t, name, e.answers(c, runBody(extra)), status, string(reason))
	}
	check("a malformed pool id", e.person, `"runner_pool_id":"nope"`, http.StatusBadRequest, runnerpool.ReasonInvalid)
	check("no pool and no default", e.person, "", http.StatusUnprocessableEntity, runnerpool.ReasonRequired)
	check("an unknown pool", e.person, `"runner_pool_id":"`+uuid.NewString()+`"`, http.StatusNotFound, runnerpool.ReasonNotFound)
	check("a switched-off pool", e.person, `"runner_pool_id":"`+dead.ID.String()+`"`, http.StatusUnprocessableEntity, runnerpool.ReasonUnavailable)
	check("a remote pool with no executor", e.person, `"runner_pool_id":"`+empty.ID.String()+`"`, http.StatusUnprocessableEntity, runnerpool.ReasonNoEligibleMember)
	check("a self-hosted pool with none of the caller's runners", e.person, `"runner_pool_id":"`+laptops.ID.String()+`"`, http.StatusUnprocessableEntity, runnerpool.ReasonNoEligibleMember)
	check("local placement against a remote pool", e.person, `"placement":"local","runner_pool_id":"`+remote.ID.String()+`"`, http.StatusUnprocessableEntity, runnerpool.ReasonMemberMismatch)
	check("a pin that is not the caller's", e.person, `"placement":"local","runner_id":"`+uuid.NewString()+`","runner_pool_id":"`+laptops.ID.String()+`"`, http.StatusUnprocessableEntity, runnerpool.ReasonMemberMismatch)
	soon := e.createPool("Soon off", types.RunnerPoolOrganization)
	e.addExecutors(soon.ID, "alpha")
	limited := e.limitedPool("Background only", types.CC1, types.CC2)
	const background = `"title":"t","experience":"background","workload":{"kind":"agent_task","agent":"claude-code","task":"fix the build"},"tools":[{"id":"claude-code","kind":"harness"}],"no_repositories_or_drives":true`
	const interactive = `"title":"t","experience":"interactive","tools":[{"id":"claude-code","kind":"harness"}],"no_repositories_or_drives":true`
	pinned := `"runner_pool_id":"` + limited.ID.String() + `"`
	checkBody := func(name, body string, status int, reason runnerpool.Reason) {
		t.Helper()
		handled[reason] = true
		sameEverywhere(t, name, e.answers(e.person, body), status, string(reason))
	}
	checkBody("an interactive run in a Background-only pool", `{`+interactive+`,`+pinned+`}`, http.StatusUnprocessableEntity, runnerpool.ReasonRunTypeNotAllowed)
	checkBody("a barrier the pool does not allow", `{`+background+`,"confinement_class":"CC3",`+pinned+`}`, http.StatusUnprocessableEntity, runnerpool.ReasonBarrierNotAllowed)
	// D-1319 carry: a request that names no experience (an older client) is never read as Background by a
	// pool that sets limits; it is refused at every door, and the same request in a pool with no limits is fine.
	checkBody("an older-shape request in a pool with limits", runBody(pinned), http.StatusUnprocessableEntity, runnerpool.ReasonRunTypeNotAllowed)
	e.orgDefaults(fmt.Sprintf(`{"pool_id":%q}`, soon.ID))
	if w := e.admin(http.MethodPut, "/api/v1/runner-pools/"+soon.ID.String(), fmt.Sprintf(`{"revision":%d,"state":"disabled"}`, e.revision(soon.ID))); w.Code != http.StatusOK {
		t.Fatalf("disable = %d %s", w.Code, w.Body)
	}
	check("an organisation default that is switched off", e.person, "", http.StatusUnprocessableEntity, runnerpool.ReasonDefaultUnavailable)
	e.personDefaults(e.person, fmt.Sprintf(`{"pool_id":%q}`, remote.ID))
	if _, err := e.pg.DeleteRunnerPool(context.Background(), remote.ID); err != nil {
		t.Fatal(err)
	}
	check("a personal default that was deleted", e.person, "", http.StatusUnprocessableEntity, runnerpool.ReasonDefaultUnavailable)

	// The queue's two refusals are one door each, not the three launch doors' shared ones: a create into
	// a person's 101st waiting run (TestPG_QueueFloodLimit) and a revive into a full pool
	// (TestPG_QueueReviveRefusedForAFullPool).
	handled[runnerpool.ReasonQueueFull], handled[runnerpool.ReasonFull] = true, true
	for _, r := range runnerpool.Reasons() {
		if !handled[r] {
			t.Errorf("reason %s is covered at no door", r)
		}
	}
}

// limitedPool is a remote pool that holds the executors, takes Background runs only and allows the given
// barriers.
func (e *placementEnv) limitedPool(name string, barriers ...types.ConfinementClass) types.RunnerPool {
	e.t.Helper()
	p := e.createPool(name, types.RunnerPoolOrganization)
	e.addExecutors(p.ID, "alpha", "beta")
	limits := types.RunnerPoolLimits{
		Background: &types.RunnerPoolBackgroundLimits{CPUMillis: &types.RunnerPoolAmount{Default: 1000, Cap: 2000}, MemoryMiB: &types.RunnerPoolAmount{Default: 1024, Cap: 2048}, Lifetime: &types.RunnerPoolDuration{DefaultSec: 3600, MaxSec: 7200}},
		Barriers:   barriers,
	}
	out, err := e.pg.UpdateRunnerPool(context.Background(), p.ID, e.revision(p.ID), nil, nil, &limits)
	if err != nil {
		e.t.Fatalf("set pool limits: %v", err)
	}
	return out
}

// DONE WHEN 2: a stale or deleted default refuses and never falls back to the next default, to another
// pool or to the other hosting type, even when a perfectly good one exists.
func TestPG_StaleDefaultNeverFallsBack(t *testing.T) {
	e := newPlacementEnv(t)
	good := e.createPool("Good", types.RunnerPoolOrganization)
	e.addExecutors(good.ID, "alpha")
	gone := e.createPool("Gone", types.RunnerPoolOrganization)
	e.addExecutors(gone.ID, "beta")
	e.orgDefaults(fmt.Sprintf(`{"pool_id":%q}`, good.ID))
	e.personDefaults(e.person, fmt.Sprintf(`{"pool_id":%q}`, gone.ID))

	if w := e.call(e.person, http.MethodPost, "/api/v1/runs", runBody("")); w.Code != http.StatusCreated {
		t.Fatalf("a valid personal default = %d %s", w.Code, w.Body)
	}
	if _, err := e.pg.DeleteRunnerPool(context.Background(), gone.ID); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*http.Cookie{e.person} {
		w := e.call(c, http.MethodPost, "/api/v1/runs", runBody(""))
		if w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != string(runnerpool.ReasonDefaultUnavailable) ||
			!strings.Contains(w.Body.String(), runnerpool.DefaultUnavailablePersonalMsg()) {
			t.Fatalf("a deleted personal default = %d %s, want the personal sentence and no fallback to the good organisation default", w.Code, w.Body)
		}
	}
	// The other person has no default of their own and gets the organisation's.
	if w := e.call(e.other, http.MethodPost, "/api/v1/runs", runBody("")); w.Code != http.StatusCreated {
		t.Fatalf("the organisation default for a person without one = %d %s", w.Code, w.Body)
	}
	if n := e.count(`SELECT count(*) FROM agent_runs WHERE created_by = $1`, e.personP); n != 1 {
		t.Errorf("the refused launches left %d extra runs", n-1)
	}
}

// DONE WHEN 8: the run row carries the placement, the evidence source and the pool binding written in its
// own insert, and a run from before pools reads back with none.
func TestPG_RunRowCarriesThePoolBinding(t *testing.T) {
	e := newPlacementEnv(t)
	ctx := context.Background()
	orgPool := e.createPool("Org", types.RunnerPoolOrganization)
	e.addExecutors(orgPool.ID, "alpha", "beta")
	named := e.createPool("Named", types.RunnerPoolOrganization)
	e.addExecutors(named.ID, "beta")
	e.orgDefaults(fmt.Sprintf(`{"pool_id":%q}`, orgPool.ID))

	created := func(c *http.Cookie, extra string) types.AgentRun {
		t.Helper()
		w := e.call(c, http.MethodPost, "/api/v1/runs", runBody(extra))
		if w.Code != http.StatusCreated {
			t.Fatalf("create = %d %s", w.Code, w.Body)
		}
		var run types.AgentRun
		if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
			t.Fatal(err)
		}
		stored, err := e.pg.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		return stored
	}
	check := func(name string, run types.AgentRun, pool types.RunnerPool, sel types.RunnerPoolSelection, executors ...string) {
		t.Helper()
		if run.Placement != types.PlacementRemote || !run.PlacementFilled || run.EvidenceSource != types.RunEvidenceSubstrate || run.RunnerID != nil ||
			run.RunnerPoolID == nil || *run.RunnerPoolID != pool.ID || run.RunnerPoolRevision != e.revision(pool.ID) || run.RunnerPoolSelection != sel ||
			!slices.Contains(executors, run.RunnerPoolExecutor) {
			t.Errorf("%s: row = placement %q filled %v evidence %q runner %v pool %v rev %d selection %q executor %q", name, run.Placement, run.PlacementFilled,
				run.EvidenceSource, run.RunnerID, run.RunnerPoolID, run.RunnerPoolRevision, run.RunnerPoolSelection, run.RunnerPoolExecutor)
		}
	}
	check("organisation default", created(e.person, ""), orgPool, types.RunnerPoolSelectedOrg, "alpha", "beta")
	e.personDefaults(e.person, fmt.Sprintf(`{"pool_id":%q}`, named.ID))
	check("a person's default beats the organisation's", created(e.person, ""), named, types.RunnerPoolSelectedPersonal, "beta")
	run := created(e.person, `"runner_pool_id":"`+orgPool.ID.String()+`"`)
	check("an explicit pool beats both", run, orgPool, types.RunnerPoolSelectedExplicit, "alpha", "beta")
	if run.PlacementFilled != true {
		t.Error("the pool decided the placement, so it is filled")
	}
	w := e.call(e.person, http.MethodPost, "/api/v1/runs", runBody(`"placement":"remote","runner_pool_id":"`+orgPool.ID.String()+`"`))
	var asked types.AgentRun
	_ = json.Unmarshal(w.Body.Bytes(), &asked)
	if stored, err := e.pg.GetRun(ctx, asked.ID); err != nil || stored.PlacementFilled || stored.Placement != types.PlacementRemote {
		t.Errorf("a placement the person asked for is not filled: %+v %v", stored, err)
	}

	// A pre-pool row reads back with no pool at all.
	old, err := e.pg.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: e.personP, Agent: "claude-code", Task: "t", State: types.RunPending, RunnerTarget: "docker", ConfinementClass: types.CC2})
	if err != nil {
		t.Fatal(err)
	}
	if old.RunnerPoolID != nil || old.RunnerPoolSelection != "" || old.RunnerPoolRevision != 0 || old.RunnerPoolExecutor != "" || old.Placement != "" {
		t.Errorf("a pre-pool row = %+v", old)
	}
	// The binding is all-or-nothing: a selection without a pool is not storable.
	if _, err := e.pool.Exec(ctx, `UPDATE agent_runs SET runner_pool_selection = 'explicit' WHERE id = $1`, old.ID); err == nil {
		t.Error("a pool selection with no pool was stored")
	}
}

// The pool changing between admission and the insert refuses the launch with runner_pool_stale and writes
// no run; the same insert at the admitted revision succeeds.
func TestPG_PoolMovingBeforeTheInsertRefusesTheLaunch(t *testing.T) {
	e := newPlacementEnv(t)
	ctx := context.Background()
	pool := e.createPool("Moving", types.RunnerPoolOrganization)
	e.addExecutors(pool.ID, "alpha")
	at := e.revision(pool.ID)
	row := func(rev int64) types.AgentRun {
		id := pool.ID
		return types.AgentRun{ID: uuid.New(), CreatedBy: e.personP, Agent: "claude-code", Task: "t", State: types.RunPending, RunnerTarget: "docker", ConfinementClass: types.CC2,
			Placement: types.PlacementRemote, EvidenceSource: types.RunEvidenceSubstrate, RunnerPoolID: &id, RunnerPoolRevision: rev,
			RunnerPoolSelection: types.RunnerPoolSelectedExplicit, RunnerPoolExecutor: "alpha"}
	}
	if _, err := e.pg.CreateRun(ctx, row(at)); err != nil {
		t.Fatalf("the admitted revision: %v", err)
	}
	e.addExecutors(pool.ID, "beta") // moves the pool's revision
	for name, create := range map[string]func(types.AgentRun) (types.AgentRun, error){
		"CreateRun":         func(r types.AgentRun) (types.AgentRun, error) { return e.pg.CreateRun(ctx, r) },
		"CreateRunUnderCap": func(r types.AgentRun) (types.AgentRun, error) { return e.pg.CreateRunUnderCap(ctx, r, 100) },
	} {
		r := row(at)
		if _, err := create(r); err != store.ErrRunnerPoolStale {
			t.Errorf("%s at a stale revision = %v, want ErrRunnerPoolStale", name, err)
		}
		if _, err := e.pg.GetRun(ctx, r.ID); err != store.ErrNotFound {
			t.Errorf("%s left a run behind: %v", name, err)
		}
	}
	if _, err := e.pg.CreateRun(ctx, row(e.revision(pool.ID))); err != nil {
		t.Errorf("the moved pool at its new revision: %v", err)
	}
}

// DONE WHEN 7 (and H14-api "a run never lands on another person's runner"): a Self-Hosted pool is its
// owner's and offers only the owner's runners. Even a membership row naming another person's runner,
// which no route can write, is never a candidate; another person, an administrator and the admin token
// can never be given, or pin, someone else's runner, and another person's launch into the pool reads
// exactly as a launch into a pool that does not exist.
func TestPlacement_SelfHostedCandidatesAreTheLaunchersOwn(t *testing.T) {
	e := newPlacementEnv(t)
	pool := e.createPool("Laptops", types.RunnerPoolUser)
	mine := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	theirs := e.laptop(e.otherP, 8000, 16384, types.CC1, types.CC2, types.CC3)
	e.addToPool(e.person, pool.ID, mine)
	if w := e.call(e.other, http.MethodPut, addRunnerPath(pool.ID, theirs), ""); w.Code != http.StatusNotFound {
		t.Fatalf("another person adding to the pool = %d %s, want 404", w.Code, w.Body)
	}
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO runner_pool_members (pool_id, runner_id) VALUES ($1, $2)`, pool.ID, theirs); err != nil {
		t.Fatal(err)
	}
	ps := e.srv.cfg.Store.(store.RunnerPoolStore)
	rows, _ := e.srv.permittedPools(operatorCtx(e.personP, e.personP+"@corp.example", "user"), ps, true)
	row, found := catalogueRow(rows, pool.ID)
	if !found {
		t.Fatal("the owner's catalogue lacks their own pool")
	}

	candidates := func(who string) []string {
		t.Helper()
		ctx := operatorCtx(who, who+"@corp.example", "user")
		ts, refusal := e.srv.ownRunnerTargets(ctx, &createRunRequest{}, ps, row)
		if refusal != nil && refusal.body.Reason != string(runnerpool.ReasonNoEligibleMember) {
			t.Fatalf("%s: %+v", who, refusal)
		}
		return ids(ts)
	}
	if got := candidates(e.personP); !slices.Equal(got, []string{mine.String()}) {
		t.Errorf("person's candidates = %v, want only %s", got, mine)
	}
	for _, nobody := range []string{e.otherP, "sub-alice", "", "sub-super", "sub-stranger"} {
		if got := candidates(nobody); len(got) != 0 {
			t.Errorf("%q was offered %v from a pool of other people's runners", nobody, got)
		}
	}

	// Through the doors: the admin token is no person and another person is not the owner, so the pool is
	// not theirs to choose, byte for byte as a pool id that never existed. (A security admin cannot launch
	// except in the user view, and is covered by candidates above.)
	body := runBody(`"runner_pool_id":"` + pool.ID.String() + `"`)
	sameEverywhere(t, "admin token", e.answers(nil, body), http.StatusNotFound, string(runnerpool.ReasonNotFound))
	foreignPool := sameEverywhere(t, "another person", e.answers(e.other, body), http.StatusNotFound, string(runnerpool.ReasonNotFound))
	randomPool := sameEverywhere(t, "a random pool", e.answers(e.other, runBody(`"runner_pool_id":"`+uuid.NewString()+`"`)), http.StatusNotFound, string(runnerpool.ReasonNotFound))
	if foreignPool != randomPool {
		t.Errorf("another person's pool answered %s, a random id %s", foreignPool, randomPool)
	}

	// Pinning another person's runner reads exactly like pinning one that does not exist, and never names it.
	pin := func(id uuid.UUID) string {
		return runBody(`"placement":"local","runner_id":"` + id.String() + `","runner_pool_id":"` + pool.ID.String() + `"`)
	}
	foreign := sameEverywhere(t, "a foreign pin", e.answers(e.person, pin(theirs)), http.StatusUnprocessableEntity, string(runnerpool.ReasonMemberMismatch))
	unknownID := uuid.New()
	unknown := sameEverywhere(t, "an unknown pin", e.answers(e.person, pin(unknownID)), http.StatusUnprocessableEntity, string(runnerpool.ReasonMemberMismatch))
	if strings.ReplaceAll(foreign, theirs.String(), "X") != strings.ReplaceAll(unknown, unknownID.String(), "X") || strings.Contains(foreign, "laptop") {
		t.Errorf("a foreign pin answered %s, an unknown one %s", foreign, unknown)
	}
	// Their own runner, in the pool, is where the run goes.
	for door, w := range e.answers(e.person, pin(mine)) {
		if w.Code >= http.StatusBadRequest {
			t.Errorf("%s with their own runner = %d %s, want it admitted", door, w.Code, w.Body)
		}
	}
	// And a revoked, an offline or an other-organisation runner of their own is no candidate.
	e.online[mine] = false
	if got := candidates(e.personP); len(got) != 1 {
		t.Errorf("an offline runner must stay a candidate (to be refused as offline), got %v", got)
	}
	sameEverywhere(t, "offline", e.answers(e.person, runBody(`"runner_pool_id":"`+pool.ID.String()+`"`)), http.StatusUnprocessableEntity, string(placement.ReasonRunnerOffline))
}

// DONE WHEN 5 and 6: the dry doors show the permitted pools and what the pool resolved to, never a
// target; and a pool the caller may not use, and a default naming one, read as an unknown id.
func TestPG_PreviewShowsPoolsNotTargetsAndLeaksNothing(t *testing.T) {
	e := newPlacementEnv(t)
	open := e.createPool("Open", types.RunnerPoolOrganization)
	e.addExecutors(open.ID, "alpha", "beta")
	secret := e.createPool("Finance only", types.RunnerPoolOrganization)
	e.addExecutors(secret.ID, "alpha")
	if w := e.call(e.alice, http.MethodPut, "/api/v1/runner-pools/"+secret.ID.String()+"/use-policy", `{"subjects":[{"subject_type":"user","subject":"sub-someone-else"}]}`); w.Code != http.StatusOK {
		t.Fatalf("use policy = %d %s", w.Code, w.Body)
	}
	e.orgDefaults(fmt.Sprintf(`{"pool_id":%q}`, open.ID))

	for _, door := range placementDoors[1:] {
		w := e.call(e.person, http.MethodPost, door, runBody(""))
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", door, w.Code, w.Body)
		}
		var body struct {
			RunnerPool  *types.ResolvedRunnerPool `json:"runner_pool"`
			RunnerPools []struct {
				ID                      uuid.UUID
				Name, Availability      string
				Candidates, Target, Run any
			} `json:"runner_pools"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.RunnerPool == nil || body.RunnerPool.ID != open.ID || body.RunnerPool.Selection != types.RunnerPoolSelectedOrg {
			t.Errorf("%s resolved %+v", door, body.RunnerPool)
		}
		if len(body.RunnerPools) != 1 || body.RunnerPools[0].ID != open.ID || body.RunnerPools[0].Availability != "available" {
			t.Errorf("%s lists %+v, want only the open pool", door, body.RunnerPools)
		}
		for _, leak := range []string{"Finance", secret.ID.String(), `"alpha"`, `"beta"`, "runner_id", "executor"} {
			if strings.Contains(w.Body.String(), leak) {
				t.Errorf("%s leaks %q: %s", door, leak, w.Body)
			}
		}
	}
	// The inaccessible pool is the unknown pool, byte for byte, at every door.
	for _, door := range placementDoors {
		a := e.call(e.person, http.MethodPost, door, runBody(`"runner_pool_id":"`+secret.ID.String()+`"`))
		b := e.call(e.person, http.MethodPost, door, runBody(`"runner_pool_id":"`+uuid.NewString()+`"`))
		if a.Code != http.StatusNotFound || a.Code != b.Code || a.Body.String() != b.Body.String() {
			t.Errorf("%s: inaccessible = %d %s, unknown = %d %s", door, a.Code, a.Body, b.Code, b.Body)
		}
	}
	// A default naming it is the same as a default naming a deleted pool.
	e.personDefaults(e.person, fmt.Sprintf(`{"pool_id":%q}`, open.ID))
	if _, err := e.pool.Exec(context.Background(), `UPDATE principal_prefs SET value = jsonb_build_object('pool_id', $2::text) WHERE principal = $1 AND key = $3`,
		e.personP, secret.ID.String(), types.RunnerPoolDefaultPrefKey); err != nil {
		t.Fatal(err)
	}
	inaccessible := e.answers(e.person, runBody(""))
	sameEverywhere(t, "a default naming a pool the caller may not use", inaccessible, http.StatusUnprocessableEntity, string(runnerpool.ReasonDefaultUnavailable))
	if _, err := e.pool.Exec(context.Background(), `UPDATE principal_prefs SET value = jsonb_build_object('pool_id', $2::text) WHERE principal = $1 AND key = $3`,
		e.personP, uuid.NewString(), types.RunnerPoolDefaultPrefKey); err != nil {
		t.Fatal(err)
	}
	deleted := e.answers(e.person, runBody(""))
	for _, door := range placementDoors {
		if inaccessible[door].Body.String() != deleted[door].Body.String() {
			t.Errorf("%s: a default naming an inaccessible pool answered %s, one naming a missing pool %s", door, inaccessible[door].Body, deleted[door].Body)
		}
	}
}

// A heterogeneous remote pool through the route: a CC3 run can only land on the executor that enforces
// it, whatever the other member can do, and a class no member enforces refuses.
func TestPG_HeterogeneousRemotePoolNeverWidens(t *testing.T) {
	e := newPlacementEnv(t)
	pool := e.createPool("Mixed", types.RunnerPoolOrganization)
	e.addExecutors(pool.ID, "alpha", "beta")
	e.runner.execFacts["beta"] = runner.TargetFacts{Classes: []types.ConfinementClass{types.CC1, types.CC2}}
	body := func(cc string) string {
		return runBody(`"confinement_class":"` + cc + `","runner_pool_id":"` + pool.ID.String() + `"`)
	}
	seen := map[string]bool{}
	for range 12 {
		w := e.call(e.person, http.MethodPost, "/api/v1/runs", body("CC3"))
		if w.Code != http.StatusCreated {
			t.Fatalf("CC3 = %d %s", w.Code, w.Body)
		}
		var run types.AgentRun
		_ = json.Unmarshal(w.Body.Bytes(), &run)
		stored, _ := e.pg.GetRun(context.Background(), run.ID)
		seen[stored.RunnerPoolExecutor] = true
	}
	if len(seen) != 1 || !seen["alpha"] {
		t.Fatalf("CC3 runs landed on %v, only alpha enforces CC3", seen)
	}
	e.runner.execFacts["alpha"] = runner.TargetFacts{Classes: []types.ConfinementClass{types.CC1, types.CC2}}
	sameEverywhere(t, "CC3 with no member that enforces it", map[string]*httptest.ResponseRecorder{
		placementDoors[0]: e.call(e.person, http.MethodPost, placementDoors[0], body("CC3")),
		placementDoors[1]: e.call(e.person, http.MethodPost, placementDoors[1], body("CC3")),
		placementDoors[2]: e.call(e.person, http.MethodPost, placementDoors[2], body("CC3")),
	}, http.StatusUnprocessableEntity, string(placement.ReasonPlacementCapability))
}

// A member that is unknown or unreachable refuses; with one reachable executor the run goes there.
func TestPG_UnknownExecutorFactsRefuseNeverRouteElsewhere(t *testing.T) {
	e := newPlacementEnv(t)
	pool := e.createPool("Flaky", types.RunnerPoolOrganization)
	e.addExecutors(pool.ID, "alpha", "beta")
	delete(e.runner.execFacts, "beta")
	e.runner.silent = map[string]bool{"beta": true} // beta is run by the deployment but cannot report
	for range 6 {
		w := e.call(e.person, http.MethodPost, "/api/v1/runs", runBody(`"runner_pool_id":"`+pool.ID.String()+`"`))
		if w.Code != http.StatusCreated {
			t.Fatalf("create = %d %s", w.Code, w.Body)
		}
		var run types.AgentRun
		_ = json.Unmarshal(w.Body.Bytes(), &run)
		if stored, _ := e.pg.GetRun(context.Background(), run.ID); stored.RunnerPoolExecutor != "alpha" {
			t.Fatalf("run bound to %q while beta is unknown", stored.RunnerPoolExecutor)
		}
	}
	e.runner.silent["alpha"] = true
	delete(e.runner.execFacts, "alpha")
	w := e.call(e.person, http.MethodPost, "/api/v1/runs", runBody(`"runner_pool_id":"`+pool.ID.String()+`"`))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no executor can report: %d %s", w.Code, w.Body)
	}
}

// B1: a run that names no class takes its default barrier from the pool's own eligible candidates, at
// create, Review and the preview alike, and launches on a candidate that enforces it. Executor alpha
// enforces CC1-CC3 and beta CC1-CC2; a pool of beta only must never promise or choose CC3.
func TestPG_DefaultClassComesFromThePoolsCandidatesAtEveryDoor(t *testing.T) {
	e := newPlacementEnv(t)
	betaOnly := e.createPool("Beta only", types.RunnerPoolOrganization)
	e.addExecutors(betaOnly.ID, "beta")
	both := e.createPool("Both", types.RunnerPoolOrganization)
	e.addExecutors(both.ID, "alpha", "beta")
	e.runner.execFacts["beta"] = runner.TargetFacts{Classes: []types.ConfinementClass{types.CC1, types.CC2}}

	for _, tc := range []struct {
		name     string
		pool     uuid.UUID
		want     types.ConfinementClass
		executor string
	}{
		{"a pool without the deployment's strongest barrier", betaOnly.ID, types.CC2, "beta"},
		{"a pool that has it", both.ID, types.CC3, "alpha"},
	} {
		body := runBody(`"runner_pool_id":"` + tc.pool.String() + `"`)
		pre := e.call(e.person, http.MethodPost, "/api/v1/runs/preflight", body)
		prev := e.call(e.person, http.MethodPost, "/api/v1/runs/policy-preview", body)
		if pre.Code != http.StatusOK || prev.Code != http.StatusOK {
			t.Fatalf("%s: preflight %d %s, preview %d %s", tc.name, pre.Code, pre.Body, prev.Code, prev.Body)
		}
		var promised struct {
			Enforced types.ConfinementClass `json:"enforced_confinement_class"`
		}
		_ = json.Unmarshal(pre.Body.Bytes(), &promised)
		w := e.call(e.person, http.MethodPost, "/api/v1/runs", body)
		if w.Code != http.StatusCreated {
			t.Fatalf("%s: create = %d %s (the doors disagree)", tc.name, w.Code, w.Body)
		}
		var created types.AgentRun
		_ = json.Unmarshal(w.Body.Bytes(), &created)
		stored, err := e.pg.GetRun(context.Background(), created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if promised.Enforced != tc.want || stored.ConfinementClass != tc.want || stored.RunnerPoolExecutor != tc.executor {
			t.Errorf("%s: preflight promised %q, create enforced %q on %q; want %q on %q", tc.name, promised.Enforced, stored.ConfinementClass, stored.RunnerPoolExecutor, tc.want, tc.executor)
		}
	}
}

// N1: pinning one of your own runners that is offline answers runner_offline at every door, even when
// another of your runners in the pool is online; a foreign or unknown pin keeps the identical mismatch body.
func TestPG_OwnOfflinePinAnswersRunnerOffline(t *testing.T) {
	e := newPlacementEnv(t)
	pool := e.createPool("Laptops", types.RunnerPoolUser)
	asleep := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	awake := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	theirs := e.laptop(e.otherP, 4000, 8192, types.CC1, types.CC2)
	e.addToPool(e.person, pool.ID, asleep)
	e.addToPool(e.person, pool.ID, awake)
	e.addToPool(e.other, e.createOwnPool(e.other, "Theirs").ID, theirs)
	e.online[asleep] = false
	pin := func(id uuid.UUID) string {
		return runBody(`"placement":"local","runner_id":"` + id.String() + `","runner_pool_id":"` + pool.ID.String() + `"`)
	}
	sameEverywhere(t, "an offline pin of their own", e.answers(e.person, pin(asleep)), http.StatusUnprocessableEntity, string(placement.ReasonRunnerOffline))
	foreign := sameEverywhere(t, "a foreign pin", e.answers(e.person, pin(theirs)), http.StatusUnprocessableEntity, string(runnerpool.ReasonMemberMismatch))
	unknownID := uuid.New()
	unknown := sameEverywhere(t, "an unknown pin", e.answers(e.person, pin(unknownID)), http.StatusUnprocessableEntity, string(runnerpool.ReasonMemberMismatch))
	if strings.ReplaceAll(foreign, theirs.String(), "X") != strings.ReplaceAll(unknown, unknownID.String(), "X") {
		t.Errorf("a foreign pin answered %s, an unknown one %s", foreign, unknown)
	}
}

// R2-B1: a pool that sets limits allows only its own barriers, so the default class of a run that names
// none is the strongest one that is both enforced by a candidate and allowed by the pool, and the three
// doors reach the same one (or the same refusal).
func TestPG_DefaultClassHonoursThePoolsAllowedBarriersAtEveryDoor(t *testing.T) {
	e := newPlacementEnv(t)
	const background = `"title":"t","experience":"background","workload":{"kind":"agent_task","agent":"claude-code","task":"fix the build"},"tools":[{"id":"claude-code","kind":"harness"}],"no_repositories_or_drives":true`
	for _, tc := range []struct {
		name     string
		barriers []types.ConfinementClass
		want     types.ConfinementClass // empty: every door refuses the same way
	}{
		{"no Vault", []types.ConfinementClass{types.CC1, types.CC2}, types.CC2},
		{"Wall only", []types.ConfinementClass{types.CC2}, types.CC2},
		{"Vault only", []types.ConfinementClass{types.CC3}, types.CC3},
		{"nothing that meets the floor", []types.ConfinementClass{types.CC1}, ""},
	} {
		pool := e.limitedPool(tc.name, tc.barriers...)
		body := `{` + background + `,"runner_pool_id":"` + pool.ID.String() + `"}`
		got := e.answers(e.person, body)
		if tc.want == "" {
			sameEverywhere(t, tc.name, got, http.StatusUnprocessableEntity, string(runnerpool.ReasonBarrierNotAllowed))
			continue
		}
		for door, w := range got {
			if w.Code >= 300 {
				t.Errorf("%s: %s = %d %s", tc.name, door, w.Code, w.Body)
			}
		}
		var promised struct {
			Enforced types.ConfinementClass `json:"enforced_confinement_class"`
		}
		_ = json.Unmarshal(got[placementDoors[1]].Body.Bytes(), &promised)
		var created types.AgentRun
		_ = json.Unmarshal(got[placementDoors[0]].Body.Bytes(), &created)
		if stored, err := e.pg.GetRun(context.Background(), created.ID); err != nil || promised.Enforced != tc.want || stored.ConfinementClass != tc.want {
			t.Errorf("%s: preflight promised %q, create enforced %q (%v), want %q", tc.name, promised.Enforced, stored.ConfinementClass, err, tc.want)
		}
	}
}
