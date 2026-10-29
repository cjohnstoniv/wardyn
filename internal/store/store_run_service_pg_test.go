// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func serviceRun(state types.RunState, ref string) types.AgentRun {
	r := newRun(state)
	r.SandboxRef = ref
	r.Service = &types.RunService{Restart: types.ServiceRestartOnFailure, Port: 8080,
		Health: &types.ServiceHealth{Path: "/healthz", IntervalSec: 15, FailureThreshold: 3}}
	return r
}

func serviceRunIDs(t *testing.T, s store.PG) []uuid.UUID {
	t.Helper()
	runs, err := s.ListServiceRuns(context.Background())
	if err != nil {
		t.Fatalf("ListServiceRuns: %v", err)
	}
	var ids []uuid.UUID
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	return ids
}

// TestRunService_InsertScanAndStatusLeaveUpdatedAt: the service spec is written at insert and read
// back whole, a run with none reads nil, and the scoped status write changes service_status only: it
// never moves updated_at, which is the idle and tail-upload clock.
func TestRunService_InsertScanAndStatusLeaveUpdatedAt(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	s := store.NewPG(pool)

	plain := persistRun(t, ctx, pool, newRun(types.RunRunning))
	if plain.Service != nil || plain.ServiceStatus != nil {
		t.Fatalf("a run with no service reads service %+v, status %+v", plain.Service, plain.ServiceStatus)
	}
	run := persistRun(t, ctx, pool, serviceRun(types.RunRunning, "ctr-1"))
	got, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Service == nil || got.Service.Restart != types.ServiceRestartOnFailure || got.Service.Port != 8080 ||
		got.Service.Health == nil || *got.Service.Health != *run.Service.Health {
		t.Fatalf("service = %+v, want the spec written at insert", got.Service)
	}
	if got.ServiceStatus != nil {
		t.Fatalf("service_status = %+v before any observation, want absent", got.ServiceStatus)
	}

	exit := 3
	st := types.RunServiceStatus{Health: types.ServiceHealthUnhealthy, Reason: "restarting", Restarts: 2, LastExitCode: &exit,
		ObservedAt: time.Now().UTC().Truncate(time.Millisecond)}
	if ok, err := s.SetRunServiceStatus(ctx, run.ID, st); err != nil || !ok {
		t.Fatalf("SetRunServiceStatus = %v, %v; want true, nil", ok, err)
	}
	after, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun after set: %v", err)
	}
	if a := after.ServiceStatus; a == nil || a.Health != st.Health || a.Reason != st.Reason || a.Restarts != 2 ||
		a.LastExitCode == nil || *a.LastExitCode != 3 || !a.ObservedAt.Equal(st.ObservedAt) {
		t.Errorf("service_status = %+v, want %+v", a, st)
	}
	if !after.UpdatedAt.Equal(got.UpdatedAt) {
		t.Errorf("updated_at moved %v -> %v on a service status write; the idle reaper and the tail-upload grace both measure from it",
			got.UpdatedAt, after.UpdatedAt)
	}
	if after.Service == nil || after.Service.Port != 8080 {
		t.Errorf("the status write changed the spec: %+v", after.Service)
	}
}

// TestRunService_StatusWritesOnlyWhileRunning: a run that is not RUNNING takes no status, and the
// answer says so.
func TestRunService_StatusWritesOnlyWhileRunning(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	s := store.NewPG(pool)

	ended := persistRun(t, ctx, pool, serviceRun(types.RunCompleted, "ctr-2"))
	ok, err := s.SetRunServiceStatus(ctx, ended.ID, types.RunServiceStatus{Health: types.ServiceHealthHealthy, ObservedAt: time.Now().UTC()})
	if err != nil || ok {
		t.Fatalf("SetRunServiceStatus on a COMPLETED run = %v, %v; want false, nil", ok, err)
	}
	got, err := s.GetRun(ctx, ended.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.ServiceStatus != nil {
		t.Errorf("a COMPLETED run took service_status %+v", got.ServiceStatus)
	}
	if ok, err := s.SetRunServiceStatus(ctx, uuid.New(), types.RunServiceStatus{}); err != nil || ok {
		t.Errorf("SetRunServiceStatus on no run = %v, %v; want false, nil", ok, err)
	}
}

// TestRunService_ListServiceRuns: RUNNING, a service, a sandbox, not lost, not paused; nothing else.
func TestRunService_ListServiceRuns(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	s := store.NewPG(pool)

	live := persistRun(t, ctx, pool, serviceRun(types.RunRunning, "ctr-live"))
	noService := newRun(types.RunRunning)
	noService.SandboxRef = "ctr-plain"
	plain := persistRun(t, ctx, pool, noService)
	noSandbox := persistRun(t, ctx, pool, serviceRun(types.RunRunning, ""))
	starting := persistRun(t, ctx, pool, serviceRun(types.RunStarting, "ctr-start"))
	lost := persistRun(t, ctx, pool, serviceRun(types.RunRunning, "ctr-lost"))
	paused := persistRun(t, ctx, pool, serviceRun(types.RunRunning, "ctr-paused"))
	for id, set := range map[uuid.UUID]string{lost.ID: `lost_at=now()`, paused.ID: `paused_at=now()`} {
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET `+set+` WHERE id=$1`, id); err != nil {
			t.Fatalf("mark %s: %v", id, err)
		}
	}

	ids := serviceRunIDs(t, s)
	if !slices.Contains(ids, live.ID) {
		t.Errorf("the live service run is not listed: %v", ids)
	}
	for name, id := range map[string]uuid.UUID{"no service": plain.ID, "no sandbox": noSandbox.ID, "starting": starting.ID, "lost": lost.ID, "paused": paused.ID} {
		if slices.Contains(ids, id) {
			t.Errorf("a run with %s is listed", name)
		}
	}
}
