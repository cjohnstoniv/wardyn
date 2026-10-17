// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The trust class through placement against a real Postgres (K1 §2, §8 trust matrix and pool rows): create
// judges a run at its eligible runners' trust classes, highest first, records the first that admits it and
// binds the run to a runner of that class; dispatch and revive read the bound runner's stored class; an
// organization pool offers the organization's own runners to the people its use policy admits.
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (e *placementEnv) setTrust(id uuid.UUID, c types.RunnerTrustClass) {
	e.t.Helper()
	if _, _, err := e.pg.SetRunnerTrustClass(context.Background(), id, c, "sec-test", time.Now()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *placementEnv) runTrust(id uuid.UUID) types.RunnerTrustClass {
	e.t.Helper()
	c, err := e.pg.RunRunnerTrustClass(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *placementEnv) deliveryPolicy(raw string) {
	e.t.Helper()
	if _, err := e.pg.PutCredentialDeliveryPolicy(context.Background(), json.RawMessage(raw), "seed", time.Now()); err != nil {
		e.t.Fatal(err)
	}
}

// orgRunner is a claimed organization runner bound to this organisation, online, reporting every class.
func (e *placementEnv) orgRunner() uuid.UUID {
	e.t.Helper()
	ctx := context.Background()
	id := uuid.New()
	fp, org := "fp-"+id.String(), federation.OrgURLSHA256(poolTestOrgURL)
	if _, err := e.pg.CreateRunner(ctx, types.Runner{ID: id, OwnerKind: types.RunnerOwnerOrg, Name: "cluster", PublicKey: append(id[:], id[:]...),
		KeyFingerprint: fp, OrgURLSHA256: org, Limits: roomyRunnerLimits()}); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pg.ClaimOrgRunner(ctx, id, fp, org, "sec-test", time.Now()); err != nil {
		e.t.Fatal(err)
	}
	e.online[id] = true
	e.runner.runnerFacts[id.String()] = runner.TargetFacts{Classes: []types.ConfinementClass{types.CC1, types.CC2, types.CC3}, CapacityAdvertised: true, CPUMillisMax: 4000, MemoryMiBMax: 8192}
	if _, err := e.pool.Exec(ctx, `UPDATE runners SET run_cpu_millis = 4000, run_memory_mib = 8192, max_runs = $2 WHERE id = $1`, id, types.RunnerMaxRunsCeiling); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func orgRunnerPath(pool, runnerID uuid.UUID) string {
	return "/api/v1/runner-pools/" + pool.String() + "/runners/" + runnerID.String()
}

// §8 trust matrix, create: with the legacy document (no by_trust) a run is judged at its runners' highest
// class and bound to a runner of it, which is recorded; when that runner is downgraded the next run lands
// at the class left.
func TestPG_RunTrust_CreateBindsTheHighestAdmittingClass(t *testing.T) {
	e := newPlacementEnv(t)
	pool := e.createOwnPool(e.person, "Laptops")
	personal := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
	managed := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
	for _, id := range []uuid.UUID{personal, managed} {
		e.maxRuns(id, e.personP, types.RunnerMaxRunsCeiling)
		e.addToPool(e.person, pool.ID, id)
	}
	e.setTrust(managed, types.RunnerTrustManagedDevice)
	body := runBody(`"placement":"local","runner_pool_id":"` + pool.ID.String() + `"`)
	for i := 0; i < 8; i++ {
		run, code, refused := e.launchStored(body)
		if code != http.StatusCreated {
			t.Fatalf("launch = %d %s", code, refused)
		}
		if run.RunnerID == nil || *run.RunnerID != managed || e.runTrust(run.ID) != types.RunnerTrustManagedDevice {
			t.Errorf("launch %d bound to %v at %q, want the managed_device runner recorded managed_device", i, run.RunnerID, e.runTrust(run.ID))
		}
	}
	e.setTrust(managed, types.RunnerTrustPersonal)
	run, code, refused := e.launchStored(body)
	if code != http.StatusCreated || e.runTrust(run.ID) != types.RunnerTrustPersonal {
		t.Fatalf("after the downgrade = %d %s, recorded %q, want personal", code, refused, e.runTrust(run.ID))
	}
}

// §8 trust matrix: persisted modes, a downgrade before dispatch and before revive, and unknown trust. A
// via_org grant is persisted via_org at the admitted class; dispatch admits a credential only the
// managed_device override delivers until the runner is downgraded, then refuses it naming personal; a
// revive refuses runner_trust_changed; a policy that no longer delivers the persisted mode refuses at
// dispatch; a run with no recorded class is refused at revive.
func TestPG_RunTrust_PersistedModesAndDowngrades(t *testing.T) {
	e := newPlacementEnv(t)
	pool := e.createOwnPool(e.person, "Laptop")
	laptop := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
	e.maxRuns(laptop, e.personP, types.RunnerMaxRunsCeiling)
	e.addToPool(e.person, pool.ID, laptop)
	e.setTrust(laptop, types.RunnerTrustManagedDevice)
	e.deliveryPolicy(`{"classes":{"api_key":{"mode":"via_org"}},"by_trust":{"managed_device":{"classes":{"env_secret":{"mode":"runner_resident"}}}}}`)

	run, code, refused := e.launchStored(runBody(`"placement":"local","runner_pool_id":"` + pool.ID.String() + `"`))
	if code != http.StatusCreated {
		t.Fatalf("launch = %d %s", code, refused)
	}
	if e.runTrust(run.ID) != types.RunnerTrustManagedDevice {
		t.Fatalf("recorded %q, want managed_device", e.runTrust(run.ID))
	}
	// The org-held api_key grant as create persists it at this policy: recorded via_org.
	ctx := context.Background()
	key := types.CredentialGrant{ID: uuid.New(), RunID: run.ID, CreatedAt: time.Now().UTC(), Delivery: types.GrantDeliveryViaOrg,
		Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"corp.example","secret_name":"corp-key"}`)}}
	if _, err := e.srv.cfg.Store.CreateGrant(ctx, key); err != nil {
		t.Fatal(err)
	}
	env := types.CredentialGrant{ID: uuid.New(), RunID: run.ID, CreatedAt: time.Now().UTC(), Spec: types.GrantSpec{Kind: types.GrantEnvSecret, Scope: json.RawMessage(`{"name":"TOKEN","secret_name":"ci-token"}`)}}
	if _, err := e.srv.cfg.Store.CreateGrant(ctx, env); err != nil {
		t.Fatal(err)
	}
	dispatch := func() *placement.Refusal {
		t.Helper()
		spec := runner.SandboxSpec{RunID: run.ID, SecretEnv: map[string]string{"TOKEN": placement.ResidentPlaceholder(env.ID)}}
		p, err := e.srv.localResolvedPlan(ctx, run, false, types.SiteConfig{}, spec, nil, llmTransport{})
		if err != nil {
			t.Fatal(err)
		}
		if r, err := e.srv.dispatchTrustRefusal(ctx, run, p); err != nil || r != nil {
			if err != nil {
				t.Fatal(err)
			}
			return r
		}
		_, r := placement.LocalEligibility(p)
		return r
	}
	revive := func() *placement.Refusal {
		t.Helper()
		cfg := proxy.Config{RunID: run.ID}
		err := e.srv.refreshLocalDeploymentConfig(ctx, run, types.SiteConfig{}, &cfg)
		r, _ := err.(*placement.Refusal)
		if err != nil && r == nil {
			t.Fatalf("revive: %v", err)
		}
		return r
	}
	if r := dispatch(); r != nil {
		t.Fatalf("dispatch at managed_device: %v", r)
	}
	if r := revive(); r != nil {
		t.Fatalf("revive at managed_device: %v", r)
	}

	e.setTrust(laptop, types.RunnerTrustPersonal)
	if r := dispatch(); r == nil || r.Reason != placement.ReasonPlacementCredential || !strings.Contains(r.Detail, "trust class personal") {
		t.Errorf("dispatch after the downgrade: %+v, want placement_credential naming personal", r)
	}
	if r := revive(); r == nil || r.Reason != placement.ReasonRunnerTrustChanged {
		t.Errorf("revive after the downgrade: %+v, want runner_trust_changed", r)
	}

	e.setTrust(laptop, types.RunnerTrustManagedDevice)
	e.deliveryPolicy(`{"classes":{"api_key":{"mode":"refuse"}},"by_trust":{"managed_device":{"classes":{"env_secret":{"mode":"runner_resident"}}}}}`)
	if r := dispatch(); r == nil || r.Reason != placement.ReasonPlacementCredential || r.Field != "eligible_grants" {
		t.Errorf("dispatch once via_org is no longer the persisted grant's mode: %+v, want the persisted-mode refusal", r)
	}

	if _, err := e.pool.Exec(ctx, `UPDATE agent_runs SET runner_trust_class = NULL WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if r := revive(); r == nil || r.Reason != placement.ReasonRunnerTrustChanged {
		t.Errorf("revive with no recorded class: %+v, want runner_trust_changed", r)
	}
}

// §8 pool rows: an organization pool takes the organization's own runners and no person's, an org runner
// never joins a user pool or a second pool, the pool offers it only to the people its use policy admits,
// and a run on it is recorded org_cluster.
func TestPG_OrgPool_OrgRunnersPlacementAndUsePolicy(t *testing.T) {
	e := newPlacementEnv(t)
	org := e.createPool("Cluster", types.RunnerPoolOrganization)
	cluster := e.orgRunner()
	mine := e.claimedRunner(e.personP)

	if w := e.admin(http.MethodPut, orgRunnerPath(org.ID, cluster), ""); w.Code != http.StatusCreated {
		t.Fatalf("add the org runner = %d %s", w.Code, w.Body)
	}
	if w := e.admin(http.MethodPut, orgRunnerPath(org.ID, cluster), ""); w.Code != http.StatusOK {
		t.Errorf("add it again = %d %s, want 200", w.Code, w.Body)
	}
	if w := e.admin(http.MethodPut, orgRunnerPath(org.ID, mine), ""); w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != string(runnerpool.ReasonMemberMismatch) {
		t.Errorf("a person's runner into the organization pool = %d %s, want member mismatch", w.Code, w.Body)
	}
	if w := e.call(e.person, http.MethodPut, orgRunnerPath(org.ID, cluster), ""); w.Code != http.StatusForbidden {
		t.Errorf("a member's org runner add = %d %s, want 403", w.Code, w.Body)
	}
	other := e.createPool("Second", types.RunnerPoolOrganization)
	if w := e.admin(http.MethodPut, orgRunnerPath(other.ID, cluster), ""); w.Code != http.StatusUnprocessableEntity || wireError(t, w) != orgRunnerInOtherPoolMsg {
		t.Errorf("the org runner into a second pool = %d %s, want the one-pool refusal", w.Code, w.Body)
	}
	own := e.createOwnPool(e.person, "Mine")
	if w := e.call(e.person, http.MethodPut, addRunnerPath(own.ID, cluster), ""); w.Code < 400 {
		t.Errorf("the org runner into a user pool = %d %s, want refused", w.Code, w.Body)
	}
	if n := e.count(`SELECT count(*) FROM runner_pool_members WHERE runner_id = $1`, cluster); n != 1 {
		t.Fatalf("the org runner is in %d pools, want 1", n)
	}

	body := runBody(`"placement":"local","runner_pool_id":"` + org.ID.String() + `"`)
	run, code, refused := e.launchStored(body)
	if code != http.StatusCreated || run.RunnerID == nil || *run.RunnerID != cluster || run.Placement != types.PlacementLocal {
		t.Fatalf("launch on the organization pool = %d %s, runner %v placement %q", code, refused, run.RunnerID, run.Placement)
	}
	if got := e.runTrust(run.ID); got != types.RunnerTrustOrgCluster {
		t.Errorf("recorded %q, want org_cluster", got)
	}
	if err := e.srv.callerOwnsRunner(withOIDCHuman(context.Background(), e.personP), cluster); err != nil {
		t.Errorf("the person may use the org runner through the pool: %v", err)
	}

	if w := e.call(e.alice, http.MethodPut, "/api/v1/runner-pools/"+org.ID.String()+"/use-policy",
		`{"subjects":[{"subject_type":"user","subject":"`+e.otherP+`"}]}`); w.Code != http.StatusOK {
		t.Fatalf("use policy = %d %s", w.Code, w.Body)
	}
	before := e.count(`SELECT count(*) FROM agent_runs`)
	if w := e.call(e.person, http.MethodPost, "/api/v1/runs", body); w.Code == http.StatusCreated {
		t.Errorf("a person the use policy leaves out launched on the org runner: %s", w.Body)
	}
	if e.count(`SELECT count(*) FROM agent_runs`) != before {
		t.Error("the refused launch wrote a run")
	}
	if err := e.srv.callerOwnsRunner(withOIDCHuman(context.Background(), e.personP), cluster); err == nil || asRefusal(t, err).Reason != placement.ReasonRunnerNotFound {
		t.Errorf("the org runner outside the person's permitted pools: %v, want runner_not_found", err)
	}

	if w := e.admin(http.MethodDelete, orgRunnerPath(org.ID, cluster), ""); w.Code != http.StatusNoContent {
		t.Fatalf("remove = %d %s", w.Code, w.Body)
	}
	if n := e.count(`SELECT count(*) FROM runner_pool_members WHERE runner_id = $1`, cluster); n != 0 {
		t.Errorf("the removed org runner still has %d memberships", n)
	}
}

// §8 pool rows: revoking an organization runner ends the run waiting in its pool's queue for it.
func TestPG_OrgPool_RevokeEndsTheRunWaitingForTheOrgRunner(t *testing.T) {
	e := newQueueEnv(t)
	body := `{"name":"Cluster","limits":{"background":{"cpu_millis":{"default":500,"cap":4000},"memory_mib":{"default":512,"cap":8192},` +
		`"lifetime":{"default_sec":3600,"max_sec":7200}},"barriers":["CC1","CC2","CC3"],"max_concurrent_runs":5}}`
	w := e.admin(http.MethodPost, "/api/v1/runner-pools", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create pool = %d %s", w.Code, w.Body)
	}
	pool := decodeJSON[types.RunnerPool](t, w)
	cluster := e.orgRunner()
	if w := e.admin(http.MethodPut, orgRunnerPath(pool.ID, cluster), ""); w.Code != http.StatusCreated {
		t.Fatalf("add the org runner = %d %s", w.Code, w.Body)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE runners SET max_runs = 1 WHERE id = $1`, cluster); err != nil {
		t.Fatal(err)
	}
	pool = e.reread(pool)
	occupyRunner(e, pool, cluster)
	waiting := e.queuedLaunch(e.person, pool, `,"placement":"local","runner_id":"`+cluster.String()+`"`)
	if w := e.call(e.alice, http.MethodDelete, "/api/v1/runners/"+cluster.String(), ""); w.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", w.Code, w.Body)
	}
	if got := e.run(waiting.ID); got.State != types.RunKilled {
		t.Fatalf("the run waiting for the revoked org runner is %s, want KILLED", got.State)
	}
	if n := e.queueRows(waiting.ID); n != 0 {
		t.Errorf("the ended run still has %d queue rows", n)
	}
}

// Fable K4 H1: a capped user pool of two laptops whose trust classes differ, the legacy delivery document.
// With the managed_device laptop full, a run promoted to the personal laptop that has room starts there:
// the invariant TestPG_QueuedRunStartsOnTheRunnerItsPromotionChose pins for two personal laptops.
func TestPG_RunTrust_PromotedRunStartsOnTheFreePersonalLaptopBesideAFullManagedOne(t *testing.T) {
	e := newQueueEnv(t)
	p, a, b := cappedLaptops(e, 1)
	e.setTrust(a, types.RunnerTrustManagedDevice)
	occupyRunner(e, p, a)
	for i := 0; i < 3; i++ {
		_, got := launchThroughQueue(e, p, a, b)
		if got.State == types.RunFailed || got.RunnerID == nil || *got.RunnerID != b {
			t.Fatalf("launch %d = %s on %v detail %q; want it started on the personal laptop with room %s", i, got.State, got.RunnerID, got.StatusDetail, b)
		}
		if c := e.runTrust(got.ID); c != types.RunnerTrustPersonal {
			t.Errorf("launch %d recorded %q, want personal", i, c)
		}
	}
}
