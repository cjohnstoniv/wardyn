// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
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
