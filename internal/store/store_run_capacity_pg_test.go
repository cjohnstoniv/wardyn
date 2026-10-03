// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_RunCapacityAggregate pins the F-D5 state table, F-D4's per-runner basis and F-D6's
// unknown-is-a-count against Postgres.
func TestPG_RunCapacityAggregate(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	cpu := func(v int64) *int64 { return &v }

	mk := func(state types.RunState, owner, target string, mutate func(*types.AgentRun)) uuid.UUID {
		t.Helper()
		r := newRun(state)
		r.CreatedBy, r.RunnerTarget = owner, target
		if mutate != nil {
			mutate(&r)
		}
		return persistRun(t, ctx, pool, r).ID
	}
	size := func(id uuid.UUID, z store.RunSizing) {
		t.Helper()
		if err := pg.SetRunSizing(ctx, id, z); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	k8s := func(c, m int64, proxyCPU int64) store.RunSizing {
		return store.RunSizing{RunnerKind: "k8s", AgentCPURequestMillis: c, AgentCPULimitMillis: c * 2,
			AgentMemoryRequestMiB: m, AgentMemoryLimitMiB: m * 2, ProxyCPUMillis: cpu(proxyCPU), ProxyMemoryMiB: 256}
	}

	// alice: a ratio-split k8s row (request 1000 under limit 2000, proxy 500m/256Mi), a paused one.
	a1 := mk(types.RunRunning, "alice", "k8s", nil)
	size(a1, k8s(1000, 2048, 500))
	a2 := mk(types.RunWaiting, "alice", "k8s", nil)
	size(a2, k8s(1000, 2048, 500))
	exec(`UPDATE agent_runs SET paused_at=now() WHERE id=$1`, a2)
	// A docker row: caps, proxy CPU uncapped. Never summed into the k8s figures.
	d1 := mk(types.RunRunning, "bob", "docker", nil)
	size(d1, store.RunSizing{RunnerKind: "docker", AgentCPURequestMillis: 4000, AgentCPULimitMillis: 4000,
		AgentMemoryRequestMiB: 8192, AgentMemoryLimitMiB: 8192, ProxyMemoryMiB: 256})
	// Legacy rows: unknown, attributed through runner_target, in no sum.
	mk(types.RunRunning, "bob", "k8s", nil)
	mk(types.RunStarting, "bob", "docker", nil)
	// Kept, PENDING, terminal (a failed-teardown run is terminal and never counted).
	kept := mk(types.RunRunning, "carol", "k8s", nil)
	size(kept, k8s(9000, 9000, 500))
	exec(`UPDATE agent_runs SET lost_at=now() WHERE id=$1`, kept)
	pend := mk(types.RunPending, "carol", "k8s", nil)
	size(pend, k8s(9000, 9000, 500))
	for _, st := range []types.RunState{types.RunFailed, types.RunCompleted, types.RunKilled} {
		id := mk(st, "carol", "k8s", nil)
		size(id, k8s(9000, 9000, 500))
	}
	// A STARTING row whose proxy is Pending is not unschedulable; an agent pod Unschedulable is.
	proxyPending := mk(types.RunStarting, "dave", "k8s", nil)
	size(proxyPending, k8s(500, 512, 500))
	exec(`UPDATE agent_runs SET status_detail='proxy: Pending: waiting' WHERE id=$1`, proxyPending)

	blocked := func(d string) string {
		if strings.HasPrefix(d, "pod: Unschedulable") {
			return "Unschedulable"
		}
		return ""
	}
	now := time.Now().UTC()
	got, err := pg.RunCapacity(ctx, store.RunCapacityOpts{Now: now, CurrentKind: "k8s", UnschedulableReason: blocked})
	if err != nil {
		t.Fatal(err)
	}

	wantStates := map[string]int{"RUNNING": 4, "WAITING_FOR_CONFIRMATION": 1, "STARTING": 2, "PENDING": 1}
	for st, n := range wantStates {
		if got.States[st] != n {
			t.Errorf("states[%s] = %d, want %d (%v)", st, got.States[st], n, got.States)
		}
	}
	if len(got.States) != len(wantStates) {
		t.Errorf("states = %v, want exactly %v (no terminal state)", got.States, wantStates)
	}
	if got.Paused != 1 || got.Kept != 1 {
		t.Errorf("paused=%d kept=%d, want 1 and 1", got.Paused, got.Kept)
	}

	k := got.ByRunner["k8s"]
	if k.Basis != "requests" || k.Holding != 4 || k.Unknown != 1 {
		t.Errorf("k8s = %+v, want requests basis, 4 holding, 1 unknown", k)
	}
	// Requests summed, never limits: alice 1000+1000, dave 500.
	if k.AgentCPURequestMillis != 2500 || k.AgentCPULimitMillis != 5000 {
		t.Errorf("k8s cpu request/limit = %d/%d, want 2500/5000", k.AgentCPURequestMillis, k.AgentCPULimitMillis)
	}
	if k.AgentMemoryRequestMiB != 4608 || k.ProxyCPUMillis != 1500 || k.ProxyMemoryMiB != 768 {
		t.Errorf("k8s memory/proxy = %+v", k)
	}
	if k.HeldCPUMillis != 4000 || k.HeldMemoryMiB != 4608+768 {
		t.Errorf("k8s held = %d/%d", k.HeldCPUMillis, k.HeldMemoryMiB)
	}
	d := got.ByRunner["docker"]
	if d.Basis != "caps" || d.Holding != 2 || d.Unknown != 1 || d.AgentCPULimitMillis != 4000 ||
		d.ProxyCPUMillis != 0 || d.ProxyCPUUncapped != 1 || d.HeldCPUMillis != 4000 {
		t.Errorf("docker = %+v", d)
	}
	// Totals carry the current kind's entry only; the docker rows appear only under by_runner.
	if got.Totals.AgentCPURequestMillis != 2500 || got.Totals.Holding != 4 || got.Totals.Unknown != 1 {
		t.Errorf("totals = %+v, want k8s figures only: 2500 cpu request, 4 holding, 1 unknown", got.Totals)
	}
	ages := 0
	for _, b := range got.AgeBuckets {
		ages += b.Count
	}
	if ages != 6 || got.AgeBuckets[0].Bucket != "under_1h" || got.AgeBuckets[0].Count != 6 {
		t.Errorf("age buckets = %+v, want all 6 holding runs under 1h", got.AgeBuckets)
	}
	if got.UnschedulableTotal != 0 || len(got.Unschedulable) != 0 {
		t.Errorf("proxy: Pending must not be unschedulable: %+v", got.Unschedulable)
	}
}

// Unschedulable runs are listed oldest first and capped at 20 with the full total; owners cap at
// 50 and report truncation.
func TestPG_RunCapacityAggregate_Caps(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	now := time.Now().UTC()

	for i := 0; i < 51; i++ {
		r := newRun(types.RunRunning)
		r.CreatedBy, r.RunnerTarget = fmt.Sprintf("owner-%02d", i), "k8s"
		id := persistRun(t, ctx, pool, r).ID
		cpu := int64(100)
		if err := pg.SetRunSizing(ctx, id, store.RunSizing{RunnerKind: "k8s", AgentCPURequestMillis: int64(100 + i),
			AgentCPULimitMillis: 1, AgentMemoryRequestMiB: 1, AgentMemoryLimitMiB: 1, ProxyCPUMillis: &cpu, ProxyMemoryMiB: 1}); err != nil {
			t.Fatal(err)
		}
	}
	var oldest uuid.UUID
	for i := 0; i < 21; i++ {
		r := newRun(types.RunStarting)
		r.CreatedAt = now.Add(-time.Duration(i+1) * time.Minute)
		r.RunnerTarget = "k8s"
		id := persistRun(t, ctx, pool, r).ID
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET status_detail='pod: Unschedulable: 0/3 nodes' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		if i == 20 {
			oldest = id
		}
	}
	got, err := pg.RunCapacity(ctx, store.RunCapacityOpts{Now: now, CurrentKind: "k8s",
		UnschedulableReason: func(d string) string {
			if strings.Contains(d, "Unschedulable") {
				return "Unschedulable"
			}
			return ""
		}})
	if err != nil {
		t.Fatal(err)
	}
	// 51 owners plus the one tester@example.com that owns the 21 starting runs.
	if len(got.ByOwner) != 50 || !got.ByOwnerTruncated {
		t.Errorf("by_owner = %d truncated=%v, want 50 and true", len(got.ByOwner), got.ByOwnerTruncated)
	}
	if got.ByOwner[0].Owner != "owner-50" {
		t.Errorf("first owner = %s, want the largest holder owner-50", got.ByOwner[0].Owner)
	}
	if got.UnschedulableTotal != 21 || len(got.Unschedulable) != 20 {
		t.Fatalf("unschedulable total=%d listed=%d, want 21 and 20", got.UnschedulableTotal, len(got.Unschedulable))
	}
	if got.Unschedulable[0].ID != oldest || got.Unschedulable[0].WaitedSeconds < 21*60 {
		t.Errorf("first unschedulable = %+v, want the oldest (%s)", got.Unschedulable[0], oldest)
	}
	if got.Unschedulable[0].RunnerKind != nil {
		t.Errorf("an unrecorded run's reservation must read nil, got %v", *got.Unschedulable[0].RunnerKind)
	}
}
