// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"filippo.io/age"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// specFor waits for the sandbox spec dispatch handed the runner for run id.
func (e *queueEnv) specFor(id uuid.UUID) runner.SandboxSpec {
	e.t.Helper()
	var spec runner.SandboxSpec
	e.eventually("run "+id.String()+" to be dispatched", func() bool {
		e.runner.fakeRunner.mu.Lock()
		defer e.runner.fakeRunner.mu.Unlock()
		spec = e.runner.fakeRunner.lastSpec
		return spec.RunID == id
	})
	return spec
}

// A run that waited in the pool's queue starts with what the same launch gets when it starts at once:
// its start folder, its harness set and its main drive reach dispatch, and its harness set is recorded.
func TestPG_QueuedRunLaunchesWhatAnImmediateStartLaunches(t *testing.T) {
	e := newQueueEnv(t)
	e.setDiskWord(types.StorageEnforcementFilesystem)
	pool := e.cappedPool("Shared", 1)
	extra := `,"start_folder":{"kind":"image_default"},"resources":{"disk_mib":2048}`
	// A capped pool queues every launch, so the pool is uncapped for the launch that starts at once.
	capTo := func(n string) {
		e.exec(`UPDATE runner_pools SET limits = jsonb_set(limits, '{max_concurrent_runs}', $2::jsonb), revision = revision + 1 WHERE id = $1`, pool.ID, n)
	}
	e.exec(`UPDATE runner_pools SET limits = limits - 'max_concurrent_runs', revision = revision + 1 WHERE id = $1`, pool.ID)

	at, w := e.launch(nil, e.reread(pool), extra)
	if w.Code != 201 || at.State == types.RunQueued {
		t.Fatalf("the first launch = %d %s, want it started at once", w.Code, w.Body)
	}
	now := e.specFor(at.ID)
	if starts := e.audits(at.ID, "run.queue.start"); len(starts) != 0 {
		t.Fatalf("the first launch went through the queue (%d run.queue.start rows), want it started at once", len(starts))
	}
	e.exec(`UPDATE agent_runs SET state = 'COMPLETED', ended_at = now() WHERE id = $1`, at.ID)

	capTo("1")
	occupant := e.occupy(pool)
	queued := e.queuedLaunch(nil, e.reread(pool), extra)
	e.free(occupant)
	e.sweep()
	e.waitStarted(queued.ID)
	later := e.specFor(queued.ID)
	if starts := e.audits(queued.ID, "run.queue.start"); len(starts) == 0 {
		t.Fatal("the second launch did not start through the queue")
	}

	for _, k := range []string{"WARDYN_START_FOLDER", "WARDYN_HARNESSES", "WARDYN_START_HARNESS"} {
		if now.Env[k] == "" || later.Env[k] != now.Env[k] {
			t.Errorf("%s: at once %q, after the queue %q; want the same, and set", k, now.Env[k], later.Env[k])
		}
	}
	if now.Resources.DiskMiB != 2048 || later.Resources.DiskMiB != now.Resources.DiskMiB {
		t.Errorf("main drive: at once %d MiB, after the queue %d MiB; want 2048 both times", now.Resources.DiskMiB, later.Resources.DiskMiB)
	}
	for _, id := range []uuid.UUID{at.ID, queued.ID} {
		hs, err := e.pg.ListRunHarnesses(context.Background(), id)
		if err != nil || len(hs) != 1 || hs[0].Harness != "claude-code" {
			t.Errorf("run %s recorded harnesses %+v (%v), want claude-code", id, hs, err)
		}
	}
}

// A launch into a full pool whose harness set cannot be recorded does not wait in the queue: it is
// refused, and its run is FAILED and out of the queue, never started without its set.
func TestPG_QueuedRunIsAbortedWhenItsHarnessesCannotBeRecorded(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Shared", 1)
	e.occupy(pool)
	e.exec(`ALTER TABLE run_harnesses ADD CONSTRAINT refuse_all CHECK (false) NOT VALID`)
	if _, w := e.launch(nil, e.reread(pool), ""); w.Code != 500 {
		t.Fatalf("a launch whose harnesses cannot be recorded = %d %s, want 500", w.Code, w.Body)
	}
	var id uuid.UUID
	var state string
	if err := e.pool.QueryRow(context.Background(), `SELECT id, state FROM agent_runs WHERE failure_hint = $1`, createRunHarnessesAbortHint).Scan(&id, &state); err != nil {
		t.Fatalf("no run carries the abort hint: %v", err)
	}
	if state != string(types.RunFailed) || e.queueRows(id) != 0 {
		t.Errorf("the aborted run is %s with %d queue rows, want FAILED and none", state, e.queueRows(id))
	}
}

// twoProviderLanes gives the organisation two key providers, Anthropic for claude-code and OpenAI for
// codex-cli, with owner's own key for each, and returns them.
func (e *queueEnv) twoProviderLanes(owner string) (a, b types.ModelProvider) {
	e.t.Helper()
	ctx := context.Background()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		e.t.Fatal(err)
	}
	sec, err := secretspg.New(e.pool, id)
	if err != nil {
		e.t.Fatal(err)
	}
	e.srv.cfg.Secrets = sec
	a = mpKeyProvider("anthropic", uuid.NewString(), types.ModelProviderAnthropicAPIKey, types.ProviderHarness{Harness: "claude-code"})
	b = mpKeyProvider("openai", uuid.NewString(), types.ModelProviderOpenAIAPIKey, types.ProviderHarness{Harness: "codex-cli"})
	site, err := e.pg.GetSiteConfig(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	site.ModelProviders = providerBlock(a, b)
	if _, err := e.pg.PutSiteConfig(ctx, site); err != nil {
		e.t.Fatal(err)
	}
	for _, p := range []types.ModelProvider{a, b} {
		name := providerSecretName(p.UID, providerKeyPart)
		if err := sec.Put(ctx, name, []byte("sk-operator-"+p.ID)); err != nil {
			e.t.Fatal(err)
		}
		if err := sec.For(owner).Put(ctx, name, []byte("sk-own-"+p.ID)); err != nil {
			e.t.Fatal(err)
		}
	}
	return a, b
}

// launchedLanes is what one run's launch left for its two provider lanes: each harness's authored grant
// as "host=secret name", by harness, and the same pairs as dispatch handed them to the proxy.
func (e *queueEnv) launchedLanes(id uuid.UUID, spec runner.SandboxSpec) (granted map[string]string, injected []string) {
	e.t.Helper()
	grants, err := e.pg.ListGrantsByRun(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	granted = map[string]string{}
	for _, g := range grants {
		var scope struct {
			Host       string                `json:"host"`
			SecretName string                `json:"secret_name"`
			Snapshot   providerGrantSnapshot `json:"snapshot"`
		}
		if g.Spec.Kind == types.GrantAPIKey && json.Unmarshal(g.Spec.Scope, &scope) == nil && scope.Snapshot.Harness != "" {
			granted[scope.Snapshot.Harness] = scope.Host + "=" + scope.SecretName
		}
	}
	for _, ig := range spec.ProxyConfig.Injection {
		injected = append(injected, ig.Rule.Host+"="+ig.Rule.SecretName)
	}
	slices.Sort(injected)
	return granted, injected
}

// The same parity for a run with two harnesses on two model providers, a main drive and a start folder:
// after the queue each harness has its own provider lane, as it has at once (its authored grant and its
// injection at the proxy), the same harness rows are recorded, both start from the image declared for
// the two harnesses, and the disk the promotion reserved is the disk dispatched.
func TestPG_QueuedTwoHarnessRunLaunchesWhatAnImmediateStartLaunches(t *testing.T) {
	e := newQueueEnv(t)
	e.setDiskWord(types.StorageEnforcementFilesystem)
	// The admin token is no person and holds no model credential, so the person launches.
	a, b := e.twoProviderLanes(e.personP)
	e.srv.cfg.AgentImages = map[string]string{"claude-code": "wardyn/agent-claude-code:local", "claude-code+codex-cli": setImage}
	// Only an Interactive run carries two harnesses.
	pool := e.bothTypesPool("Shared", 1)
	body := func() string {
		return `{"title":"t","experience":"interactive","startup":{"kind":"harness","tool":"claude-code","prompt":"fix the build"},` +
			`"tools":[{"id":"claude-code","kind":"harness","model_provider":"` + a.ID + `"},{"id":"codex-cli","kind":"harness","model_provider":"` + b.ID + `"}],` +
			`"no_repositories_or_drives":true,"runner_pool_id":"` + pool.ID.String() + `","start_folder":{"kind":"image_default"},"resources":{"disk_mib":2048}}`
	}
	launch := func() queuedLaunchAnswer {
		t.Helper()
		w := e.call(e.person, http.MethodPost, "/api/v1/runs", body())
		var out queuedLaunchAnswer
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusCreated {
			t.Fatalf("launch = %d %s (%v)", w.Code, w.Body, err)
		}
		return out
	}
	// A capped pool queues every launch, so the pool is uncapped for the launch that starts at once.
	e.exec(`UPDATE runner_pools SET limits = limits - 'max_concurrent_runs', revision = revision + 1 WHERE id = $1`, pool.ID)
	at := launch()
	if at.State == types.RunQueued {
		t.Fatal("the first launch waited, want it started at once")
	}
	now := e.specFor(at.ID)
	e.exec(`UPDATE agent_runs SET state = 'COMPLETED', ended_at = now() WHERE id = $1`, at.ID)

	e.exec(`UPDATE runner_pools SET limits = jsonb_set(limits, '{max_concurrent_runs}', '1'::jsonb), revision = revision + 1 WHERE id = $1`, pool.ID)
	occupant := e.occupy(e.reread(pool))
	queued := launch()
	if queued.State != types.RunQueued {
		t.Fatalf("the second launch is %s, want QUEUED", queued.State)
	}
	e.free(occupant)
	e.sweep()
	e.waitStarted(queued.ID)
	later := e.specFor(queued.ID)

	for _, k := range []string{"WARDYN_START_FOLDER", "WARDYN_HARNESSES", "WARDYN_START_HARNESS"} {
		if now.Env[k] == "" || later.Env[k] != now.Env[k] {
			t.Errorf("%s: at once %q, after the queue %q; want the same, and set", k, now.Env[k], later.Env[k])
		}
	}
	if now.Image != setImage || later.Image != now.Image {
		t.Errorf("image: at once %q, after the queue %q; want the two-harness image %q for both", now.Image, later.Image, setImage)
	}
	wantRows := []types.RunHarness{{Harness: "claude-code", ModelProviderID: a.ID, Starts: true}, {Harness: "codex-cli", ModelProviderID: b.ID}}
	wantLanes := map[string]string{
		"claude-code": "api.anthropic.com=" + providerSecretName(a.UID, providerKeyPart),
		"codex-cli":   "api.openai.com=" + providerSecretName(b.UID, providerKeyPart),
	}
	for _, run := range []struct {
		when string
		id   uuid.UUID
		spec runner.SandboxSpec
	}{{"at once", at.ID, now}, {"after the queue", queued.ID, later}} {
		rows, err := e.pg.ListRunHarnesses(context.Background(), run.id)
		if err != nil || !slices.Equal(rows, wantRows) {
			t.Errorf("%s: recorded harnesses %+v (%v), want %+v", run.when, rows, err, wantRows)
		}
		granted, injected := e.launchedLanes(run.id, run.spec)
		for harness, lane := range wantLanes {
			if granted[harness] != lane || !slices.Contains(injected, lane) {
				t.Errorf("%s: the %s lane's grant is %q and the proxy's injections are %v, want %q in both", run.when, harness, granted[harness], injected, lane)
			}
		}
		if reserved := e.reservedOn(run.id).DiskMiB; reserved != 2048 || run.spec.Resources.DiskMiB != reserved {
			t.Errorf("%s: %d MiB of disk reserved and %d MiB dispatched, want 2048 for both", run.when, reserved, run.spec.Resources.DiskMiB)
		}
	}
}
