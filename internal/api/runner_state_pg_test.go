// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type offlineContainmentProbe struct {
	*fakeRunner
	stops atomic.Int32
}

func (p *offlineContainmentProbe) StopProxy(context.Context, string) error {
	p.stops.Add(1)
	return runner.ErrPendingOnRunner
}

func TestRunnerStatePG_OfflineUsesExistingHeadlessLossPath(t *testing.T) {
	pool, cfg, rn, run, _, _ := runnerExitAPIFixture(t)
	ctx := t.Context()
	st := cfg.Store.(store.PG)
	probe := &offlineContainmentProbe{fakeRunner: &fakeRunner{}}
	cfg.Runner = probe
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET interactive=false,ends_at=NULL WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	srv := New(cfg)
	missing := []runnerwire.RunState{{RunID: run.ID, SandboxPresent: false}}
	if err := finalStatePage(t, srv, rn.ID, missing...); !errors.Is(err, store.ErrRunnerReconciliationRequired) {
		t.Fatalf("fresh missing sandbox admitted: %v", err)
	}
	if err := srv.OfflineFromRunner(ctx, rn.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetRun(ctx, run.ID)
	if got.LostAt != nil || probe.stops.Load() != 0 {
		t.Fatal("fresh disconnect changed lifecycle")
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET token_renewed_at=now()-interval '2 hours' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := srv.OfflineFromRunner(ctx, rn.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetRun(ctx, run.ID)
	if got.State != types.RunRunning || got.LostAt == nil || got.LostReason != types.LostOutage || probe.stops.Load() != 1 {
		t.Fatalf("headless loss: %+v stops=%d", got, probe.stops.Load())
	}
	if err := srv.OfflineFromRunner(ctx, rn.ID); err != nil {
		t.Fatal(err)
	}
	if err := finalStatePage(t, srv, rn.ID, missing...); err != nil {
		t.Fatalf("lapsed missing sandbox cannot converge: %v", err)
	}
	if err := finalStatePage(t, srv, rn.ID); err != nil {
		t.Fatalf("omitted kept sandbox cannot converge: %v", err)
	}
	if probe.stops.Load() != 1 {
		t.Fatal("offline callback repeated loss effects")
	}
}
