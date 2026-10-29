// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && k8s

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// workloadEvents collects what a watcher sends as a runner's session would.
type workloadEvents struct {
	ch chan runnerwire.WorkloadReport
}

func newWorkloadEvents() *workloadEvents {
	return &workloadEvents{ch: make(chan runnerwire.WorkloadReport, 64)}
}

func (e *workloadEvents) Event(_ context.Context, ev runnerwire.Event) error {
	var rep runnerwire.WorkloadReport
	if ev.Kind != runnerwire.EventWorkload {
		return fmt.Errorf("event kind %q, want %q", ev.Kind, runnerwire.EventWorkload)
	}
	if err := json.Unmarshal(ev.Data, &rep); err != nil {
		return err
	}
	e.ch <- rep
	return nil
}

func (e *workloadEvents) next(t *testing.T) runnerwire.WorkloadReport {
	t.Helper()
	select {
	case rep := <-e.ch:
		return rep
	case <-time.After(10 * time.Second):
		t.Fatal("no workload report within 10s")
		return runnerwire.WorkloadReport{}
	}
}

func (e *workloadEvents) none(t *testing.T) {
	t.Helper()
	select {
	case rep := <-e.ch:
		t.Fatalf("unexpected workload report %+v", rep)
	case <-time.After(300 * time.Millisecond):
	}
}

func workloadCR(ns, name string, generation int64, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "wardyn.dev/v1alpha1", "kind": "WardynWorkload",
		"metadata": map[string]any{"name": name, "namespace": ns, "uid": "uid-" + name, "generation": generation},
		"spec":     spec,
	}}
}

func intentSpec(command string) map[string]any {
	return map[string]any{"intent": map[string]any{"experience": "background", "workload": map[string]any{"kind": "command", "command": command}}}
}

func fakeWorkloadClient(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{workloadGVR: "WardynWorkloadList"}, objs...)
}

// startWorkloadWatcher runs a watcher over namespaces until the test ends; peer, when set, is its session.
func startWorkloadWatcher(t *testing.T, client *dynamicfake.FakeDynamicClient, resync time.Duration, peer workloadPeer, namespaces ...string) *workloadWatcher {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w, err := newWorkloadWatcher(ctx, client, namespaces, resync)
	if err != nil {
		t.Fatal(err)
	}
	if peer != nil {
		w.setPeer(peer)
	}
	go w.run(ctx)
	return w
}

func waitWorkload(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestWorkloadWatcherReportsEverythingAfterSync: once the informer has listed the namespace, each
// resource is applied (spec as written) and one sync names them all. A resource whose spec the wire
// cannot carry is not applied but is still named, so the organisation does not delete it. A
// digit-led name, which Kubernetes allows and no workload can have, is left out of both, so the sync
// still goes.
func TestWorkloadWatcherReportsEverythingAfterSync(t *testing.T) {
	huge := map[string]any{"intent": strings.Repeat("x", runnerwire.MaxWorkloadSpecBytes+1)}
	events := newWorkloadEvents()
	startWorkloadWatcher(t, fakeWorkloadClient(
		workloadCR("team-a", "beta", 2, intentSpec("./beta")),
		workloadCR("team-a", "alpha", 1, intentSpec("./alpha")),
		workloadCR("team-a", "huge", 1, huge),
		workloadCR("team-a", "1web", 1, intentSpec("./web")),
		workloadCR("team-b", "elsewhere", 1, intentSpec("./x")),
	), time.Hour, events, "team-a")

	a := events.next(t)
	if a.Op != runnerwire.WorkloadOpApply || a.Namespace != "team-a" || a.Name != "alpha" || a.UID != "uid-alpha" || a.Generation != 1 || !strings.Contains(string(a.Spec), `"command":"./alpha"`) {
		t.Fatalf("first report %+v, want alpha's apply", a)
	}
	if b := events.next(t); b.Op != runnerwire.WorkloadOpApply || b.Name != "beta" || b.Generation != 2 {
		t.Fatalf("second report %+v, want beta's apply", b)
	}
	if s := events.next(t); s.Op != runnerwire.WorkloadOpSync || s.Namespace != "team-a" || strings.Join(s.Names, ",") != "alpha,beta,huge" {
		t.Fatalf("third report %+v, want a sync of alpha, beta, huge", s)
	}
	events.none(t)
}

// TestWorkloadWatcherLiveEvents: an add and a changed generation apply, a status-only update says
// nothing, and a delete carries the uid.
func TestWorkloadWatcherLiveEvents(t *testing.T) {
	client := fakeWorkloadClient()
	events := newWorkloadEvents()
	startWorkloadWatcher(t, client, time.Hour, events, "team-a")
	if s := events.next(t); s.Op != runnerwire.WorkloadOpSync || len(s.Names) != 0 {
		t.Fatalf("empty namespace: %+v, want an empty sync", s)
	}
	crs := client.Resource(workloadGVR).Namespace("team-a")
	ctx := context.Background()

	if _, err := crs.Create(ctx, workloadCR("team-a", "bot", 1, intentSpec("./one")), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if a := events.next(t); a.Op != runnerwire.WorkloadOpApply || a.Name != "bot" || a.Generation != 1 {
		t.Fatalf("add: %+v", a)
	}

	withStatus := workloadCR("team-a", "bot", 1, intentSpec("./one"))
	withStatus.Object["status"] = map[string]any{"phase": "Running"}
	if _, err := crs.Update(ctx, withStatus, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	events.none(t)

	if _, err := crs.Update(ctx, workloadCR("team-a", "bot", 2, intentSpec("./two")), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if a := events.next(t); a.Op != runnerwire.WorkloadOpApply || a.Generation != 2 || !strings.Contains(string(a.Spec), "./two") {
		t.Fatalf("generation change: %+v", a)
	}

	if err := crs.Delete(ctx, "bot", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if d := events.next(t); d.Op != runnerwire.WorkloadOpDelete || d.Name != "bot" || d.UID != "uid-bot" || d.Generation != 0 || len(d.Spec) != 0 {
		t.Fatalf("delete: %+v", d)
	}
}

// TestWorkloadWatcherOfflineThenOnline: what the informer saw with no session up is dropped, and the
// session coming up brings a full report of the store.
func TestWorkloadWatcherOfflineThenOnline(t *testing.T) {
	w := startWorkloadWatcher(t, fakeWorkloadClient(workloadCR("team-a", "bot", 1, intentSpec("./one"))), time.Hour, nil, "team-a")
	waitWorkload(t, "the informer to sync", func() bool { return w.synced["team-a"].Load() })
	events := newWorkloadEvents()
	w.setPeer(events)
	if a := events.next(t); a.Op != runnerwire.WorkloadOpApply || a.Name != "bot" {
		t.Fatalf("on connect: %+v, want bot's apply", a)
	}
	if s := events.next(t); s.Op != runnerwire.WorkloadOpSync || strings.Join(s.Names, ",") != "bot" {
		t.Fatalf("on connect: %+v, want a sync of bot", s)
	}

	w.dropPeer(events)
	if w.currentPeer() != nil {
		t.Fatal("the session is still held after it went offline")
	}
	w.dropPeer(newWorkloadEvents()) // another session's end leaves the current one alone
	w.setPeer(events)
	w.dropPeer(newWorkloadEvents())
	if w.currentPeer() == nil {
		t.Fatal("a different session's end dropped the current one")
	}
}

// TestWorkloadWatcherResyncs: every resync reports the namespace in full again.
func TestWorkloadWatcherResyncs(t *testing.T) {
	events := newWorkloadEvents()
	startWorkloadWatcher(t, fakeWorkloadClient(workloadCR("team-a", "bot", 1, intentSpec("./one"))), 50*time.Millisecond, events, "team-a")
	for round := 0; round < 3; round++ {
		if a := events.next(t); a.Op != runnerwire.WorkloadOpApply || a.Name != "bot" {
			t.Fatalf("round %d: %+v, want bot's apply", round, a)
		}
		if s := events.next(t); s.Op != runnerwire.WorkloadOpSync {
			t.Fatalf("round %d: %+v, want a sync", round, s)
		}
	}
}

// TestWorkloadWatcherStatus: the organisation's answer lands on the status subresource; Ready moves
// its transition time only when it flips; an unwatched namespace is refused and a deleted resource is
// not an error.
func TestWorkloadWatcherStatus(t *testing.T) {
	client := fakeWorkloadClient(workloadCR("team-a", "bot", 3, intentSpec("./one")))
	w := startWorkloadWatcher(t, client, time.Hour, nil, "team-a")
	waitWorkload(t, "the informer to sync", func() bool { return w.synced["team-a"].Load() })
	clock := time.Now().UTC().Truncate(time.Second)
	first, later := clock.Format(time.RFC3339), clock.Add(time.Hour).Format(time.RFC3339)
	w.now = func() time.Time { return clock }
	ctx := context.Background()
	answer := runnerwire.WorkloadStatusArgs{Namespace: "team-a", Name: "bot", UID: "uid-bot", ObservedGeneration: 3, Phase: runnerwire.WorkloadPhaseRunning, RunID: "5f0c"}

	read := func() map[string]any {
		got, err := client.Resource(workloadGVR).Namespace("team-a").Get(ctx, "bot", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		st, _ := got.Object["status"].(map[string]any)
		return st
	}
	cond := func(st map[string]any) map[string]any {
		cs, _ := st["conditions"].([]any)
		if len(cs) != 1 {
			t.Fatalf("conditions %v, want exactly Ready", st["conditions"])
		}
		return cs[0].(map[string]any)
	}
	if err := w.status(ctx, answer); err != nil {
		t.Fatal(err)
	}
	st := read()
	if st["phase"] != "Running" || st["runID"] != "5f0c" || fmt.Sprint(st["observedGeneration"]) != "3" {
		t.Fatalf("status %v", st)
	}
	if c := cond(st); c["type"] != "Ready" || c["status"] != "True" || c["lastTransitionTime"] != first {
		t.Fatalf("condition %v, want Ready True at the first answer", c)
	}

	// The same answer later keeps the transition time; Unhealthy flips Ready and moves it.
	waitWorkload(t, "the informer to see the status", func() bool {
		o, ok, _ := w.informers["team-a"].GetStore().GetByKey("team-a/bot")
		if !ok {
			return false
		}
		s, _ := readyCondition(o.(*unstructured.Unstructured))
		return s == "True"
	})
	clock = clock.Add(time.Hour)
	if err := w.status(ctx, answer); err != nil {
		t.Fatal(err)
	}
	if c := cond(read()); c["lastTransitionTime"] != first {
		t.Fatalf("condition %v, want the transition time kept", c)
	}
	answer.Phase, answer.Reason, answer.Message = runnerwire.WorkloadPhaseUnhealthy, "health_failed", "probe refused"
	if err := w.status(ctx, answer); err != nil {
		t.Fatal(err)
	}
	st = read()
	if c := cond(st); c["status"] != "False" || c["reason"] != "Unhealthy" || c["lastTransitionTime"] != later || st["reason"] != "health_failed" || st["message"] != "probe refused" {
		t.Fatalf("unhealthy: status %v", st)
	}

	other := answer
	other.Namespace = "team-b"
	if err := w.status(ctx, other); err == nil {
		t.Fatal("a status for a namespace this runner does not watch was written")
	}
	gone := answer
	gone.Name = "deleted"
	if err := w.status(ctx, gone); err != nil {
		t.Fatalf("a deleted resource: %v, want no error", err)
	}
}

// TestWorkloadWatcherStatusOnlyForItsResource: an answer is written only to the resource it names, by
// uid and generation, at the resourceVersion it was checked at; a resource recreated under the name, a
// newer spec, a delete before the write, or a write that meets a change are no error storm.
func TestWorkloadWatcherStatusOnlyForItsResource(t *testing.T) {
	cr := workloadCR("team-a", "bot", 2, intentSpec("./one"))
	cr.SetResourceVersion("7")
	client := fakeWorkloadClient(cr)
	var patches []map[string]any
	var patchErrs []error // answered, in order, before the tracker sees a patch
	client.PrependReactor("patch", "wardynworkloads", func(a k8stesting.Action) (bool, runtime.Object, error) {
		var body map[string]any
		_ = json.Unmarshal(a.(k8stesting.PatchAction).GetPatch(), &body)
		patches = append(patches, body)
		if len(patchErrs) > 0 {
			err := patchErrs[0]
			patchErrs = patchErrs[1:]
			return true, nil, err
		}
		return false, nil, nil
	})
	w := startWorkloadWatcher(t, client, time.Hour, nil, "team-a")
	waitWorkload(t, "the informer to sync", func() bool { return w.synced["team-a"].Load() })
	ctx := context.Background()
	status := func() map[string]any {
		got, err := client.Resource(workloadGVR).Namespace("team-a").Get(ctx, "bot", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		st, _ := got.Object["status"].(map[string]any)
		return st
	}
	answer := runnerwire.WorkloadStatusArgs{Namespace: "team-a", Name: "bot", UID: "uid-bot", ObservedGeneration: 2, Phase: runnerwire.WorkloadPhaseRunning, RunID: "run-b"}

	replaced := answer
	replaced.UID, replaced.ObservedGeneration, replaced.RunID = "uid-old", 2, "run-a"
	stale := answer
	stale.ObservedGeneration = 1
	for name, a := range map[string]runnerwire.WorkloadStatusArgs{"a resource recreated under the name": replaced, "an older generation": stale} {
		if err := w.status(ctx, a); err != nil {
			t.Fatalf("%s: %v, want dropped without an error", name, err)
		}
	}
	if st := status(); st != nil || len(patches) != 0 {
		t.Fatalf("a stale answer was written: status %v, %d patches", st, len(patches))
	}

	if err := w.status(ctx, answer); err != nil {
		t.Fatal(err)
	}
	if st := status(); st["phase"] != "Running" || st["runID"] != "run-b" {
		t.Fatalf("the matching answer: status %v", st)
	}
	md, _ := patches[0]["metadata"].(map[string]any)
	if len(patches) != 1 || md["uid"] != "uid-bot" || md["resourceVersion"] != "7" {
		t.Fatalf("patches %v, want one carrying uid-bot at resourceVersion 7", patches)
	}

	// A write that meets a change is read again and retried; one whose resource went is no error.
	patchErrs = []error{apierrors.NewConflict(workloadGVR.GroupResource(), "bot", fmt.Errorf("changed"))}
	answer.Phase = runnerwire.WorkloadPhaseUnhealthy
	if err := w.status(ctx, answer); err != nil || len(patches) != 3 || status()["phase"] != "Unhealthy" {
		t.Fatalf("after a conflict: %v, %d patches, status %v; want it retried once and written", err, len(patches), status())
	}
	patchErrs = []error{apierrors.NewNotFound(workloadGVR.GroupResource(), "bot")}
	if err := w.status(ctx, answer); err != nil {
		t.Fatalf("a resource deleted between the read and the write: %v, want no error", err)
	}
	// One that loses every attempt is an error, so the org sends it again.
	for range workloadStatusAttempts {
		patchErrs = append(patchErrs, apierrors.NewConflict(workloadGVR.GroupResource(), "bot", fmt.Errorf("changed")))
	}
	answer.Phase = runnerwire.WorkloadPhaseRunning
	if err := w.status(ctx, answer); err == nil || status()["phase"] != "Unhealthy" {
		t.Fatalf("after %d conflicts: %v, status %v; want an error and the status unwritten", workloadStatusAttempts, err, status())
	}
}

// TestWorkloadWatcherRefusesMissingCRD: a listed namespace whose WardynWorkload resource the API server
// does not know refuses boot naming the CRD and its file; any other list error refuses too.
func TestWorkloadWatcherRefusesMissingCRD(t *testing.T) {
	client := fakeWorkloadClient()
	client.PrependReactor("list", "wardynworkloads", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(workloadGVR.GroupResource(), "")
	})
	_, err := newWorkloadWatcher(context.Background(), client, []string{"team-a"}, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "wardynworkloads.wardyn.dev") || !strings.Contains(err.Error(), "crds/wardyn.dev_wardynworkloads.yaml") {
		t.Fatalf("missing CRD: %v, want a refusal naming wardynworkloads.wardyn.dev and the chart's crds file", err)
	}

	client = fakeWorkloadClient()
	client.PrependReactor("list", "wardynworkloads", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(workloadGVR.GroupResource(), "", fmt.Errorf("no grant"))
	})
	if _, err = newWorkloadWatcher(context.Background(), client, []string{"team-a"}, time.Hour); err == nil || !strings.Contains(err.Error(), "team-a") {
		t.Fatalf("forbidden list: %v, want a refusal naming the namespace", err)
	}
}

// TestWorkloadWatcherAttach: the session hooks hand the watcher its peer and the runtime still sees
// both; with no namespaces there is no watcher and the runtime is untouched.
func TestWorkloadWatcherAttach(t *testing.T) {
	var online, offline int
	rt := runnerRuntime{hooks: runnerwire.RunnerHooks{
		Online:        func(*runnerwire.Peer, bool) { online++ },
		StreamOffline: func(*runnerwire.Peer) { offline++ },
	}, run: func(context.Context) error { return nil }}
	if off, err := newClusterWorkloadWatcher(context.Background(), nil); off != nil || err != nil || off.statusFunc() != nil || off.attach(rt).run == nil {
		t.Fatalf("no namespaces: watcher %v, err %v, want none", off, err)
	}

	w, err := newWorkloadWatcher(context.Background(), fakeWorkloadClient(), []string{"team-a"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if w.statusFunc() == nil {
		t.Fatal("a watcher with namespaces has no status adapter")
	}
	rt = w.attach(rt)
	a, b := runnerwire.Loopback()
	defer b.Close()
	peer := runnerwire.NewPeer(a, runnerwire.PeerConfig{})
	defer peer.Close()
	rt.hooks.Online(peer, false)
	if online != 1 || w.currentPeer() == nil {
		t.Fatalf("online hook ran %d times, peer held %v", online, w.currentPeer() != nil)
	}
	rt.hooks.StreamOffline(peer)
	if offline != 1 || w.currentPeer() != nil {
		t.Fatalf("offline hook ran %d times, peer held %v", offline, w.currentPeer() != nil)
	}
}
