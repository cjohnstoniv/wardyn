// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_RunProxyConfigs (#1176, migration 0090): a run's sealed proxy config
// is written, replaced, read and deleted by run; the purge removes exactly the
// rows of terminal runs; and a deleted run takes its row with it. Asserts only
// on its own rows: the database is shared (runsPGPool).
func TestPG_RunProxyConfigs(t *testing.T) {
	s := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	newRun := func() types.AgentRun {
		r, err := s.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedAt: time.Now(), UpdatedAt: time.Now(),
			CreatedBy: "op@example.com", Agent: "claude-code", Task: "t", ConfinementClass: types.CC1,
			State: types.RunRunning, RunnerTarget: "docker"})
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		return r
	}
	live, done := newRun(), newRun()
	for _, r := range []types.AgentRun{live, done} {
		if err := s.PutRunProxyConfig(ctx, r.ID, []byte("first")); err != nil {
			t.Fatalf("PutRunProxyConfig: %v", err)
		}
		if err := s.PutRunProxyConfig(ctx, r.ID, []byte("sealed-"+r.ID.String())); err != nil {
			t.Fatalf("PutRunProxyConfig (replace): %v", err)
		}
	}
	if got, err := s.GetRunProxyConfig(ctx, live.ID); err != nil || string(got) != "sealed-"+live.ID.String() {
		t.Fatalf("GetRunProxyConfig = %q, %v; want the replaced row", got, err)
	}
	if _, err := s.GetRunProxyConfig(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRunProxyConfig of no row = %v, want ErrNotFound", err)
	}

	if ok, err := s.UpdateRunStateIf(ctx, done.ID, types.RunRunning, types.RunKilled); err != nil || !ok {
		t.Fatalf("UpdateRunStateIf: %v %v", ok, err)
	}
	if n, err := s.PurgeTerminalRunProxyConfigs(ctx); err != nil || n < 1 {
		t.Fatalf("PurgeTerminalRunProxyConfigs = %d, %v; want at least this test's terminal run's row", n, err)
	}
	if _, err := s.GetRunProxyConfig(ctx, done.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a terminal run's row survived the purge: %v", err)
	}
	if _, err := s.GetRunProxyConfig(ctx, live.ID); err != nil {
		t.Errorf("a live run's row was purged: %v", err)
	}

	if err := s.DeleteRunProxyConfig(ctx, live.ID); err != nil {
		t.Fatalf("DeleteRunProxyConfig: %v", err)
	}
	if err := s.DeleteRunProxyConfig(ctx, live.ID); err != nil {
		t.Fatalf("DeleteRunProxyConfig again: %v", err)
	}
	if _, err := s.GetRunProxyConfig(ctx, live.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("row after delete: %v, want ErrNotFound", err)
	}

	gone := newRun()
	if err := s.PutRunProxyConfig(ctx, gone.ID, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, gone.ID); err != nil {
		t.Fatalf("delete run: %v", err)
	}
	if _, err := s.GetRunProxyConfig(ctx, gone.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a deleted run's row survived: %v", err)
	}
}
