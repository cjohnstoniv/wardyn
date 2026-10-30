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
	"github.com/cjohnstoniv/wardyn/pkg/client"
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
		SourceNamespace: "team-a", SourceRunnerID: &runner, K8sGeneration: &gen, K8sUID: "uid-" + name, Generation: 2, CurrentGeneration: 2, CurrentRunID: &run},
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
		if err := srv.WorkloadFromRunner(context.Background(), uuid.New(), "sess-1", ev); !errors.Is(err, errRunnerEventShape) {
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
		if a.ObservedGeneration != 4 || a.Namespace != "team-a" || a.Name != "web" || a.UID != "uid-web" {
			t.Errorf("%s: status %+v, want team-a/web (uid-web) at observed generation 4", tc.name, a)
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
	unnamed := k8sView("unnamed", online, types.RunRunning)
	unnamed.K8sUID = "" // recorded before the uid column: waits for its resource's next report
	f.views = []store.WorkloadView{moved, same, away, unnamed}

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
	if _, ok := f.pushed[unnamed.ID]; ok {
		t.Fatal("a status with no resource uid was recorded as sent")
	}

	srv.pushRunnerWorkloadStatuses(context.Background(), online)
	if got := fake.WorkloadStatuses(); len(got) != 3 || got[1].Name != "moved" || got[2].Name != "same" {
		t.Fatalf("after READY the runner holds %+v, want both of its statuses sent again", got)
	}
}

// standingFaultStore is the in-memory store whose identity check cannot be answered, with one launch claim
// that ClaimWorkloadLaunch takes and then holds until released; every other workload call is unset and would
// panic.
type standingFaultStore struct {
	*authzStore
	store.WorkloadStore

	mu            sync.Mutex
	held          bool
	claims, reads int
}

func (f *standingFaultStore) ClaimWorkloadLaunch(_ context.Context, id uuid.UUID) (store.Workload, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims++
	if f.held {
		return store.Workload{}, false, nil
	}
	f.held = true
	return store.Workload{ID: id, Owner: "service:ci-bot", Name: "web", Source: store.WorkloadSourceK8s, Generation: 2,
		CurrentGeneration: 1, ClaimToken: uuid.New()}, true, nil
}

func (f *standingFaultStore) IdentityBlocked(context.Context, string, int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return false, errors.New("connection refused")
}

// A launch whose binding's standing cannot be read takes its claim first, so the lapsed-launch sweep's
// passes while the claim is held read nothing: the standing is read once a claim, as the CHANGELOG bounds it.
func TestLaunchUnderBinding_AnUnreadableStandingIsReadOnceAClaim(t *testing.T) {
	f := &standingFaultStore{authzStore: newAuthzStore()}
	srv := New(baseTestConfig(newHarness(t), f))
	si := types.ServiceIdentity{Name: "ci-bot", Sponsor: "sponsor-sub"}
	b := store.WorkloadBinding{ID: uuid.New(), PoolID: uuid.New(), Namespace: "team-a", Service: si.Name, Sponsor: si.Sponsor}
	wl := store.Workload{ID: uuid.New(), Owner: "service:ci-bot", Name: "web", Source: store.WorkloadSourceK8s, Generation: 2, CurrentGeneration: 1}
	sr, cancel, err := serviceLaunchRequest(si, b.Sponsor)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	if _, err := srv.launchUnderBinding(sr, f, b, true, si, wl); err == nil {
		t.Fatal("a launch whose standing cannot be read succeeded")
	}
	for pass := 0; pass < 3; pass++ { // the sweep's passes while the claim is still held
		if launch, err := srv.launchUnderBinding(sr, f, b, true, si, wl); err != nil || launch.State != client.WorkloadNoLaunch {
			t.Fatalf("pass %d under a held claim = %+v, %v; want no launch and no error", pass, launch, err)
		}
	}
	if f.claims != 4 || f.reads != 1 {
		t.Fatalf("claims/standing reads = %d/%d, want 4/1: one read for the one claim taken", f.claims, f.reads)
	}
	f.held = false // the claim lapses: the next retry takes it and reads the standing again
	if _, err := srv.launchUnderBinding(sr, f, b, true, si, wl); err == nil || f.reads != 2 {
		t.Fatalf("after the claim lapsed: err %v, %d standing reads; want the error and a second read", err, f.reads)
	}
}
