// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A pre-record row reads all NULL; SetRunSizing writes the seven columns without bumping
// updated_at; a later write for another run leaves the first row unchanged.
func TestPG_SetRunSizing(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	a := persistRun(t, ctx, pool, newRun(types.RunStarting))
	b := persistRun(t, ctx, pool, newRun(types.RunStarting))

	const q = `SELECT runner_kind, agent_cpu_request_millis, agent_cpu_limit_millis,
	  agent_memory_request_mib, agent_memory_limit_mib, proxy_cpu_millis, proxy_memory_mib FROM agent_runs WHERE id=$1`
	read := func(id any) (vals [7]any) {
		t.Helper()
		var k *string
		var n [6]*int32
		if err := pool.QueryRow(ctx, q, id).Scan(&k, &n[0], &n[1], &n[2], &n[3], &n[4], &n[5]); err != nil {
			t.Fatalf("read sizing: %v", err)
		}
		if k != nil {
			vals[0] = *k
		}
		for i, v := range n {
			if v != nil {
				vals[i+1] = int(*v)
			}
		}
		return vals
	}
	if got := read(a.ID); got != [7]any{} {
		t.Fatalf("pre-record row = %v, want all NULL", got)
	}

	var updBefore any
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM agent_runs WHERE id=$1`, a.ID).Scan(&updBefore); err != nil {
		t.Fatal(err)
	}
	cpu := int64(500)
	if err := pg.SetRunSizing(ctx, a.ID, store.RunSizing{RunnerKind: "k8s", AgentCPURequestMillis: 3000,
		AgentCPULimitMillis: 3000, AgentMemoryRequestMiB: 6144, AgentMemoryLimitMiB: 6144,
		ProxyCPUMillis: &cpu, ProxyMemoryMiB: 256}); err != nil {
		t.Fatal(err)
	}
	want := [7]any{"k8s", 3000, 3000, 6144, 6144, 500, 256}
	if got := read(a.ID); got != want {
		t.Errorf("row = %v, want %v", got, want)
	}
	var updAfter any
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM agent_runs WHERE id=$1`, a.ID).Scan(&updAfter); err != nil {
		t.Fatal(err)
	}
	if updBefore != updAfter {
		t.Errorf("SetRunSizing bumped updated_at: %v -> %v", updBefore, updAfter)
	}

	if err := pg.SetRunSizing(ctx, b.ID, store.RunSizing{RunnerKind: "docker", AgentCPULimitMillis: 1, AgentMemoryLimitMiB: 1}); err != nil {
		t.Fatal(err)
	}
	if got := read(a.ID); got != want {
		t.Errorf("first row changed by a later dispatch: %v", got)
	}
	if got := read(b.ID); got[5] != nil {
		t.Errorf("nil ProxyCPUMillis must store NULL, got %v", got[5])
	}
}
