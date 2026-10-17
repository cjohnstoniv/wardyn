// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The size the queue promotes a waiting run at on a runner (startSizeOn of its unbound size) is the size
// a fresh launch gets with that runner as its source, and the size the promoted run then starts with:
// defaults, explicit asks, policy-written sizes, the pool's own disk and the organisation's.
func TestPG_QueueSizesARunAsAFreshLaunchOnThatRunner(t *testing.T) {
	type amounts = types.RunnerAmounts
	size := func(cpu, mem, disk int64) amounts { return amounts{CPUMillis: cpu, MemoryMiB: mem, DiskMiB: disk} }
	for _, tc := range []struct {
		name             string
		alpha            amounts // alpha's per-run limit
		poolCPU, poolMem *types.RunnerPoolAmount
		poolDisk         *types.RunnerPoolAmount
		orgDisk          [2]int // organisation default, maximum
		extra            string // request fields
	}{
		{name: "a default sizes, disk from the runner", alpha: size(2000, 2048, 3072)},
		{name: "a2 default memory above the runner's per-run memory", alpha: size(2000, 512, 3072)},
		{name: "b explicit ask over the runner on cpu and disk", alpha: size(1000, 8192, 4096), extra: `,"resources":{"cpu_millis":3000,"memory_mib":3000,"disk_mib":8192}`},
		{name: "b2 explicit ask under the runner everywhere", alpha: size(4000, 8192, 8192), extra: `,"resources":{"cpu_millis":1500,"memory_mib":2000,"disk_mib":2048}`},
		{name: "b3 explicit ask, runner binds memory only", alpha: size(4000, 1024, 8192), extra: `,"resources":{"cpu_millis":1500,"memory_mib":3000,"disk_mib":2048}`},
		{name: "c policy-written disk over the runner", alpha: size(2000, 4096, 4096), extra: `,"inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"],"resources":{"disk_mib":7168}}`},
		{name: "c2 policy-written disk under the runner", alpha: size(2000, 4096, 8192), extra: `,"inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"],"resources":{"disk_mib":2048}}`},
		{name: "c3 request over a policy-written disk", alpha: size(2000, 4096, 4096), extra: `,"resources":{"disk_mib":100352},"inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"],"resources":{"disk_mib":7168}}`},
		{name: "c4 policy-written cpu and memory", alpha: size(1000, 1024, 4096), extra: `,"inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"],"resources":{"cpu_millis":2000,"memory_mib":3000}}`},
		{name: "d pool disk default over the runner", alpha: size(2000, 4096, 1024), poolDisk: &types.RunnerPoolAmount{Default: 2048, Cap: 5120}},
		{name: "d2 pool disk default under the runner", alpha: size(2000, 4096, 4096), poolDisk: &types.RunnerPoolAmount{Default: 2048, Cap: 5120}},
		{name: "d3 ask over the pool's disk maximum", alpha: size(2000, 4096, 8192), poolDisk: &types.RunnerPoolAmount{Default: 2048, Cap: 5120}, extra: `,"resources":{"disk_mib":100352}`},
		{name: "d4 pool cpu and memory defaults over the runner", alpha: size(1000, 1024, 2048), poolCPU: &types.RunnerPoolAmount{Default: 3000, Cap: 4000}, poolMem: &types.RunnerPoolAmount{Default: 6000, Cap: 8192}},
		{name: "e organisation disk default over the runner", alpha: size(2000, 4096, 2048), orgDisk: [2]int{4096, 8192}},
		{name: "e2 organisation disk maximum only", alpha: size(2000, 4096, 16384), orgDisk: [2]int{0, 8192}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newQueueEnv(t)
			ctx := context.Background()
			e.setDiskWord(types.StorageEnforcementFilesystem)
			if tc.orgDisk != [2]int{} {
				e.orgStorage(tc.orgDisk[0], tc.orgDisk[1])
			}
			pool := e.createPool("Sized", types.RunnerPoolOrganization)
			e.addExecutors(pool.ID, "alpha")
			bg := types.RunnerPoolBackgroundLimits{CPUMillis: &types.RunnerPoolAmount{Default: 1000, Cap: 4000},
				MemoryMiB: &types.RunnerPoolAmount{Default: 1024, Cap: 8192}, Lifetime: &types.RunnerPoolDuration{DefaultSec: 3600, MaxSec: 7200}, DiskMiB: tc.poolDisk}
			if tc.poolCPU != nil {
				bg.CPUMillis = tc.poolCPU
			}
			if tc.poolMem != nil {
				bg.MemoryMiB = tc.poolMem
			}
			limits := types.RunnerPoolLimits{Background: &bg, Barriers: []types.ConfinementClass{types.CC1, types.CC2, types.CC3}}
			setCap := func(n int) {
				limits.MaxConcurrentRuns = nil
				if n > 0 {
					limits.MaxConcurrentRuns = intp(n)
				}
				if _, err := e.pg.UpdateRunnerPool(ctx, pool.ID, e.revision(pool.ID), nil, nil, &limits, nil); err != nil {
					t.Fatal(err)
				}
			}
			setCap(0)
			if _, err := e.pool.Exec(ctx, `UPDATE remote_runners SET run_cpu_millis = $1, run_memory_mib = $2, run_disk_mib = $3 WHERE id = 'alpha'`,
				tc.alpha.CPUMillis, tc.alpha.MemoryMiB, tc.alpha.DiskMiB); err != nil {
				t.Fatal(err)
			}
			body := limitsBody(fmt.Sprintf(`"runner_pool_id":%q`, pool.ID) + tc.extra)

			// 1. A fresh launch, placed at once with alpha as its source.
			w := e.call(e.person, http.MethodPost, "/api/v1/runs", body)
			if w.Code != http.StatusCreated {
				t.Fatalf("direct launch = %d %s", w.Code, w.Body)
			}
			first := decodeJSON[types.AgentRun](t, w)
			if first.State == types.RunQueued || first.RunnerPoolExecutor != "alpha" {
				t.Fatalf("direct launch is %s on %q", first.State, first.RunnerPoolExecutor)
			}
			direct := e.reservedOn(first.ID)
			if _, err := e.pool.Exec(ctx, `UPDATE agent_runs SET state = 'COMPLETED' WHERE id = $1`, first.ID); err != nil {
				t.Fatal(err)
			}

			// 2. The same launch waits behind the pool's cap, then is promoted and started.
			setCap(1)
			occupant := e.occupy(e.reread(pool))
			w = e.call(e.person, http.MethodPost, "/api/v1/runs", body)
			if w.Code != http.StatusCreated {
				t.Fatalf("queued launch = %d %s", w.Code, w.Body)
			}
			queued := decodeJSON[types.AgentRun](t, w)
			if queued.State != types.RunQueued {
				t.Fatalf("second launch is %s, want QUEUED", queued.State)
			}
			var raw json.RawMessage
			var launch queuedLaunch
			if err := e.pool.QueryRow(ctx, `SELECT launch FROM run_queue WHERE run_id = $1`, queued.ID).Scan(&raw); err != nil || json.Unmarshal(raw, &launch) != nil || launch.Unbound == nil {
				t.Fatalf("no unbound size on the queued launch: %s %v", raw, err)
			}
			room, err := e.srv.queueRunnerFree(withOIDCHuman(ctx, e.personP), e.reread(pool), launch)
			if err != nil {
				t.Fatal(err)
			}
			verdict, sized := room.needs["alpha"]
			e.free(occupant)
			e.sweep()
			var got types.AgentRun
			e.eventually("the run to leave the queue", func() bool {
				got = e.run(queued.ID)
				return got.State == types.RunRunning || got.State == types.RunFailed || got.State == types.RunCompleted
			})
			started := e.reservedOn(queued.ID)
			e.runner.fakeRunner.mu.Lock()
			spec := e.runner.fakeRunner.lastSpec
			e.runner.fakeRunner.mu.Unlock()
			if got.State != types.RunRunning {
				t.Fatalf("the promoted run ended %s (%s / %s)", got.State, got.StatusDetail, got.FailureHint)
			}
			if !sized || verdict != direct {
				t.Errorf("the queue sizes the run on alpha at %+v, a fresh launch there gets %+v", verdict, direct)
			}
			if started != direct {
				t.Errorf("the promoted run started reserving %+v, a fresh launch there gets %+v", started, direct)
			}
			if spec.RunID == queued.ID && (spec.Resources.CPUMillis != started.CPUMillis || spec.Resources.MemoryMiB != started.MemoryMiB) {
				t.Errorf("the sandbox was given cpu %d mem %d, the run reserves %+v", spec.Resources.CPUMillis, spec.Resources.MemoryMiB, started)
			}
		})
	}
}
