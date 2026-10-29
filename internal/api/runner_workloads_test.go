// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// workloadSweepStore is the in-memory store with the two workload calls the status sweep makes; every other
// workload call is unset and would panic.
type workloadSweepStore struct {
	*authzStore
	store.WorkloadStore

	mu     sync.Mutex
	views  []store.WorkloadView
	pushed map[uuid.UUID]string
}

func (f *workloadSweepStore) ListK8sWorkloadStatuses(context.Context) ([]store.WorkloadView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.WorkloadView(nil), f.views...), nil
}

func (f *workloadSweepStore) SetWorkloadStatusPushed(_ context.Context, id uuid.UUID, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushed[id] = hash
	return nil
}

func newWorkloadSweepServer(t *testing.T) (*Server, *workloadSweepStore) {
	t.Helper()
	f := &workloadSweepStore{authzStore: newAuthzStore(), pushed: map[uuid.UUID]string{}}
	return New(baseTestConfig(newHarness(t), f)), f
}

// k8sView is a custom resource's workload reported by runner, its current run in state.
func k8sView(name string, runner uuid.UUID, state types.RunState) store.WorkloadView {
	run, gen := uuid.New(), int64(4)
	return store.WorkloadView{Workload: store.Workload{ID: uuid.New(), Owner: "service:ci-bot", Name: name, Source: store.WorkloadSourceK8s,
		SourceNamespace: "team-a", SourceRunnerID: &runner, K8sGeneration: &gen, Generation: 2, CurrentGeneration: 2, CurrentRunID: &run},
		RunState: state}
}

// A workload event outside the wire's shape is refused for good, before any store call.
func TestWorkloadFromRunner_RefusesAReportOutsideItsShape(t *testing.T) {
	srv, _ := newWorkloadSweepServer(t)
	run := uuid.New()
	good := `{"op":"apply","namespace":"team-a","name":"web","uid":"u1","generation":1,"spec":{}}`
	for name, ev := range map[string]runnerwire.Event{
		"names a run":    {Kind: runnerwire.EventWorkload, RunID: &run, Data: json.RawMessage(good)},
		"unknown field":  {Kind: runnerwire.EventWorkload, Data: json.RawMessage(`{"op":"apply","namespace":"team-a","name":"web","uid":"u1","generation":1,"spec":{},"pool":"p"}`)},
		"trailing data":  {Kind: runnerwire.EventWorkload, Data: json.RawMessage(good + `{}`)},
		"unknown op":     {Kind: runnerwire.EventWorkload, Data: json.RawMessage(`{"op":"patch","namespace":"team-a","name":"web","uid":"u1"}`)},
		"bad namespace":  {Kind: runnerwire.EventWorkload, Data: json.RawMessage(`{"op":"sync","namespace":"Team_A"}`)},
		"apply, no spec": {Kind: runnerwire.EventWorkload, Data: json.RawMessage(`{"op":"apply","namespace":"team-a","name":"web","uid":"u1","generation":1}`)},
		"oversize": {Kind: runnerwire.EventWorkload, Data: json.RawMessage(`{"op":"sync","namespace":"team-a","names":["` +
			strings.Repeat("a", maxWorkloadReportBytes) + `"]}`)},
	} {
		if err := srv.WorkloadFromRunner(context.Background(), uuid.New(), ev); !errors.Is(err, errRunnerEventShape) {
			t.Errorf("%s: %v, want a shape refusal", name, err)
		}
	}
}

// The status a stored workload reads as: a refusal of its current spec first, then its run's state.
func TestWorkloadStatusOf(t *testing.T) {
	runner := uuid.New()
	refused := k8sView("web", runner, types.RunRunning)
	refused.Generation = 3
	refused.LastRefusal = &store.WorkloadRefusal{Generation: 3, Reason: "policy_ceiling_exceeded", Message: strings.Repeat("é", 400)}
	inactive := k8sView("web", runner, types.RunRunning)
	inactive.Generation = 3
	inactive.LastRefusal = &store.WorkloadRefusal{Generation: 3, Reason: reasonWorkloadBindingInactive}
	oldRefusal := k8sView("web", runner, types.RunRunning)
	oldRefusal.LastRefusal = &store.WorkloadRefusal{Generation: 1, Reason: "policy_ceiling_exceeded"}
	unhealthy := k8sView("web", runner, types.RunRunning)
	unhealthy.ServiceHealth = string(types.ServiceHealthUnhealthy)
	owed := k8sView("web", runner, types.RunRunning)
	owed.Generation = 3
	noRun := k8sView("web", runner, "")
	noRun.CurrentRunID = nil
	for _, tc := range []struct {
		name  string
		v     store.WorkloadView
		phase string
	}{
		{"refused", refused, runnerwire.WorkloadPhaseRefused},
		{"binding inactive", inactive, runnerwire.WorkloadPhaseBindingInactive},
		{"a refusal of an older spec", oldRefusal, runnerwire.WorkloadPhaseRunning},
		{"unhealthy", unhealthy, runnerwire.WorkloadPhaseUnhealthy},
		{"spec ahead of its run", owed, runnerwire.WorkloadPhasePending},
		{"no run", noRun, runnerwire.WorkloadPhasePending},
		{"queued", k8sView("web", runner, types.RunQueued), runnerwire.WorkloadPhaseQueued},
		{"starting", k8sView("web", runner, types.RunStarting), runnerwire.WorkloadPhasePending},
		{"running", k8sView("web", runner, types.RunRunning), runnerwire.WorkloadPhaseRunning},
		{"completed", k8sView("web", runner, types.RunCompleted), runnerwire.WorkloadPhaseCompleted},
		{"killed", k8sView("web", runner, types.RunKilled), runnerwire.WorkloadPhaseEnded},
	} {
		a := workloadStatusOf(tc.v)
		if a.Phase != tc.phase {
			t.Errorf("%s: phase %q, want %q", tc.name, a.Phase, tc.phase)
		}
		if err := a.Validate(); err != nil {
			t.Errorf("%s: %+v does not fit the wire: %v", tc.name, a, err)
		}
		if a.ObservedGeneration != 4 || a.Namespace != "team-a" || a.Name != "web" {
			t.Errorf("%s: status %+v, want team-a/web at observed generation 4", tc.name, a)
		}
	}
}

// The sweep pushes a status only when it moved since it was last sent, and only to a runner whose session
// this replica holds; a READY runner gets every status of its resources again.
func TestWorkloadStatusSweep_PushesWhatMovedToItsOnlineRunner(t *testing.T) {
	srv, f := newWorkloadSweepServer(t)
	online, offline := uuid.New(), uuid.New()
	srv.cfg.RunnerOnline = func(id uuid.UUID) bool { return id == online }
	fake := runnertest.NewFake()
	attachRunner(t, srv, online, fake)

	moved := k8sView("moved", online, types.RunRunning)
	same := k8sView("same", online, types.RunRunning)
	same.StatusPushed = workloadStatusHash(workloadStatusOf(same))
	away := k8sView("away", offline, types.RunRunning)
	f.views = []store.WorkloadView{moved, same, away}

	if err := srv.sweepWorkloadStatuses(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := fake.WorkloadStatuses()
	if len(got) != 1 || got[0] != workloadStatusOf(moved) {
		t.Fatalf("the sweep sent %+v, want only %q's status", got, moved.Name)
	}
	if f.pushed[moved.ID] != workloadStatusHash(got[0]) {
		t.Fatalf("pushed hash %q, want the hash of what was sent", f.pushed[moved.ID])
	}
	if _, ok := f.pushed[away.ID]; ok {
		t.Fatal("a status for a runner held elsewhere was recorded as sent")
	}

	srv.pushRunnerWorkloadStatuses(context.Background(), online)
	if got := fake.WorkloadStatuses(); len(got) != 3 || got[1].Name != "moved" || got[2].Name != "same" {
		t.Fatalf("after READY the runner holds %+v, want both of its statuses sent again", got)
	}
}
