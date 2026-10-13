// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The main drive's size is decided once (mainDriveFor) and read by both dry doors, create and
// dispatch. Against a real Postgres: guarded by WARDYN_TEST_PG, skipped cleanly when unset, must
// PASS when set.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// diskPool is a remote pool over both executors that takes Background runs, with a main-drive
// bound when capMiB is set.
func (e *placementEnv) diskPool(name string, defMiB, capMiB int, executors ...string) types.RunnerPool {
	e.t.Helper()
	p := e.createPool(name, types.RunnerPoolOrganization)
	if len(executors) == 0 {
		executors = []string{"alpha", "beta"}
	}
	e.addExecutors(p.ID, executors...)
	bg := types.RunnerPoolBackgroundLimits{CPUMillis: types.RunnerPoolAmount{Default: 1000, Cap: 4000},
		MemoryMiB: types.RunnerPoolAmount{Default: 1024, Cap: 8192}, Lifetime: types.RunnerPoolDuration{DefaultSec: 3600, MaxSec: 7200}}
	if capMiB > 0 {
		bg.DiskMiB = &types.RunnerPoolAmount{Default: defMiB, Cap: capMiB}
	}
	limits := types.RunnerPoolLimits{Background: &bg, Barriers: []types.ConfinementClass{types.CC1, types.CC2, types.CC3}}
	out, err := e.pg.UpdateRunnerPool(context.Background(), p.ID, e.revision(p.ID), nil, nil, &limits)
	if err != nil {
		e.t.Fatalf("set pool limits: %v", err)
	}
	return out
}

// setDiskWord makes every executor, and the deployment-wide capability, report one disk word.
func (e *placementEnv) setDiskWord(w types.StorageEnforcement) {
	e.runner.fakeRunner.capsDisk = w
	for name, f := range e.runner.execFacts {
		f.EphemeralDiskEnforcement = w
		e.runner.execFacts[name] = f
	}
}

// orgStorage sets the organisation's storage.ephemeral block.
func (e *placementEnv) orgStorage(defMiB, maxMiB int) {
	e.t.Helper()
	site := types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true}, WorkspaceProviders: &types.WorkspaceProviders{
		Storage: &types.StorageProviders{Ephemeral: &types.EphemeralProvider{DefaultDiskMiB: defMiB, MaxDiskMiB: maxMiB}}}}
	if _, err := e.pg.PutSiteConfig(context.Background(), site); err != nil {
		e.t.Fatal(err)
	}
}

func (e *placementEnv) preview(body string) policyPreviewResponse {
	e.t.Helper()
	w := e.call(e.person, http.MethodPost, "/api/v1/runs/policy-preview", body)
	if w.Code != http.StatusOK {
		e.t.Fatalf("preview = %d %s", w.Code, w.Body)
	}
	return decodeJSON[policyPreviewResponse](e.t, w)
}

// nextDispatch waits for the create after the first n and returns the size its sandbox got.
func (e *placementEnv) nextDispatch(n int) runner.Resources {
	e.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); e.runner.creates() <= n && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}
	if e.runner.creates() <= n {
		e.t.Fatal("dispatch never created a sandbox")
	}
	e.runner.fakeRunner.mu.Lock()
	defer e.runner.fakeRunner.mu.Unlock()
	return e.runner.fakeRunner.lastSpec.Resources
}

// diskBody is a Background request in pool, asking for requestMiB and carrying an inline policy that
// writes policyMiB; a zero leaves that out.
func diskBody(pool types.RunnerPool, requestMiB, policyMiB int) string {
	extra := fmt.Sprintf(`"runner_pool_id":%q`, pool.ID)
	if requestMiB > 0 {
		extra += fmt.Sprintf(`,"resources":{"disk_mib":%d}`, requestMiB)
	}
	if policyMiB > 0 {
		extra += fmt.Sprintf(`,"inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"],"resources":{"disk_mib":%d}}`, policyMiB)
	}
	return limitsBody(extra)
}

// TestPreflightAndDispatchAgreeOnEphemeralDisk is the pin for "the preview always agrees with dispatch":
// across {pool bound, org default and maximum, profile maximum} x {request, policy, both, neither},
// Review and the policy preview report the size and softness the sandbox is created with.
func TestPreflightAndDispatchAgreeOnEphemeralDisk(t *testing.T) {
	for _, pooled := range []bool{false, true} {
		for _, org := range []bool{false, true} {
			for _, profile := range []bool{false, true} {
				t.Run(fmt.Sprintf("pool=%v/org=%v/profile=%v", pooled, org, profile), func(t *testing.T) {
					e := newPlacementEnv(t)
					pool := e.diskPool("Disk", 0, 0)
					if pooled {
						pool = e.diskPool("Capped", 2048, 4096)
					}
					if org {
						e.orgStorage(3072, 6144)
					}
					if profile {
						e.assignSizedProfile("capped", nil, types.GovernanceLimits{MaxEphemeralDiskMiB: 5120})
					}
					for _, ask := range []struct{ request, policy int }{{0, 0}, {1024, 0}, {100000, 0}, {0, 7168}, {1024, 7168}, {100000, 7168}} {
						body := diskBody(pool, ask.request, ask.policy)
						pf, pv := e.preflight(body), e.preview(body)
						n := e.runner.creates()
						created := e.create(body)
						res := e.nextDispatch(n)
						for door, md := range map[string]*client.MainDriveApplied{"preflight": pf.MainDrive, "policy-preview": pv.MainDrive} {
							if md == nil {
								t.Fatalf("%+v: %s carries no main_drive", ask, door)
							}
							if int64(md.AppliedMiB) != res.DiskMiB || md.Soft != res.DiskMiBSoft {
								t.Errorf("%+v: DISAGREE: %s says %d MiB soft=%v, dispatch created %d MiB soft=%v",
									ask, door, md.AppliedMiB, md.Soft, res.DiskMiB, res.DiskMiBSoft)
							}
						}
						if got := pv.Spec.Resources; got == nil || int64(got.DiskMiB) != res.DiskMiB {
							t.Errorf("%+v: the preview's spec shows %v, dispatch created %d MiB", ask, got, res.DiskMiB)
						}
						if by := pf.MainDrive.ClampedBy; by != "" {
							want := composer.WarnResourcesCapped
							if by == client.MainDriveClampPolicy {
								want = warnDiskPolicyCut
							}
							if !warned(created.Warnings, want) {
								t.Errorf("%+v: the 201 warnings %v do not say the clamp %q with %q", ask, created.Warnings, by, want)
							}
						}
					}
				})
			}
		}
	}
}

func warned(warnings []string, w string) bool {
	for _, got := range warnings {
		if strings.Contains(got, w) {
			return true
		}
	}
	return false
}

// TestRequestLessRunGetsCapWithoutDefault pins the behaviour change: a cap with no default is the
// default size, as a soft size, so a request-less run now gets the organisation's maximum.
func TestRequestLessRunGetsCapWithoutDefault(t *testing.T) {
	e := newPlacementEnv(t)
	e.orgStorage(0, 6144)
	pool := e.diskPool("Plain", 0, 0)
	pf := e.preflight(diskBody(pool, 0, 0))
	if md := pf.MainDrive; md == nil || md.AppliedMiB != 6144 || !md.Soft || md.Basis != client.MainDriveBasisDefault || md.DefaultSource != types.LimitSourceDeployment {
		t.Fatalf("main_drive = %+v, want the 6144 MiB maximum as the deployment's soft default", md)
	}
	n := e.runner.creates()
	e.create(diskBody(pool, 0, 0))
	if res := e.nextDispatch(n); res.DiskMiB != 6144 || !res.DiskMiBSoft {
		t.Errorf("dispatch created %d MiB soft=%v, want 6144 soft", res.DiskMiB, res.DiskMiBSoft)
	}
}

// TestRequestedDiskReachesTheRunnerSoft: a requested size reaches the runner as a SOFT one, which a
// docker host over overlay2/ext4 runs uncapped (docker's TestApplyDiskQuota_SoftSizeOnUnenforceableHost
// RunsUncapped pins that half); the same number written by the run's policy reaches it hard. The fake
// runner here only records the bit, so this proves the hand-off, not the host's degrade.
func TestRequestedDiskReachesTheRunnerSoft(t *testing.T) {
	e := newPlacementEnv(t)
	pool := e.diskPool("Plain", 0, 0)
	e.runner.fakeRunner.createErr = nil
	n := e.runner.creates()
	e.create(diskBody(pool, 4096, 0))
	res := e.nextDispatch(n)
	if res.DiskMiB != 4096 || !res.DiskMiBSoft {
		t.Fatalf("a requested size reached the runner as %d MiB soft=%v, want 4096 soft: on overlay2/ext4 it runs uncapped, not refused", res.DiskMiB, res.DiskMiBSoft)
	}
	n = e.runner.creates()
	e.create(diskBody(pool, 0, 4096))
	if res = e.nextDispatch(n); res.DiskMiBSoft {
		t.Fatalf("a size the policy writes reached the runner soft: %d MiB", res.DiskMiB)
	}
}

// TestMainDriveUnenforceable (owner ruling A7): a size the run's policy writes, on a runner that
// reports it cannot cap disk, is refused at Review, the preview and create alike; a soft size on
// the same runner is not.
func TestMainDriveUnenforceable(t *testing.T) {
	e := newPlacementEnv(t)
	e.setDiskWord(types.StorageEnforcementNone)
	pool := e.diskPool("Plain", 0, 0)
	hard, soft := diskBody(pool, 0, 4096), diskBody(pool, 4096, 0)
	for _, door := range placementDoors {
		w := e.call(e.person, http.MethodPost, door, hard)
		if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonMainDriveUnenforceable {
			t.Errorf("%s with a policy size: %d %s, want 422 %s", door, w.Code, w.Body, reasonMainDriveUnenforceable)
		}
		if w.Code == http.StatusUnprocessableEntity && !strings.Contains(w.Body.String(), mainDriveUnenforceableMsg()) {
			t.Errorf("%s: the sentence is not the golden one: %s", door, w.Body)
		}
		if w := e.call(e.person, http.MethodPost, door, soft); w.Code >= 300 {
			t.Errorf("%s with a requested size: %d %s, want it accepted (a soft size runs uncapped)", door, w.Code, w.Body)
		}
	}
	e.setDiskWord(types.StorageEnforcementFilesystem)
	if w := e.call(e.person, http.MethodPost, "/api/v1/runs/preflight", hard); w.Code != http.StatusOK {
		t.Errorf("a policy size on a runner that can cap disk: %d %s", w.Code, w.Body)
	}
}

// TestMainDriveUnenforceableJudgesTheRunsOwnPool: on a mixed estate (docker over ext4 reporting
// `none`, Kubernetes reporting `eviction`) the deployment-wide word is the weakest, `none`. A policy
// size is refused in a pool with ANY `none` target, since placement picks a target by run id and
// never by disk (a hard size must never bind to a `none` target); a pool of the Kubernetes executor
// alone is accepted. The refused pools create no sandbox, so no hard size lands on `none`.
func TestMainDriveUnenforceableJudgesTheRunsOwnPool(t *testing.T) {
	e := newPlacementEnv(t)
	e.runner.fakeRunner.capsDisk = types.StorageEnforcementNone // the orchestrator's weakest word
	e.runner.execFacts["alpha"] = runner.TargetFacts{Classes: e.runner.execFacts["alpha"].Classes, EphemeralDiskEnforcement: types.StorageEnforcementNone}
	e.runner.execFacts["beta"] = runner.TargetFacts{Classes: e.runner.execFacts["beta"].Classes, EphemeralDiskEnforcement: types.StorageEnforcementEviction}
	dockerPool := e.diskPool("Docker", 0, 0, "alpha")
	k8sPool := e.diskPool("K8s", 0, 0, "beta")
	both := e.diskPool("Both", 0, 0)
	for _, door := range placementDoors {
		for _, tc := range []struct {
			pool    types.RunnerPool
			refused bool
		}{{dockerPool, true}, {k8sPool, false}, {both, true}} {
			w := e.call(e.person, http.MethodPost, door, diskBody(tc.pool, 0, 4096))
			refused := w.Code == http.StatusUnprocessableEntity && errorReason(w) == reasonMainDriveUnenforceable
			if refused != tc.refused || (!refused && w.Code >= 300) {
				t.Errorf("%s in pool %s: %d %s, want refused=%v", door, tc.pool.Name, w.Code, w.Body, tc.refused)
			}
		}
	}
	// Every create above that was accepted ran in the Kubernetes pool: none reached the `none` executor.
	for _, ex := range e.runner.onExecutor {
		if ex == "alpha" {
			t.Errorf("a policy-written size was created on executor alpha, which reports none")
		}
	}
	// A soft size is still fine in the mixed pool: it runs uncapped there, by design.
	if w := e.call(e.person, http.MethodPost, "/api/v1/runs/preflight", diskBody(both, 4096, 0)); w.Code != http.StatusOK {
		t.Errorf("a requested size in the mixed pool: %d %s", w.Code, w.Body)
	}
}

// TestClampWarningNamesThePolicyWhenThePolicyCut: a request above the size the run's own policy
// writes is cut to it, and the 201 does not blame an operator maximum for that (the policy may be
// the launcher's own). A cut by a real maximum keeps the operator sentence.
func TestClampWarningNamesThePolicyWhenThePolicyCut(t *testing.T) {
	e := newPlacementEnv(t)
	pool := e.diskPool("Plain", 0, 0)
	got := e.create(diskBody(pool, 100000, 7168)).Warnings
	if !warned(got, warnDiskPolicyCut) || warned(got, composer.WarnResourcesCapped) {
		t.Errorf("warnings = %v, want the policy sentence and not %q", got, composer.WarnResourcesCapped)
	}
	e.orgStorage(0, 6144)
	if got = e.create(diskBody(pool, 100000, 0)).Warnings; !warned(got, composer.WarnResourcesCapped) {
		t.Errorf("a cut by the organisation's maximum: warnings = %v, want %q", got, composer.WarnResourcesCapped)
	}
}
