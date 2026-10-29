// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func workloadPut(owner, name, digest string) store.WorkloadPut {
	return store.WorkloadPut{Owner: owner, Name: name, Source: store.WorkloadSourceAPI, Intent: json.RawMessage(`{"experience":"background"}`), Digest: digest, By: owner}
}

// mustClaim takes the workload's launch claim and returns its token.
func mustClaim(t *testing.T, st store.PG, id uuid.UUID) uuid.UUID {
	t.Helper()
	w, ok, err := st.ClaimWorkloadLaunch(t.Context(), id)
	if err != nil || !ok || w.ClaimToken == uuid.Nil {
		t.Fatalf("claim workload %s = %+v, %v, %v; want a claim", id, w, ok, err)
	}
	return w.ClaimToken
}

func mustPutWorkload(t *testing.T, st store.PG, in store.WorkloadPut) store.Workload {
	t.Helper()
	w, _, err := st.PutWorkload(t.Context(), in)
	if err != nil {
		t.Fatalf("put workload %s/%s: %v", in.Owner, in.Name, err)
	}
	return w
}

// An identical spec writes nothing but a changed description; a different digest moves the generation by
// one; the same name under another owner is another workload.
func TestPG_Workload_PutCreatesUpdatesAndLeavesUnchanged(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := t.Context()

	in := workloadPut("sub-a", "triage", "d1")
	in.Description = "first"
	in.RunIn = "platform"
	created, write, err := st.PutWorkload(ctx, in)
	if err != nil || write != store.WorkloadCreated {
		t.Fatalf("first put = %v, %v; want created", write, err)
	}
	if created.Generation != 1 || created.CurrentGeneration != 0 || created.CurrentRunID != nil || created.CreatedBy != "sub-a" ||
		created.Source != store.WorkloadSourceAPI || created.SourcePoolID != nil || created.SourceNamespace != "" || created.RunIn != "platform" {
		t.Fatalf("created row = %+v", created)
	}

	in.Description = "second"
	moved, write, err := st.PutWorkload(ctx, in)
	if err != nil || write != store.WorkloadUnchanged {
		t.Fatalf("description-only put = %v, %v; want unchanged", write, err)
	}
	if moved.ID != created.ID || moved.Generation != 1 || moved.Description != "second" || !moved.UpdatedAt.After(created.UpdatedAt) {
		t.Fatalf("description-only row = %+v (created %+v)", moved, created)
	}

	same, write, err := st.PutWorkload(ctx, in)
	if err != nil || write != store.WorkloadUnchanged || !same.UpdatedAt.Equal(moved.UpdatedAt) {
		t.Fatalf("identical put = %v, %v, updated_at %v want %v", write, err, same.UpdatedAt, moved.UpdatedAt)
	}

	in.Digest = "d2"
	in.Intent = json.RawMessage(`{"experience":"background","task":"two"}`)
	in.RestartKey = "1"
	changed, write, err := st.PutWorkload(ctx, in)
	if err != nil || write != store.WorkloadUpdated {
		t.Fatalf("changed put = %v, %v; want updated", write, err)
	}
	if changed.ID != created.ID || changed.Generation != 2 || changed.Digest != "d2" || changed.RestartKey != "1" {
		t.Fatalf("changed row = %+v", changed)
	}
	var intent map[string]string
	if err := json.Unmarshal(changed.Intent, &intent); err != nil || intent["task"] != "two" {
		t.Fatalf("intent = %s (%v), want the new one", changed.Intent, err)
	}

	other := mustPutWorkload(t, st, workloadPut("sub-b", "triage", "d1"))
	if other.ID == created.ID || other.Generation != 1 {
		t.Fatalf("another owner's workload = %+v", other)
	}
}

// Racing applies of one new spec leave one created row at generation 1, and exactly one of them wins the
// launch claim.
func TestPG_Workload_ConcurrentApplyLaunchesOnce(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := t.Context()

	const n = 8
	var created, claimed atomic.Int32
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w, write, err := st.PutWorkload(ctx, workloadPut("sub-a", "svc", "d1"))
			if err != nil {
				errs <- err
				return
			}
			if write == store.WorkloadCreated {
				created.Add(1)
			}
			_, ok, err := st.ClaimWorkloadLaunch(ctx, w.ID)
			if err != nil {
				errs <- err
				return
			}
			if ok {
				claimed.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if created.Load() != 1 || claimed.Load() != 1 {
		t.Fatalf("%d creates and %d claims, want one of each", created.Load(), claimed.Load())
	}
	got, err := st.GetWorkload(ctx, "sub-a", "svc")
	if err != nil || got.Generation != 1 {
		t.Fatalf("workload = %+v (%v), want generation 1", got.Workload, err)
	}
}

// A claim holds for its window, a crashed claimer's lapses, and a recorded launch or refusal releases it.
func TestPG_Workload_ClaimHoldsThenLapses(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := t.Context()
	w := mustPutWorkload(t, st, workloadPut("sub-a", "svc", "d1"))

	claimed, ok, err := st.ClaimWorkloadLaunch(ctx, w.ID)
	if err != nil || !ok || claimed.Generation != 1 || claimed.LaunchClaimUntil == nil || !claimed.LaunchClaimUntil.After(claimed.UpdatedAt) {
		t.Fatalf("first claim = %+v, %v, %v", claimed, ok, err)
	}
	if _, ok, err := st.ClaimWorkloadLaunch(ctx, w.ID); err != nil || ok {
		t.Fatalf("second claim = %v, %v; want held", ok, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workloads SET launch_claim_until = now() - interval '1 second' WHERE id = $1`, w.ID); err != nil {
		t.Fatal(err)
	}
	retaken, ok, err := st.ClaimWorkloadLaunch(ctx, w.ID)
	if err != nil || !ok || retaken.ClaimToken == claimed.ClaimToken {
		t.Fatalf("claim after the lapse = %v, %v; want taken under a new token", ok, err)
	}
	if err := st.RecordWorkloadRefusal(ctx, w.ID, retaken.ClaimToken, store.WorkloadRefusal{Generation: 1, Reason: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.ClaimWorkloadLaunch(ctx, w.ID); err != nil || !ok {
		t.Fatalf("claim after a refusal = %v, %v; want taken", ok, err)
	}
	if _, ok, err := st.ClaimWorkloadLaunch(ctx, uuid.New()); err != nil || ok {
		t.Fatalf("claim of an unknown workload = %v, %v; want none", ok, err)
	}
}

// A run is recorded against a generation: the previous run comes back to be ended, a claim is owed only
// while the spec is ahead of its run, and a refusal keeps the old run.
func TestPG_Workload_RunRefusalAndRestart(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := t.Context()
	run := func() uuid.UUID { return persistRun(t, ctx, pool, newRun(types.RunRunning)).ID }
	run1, run2, run3, run4 := run(), run(), run(), run()

	w := mustPutWorkload(t, st, workloadPut("sub-a", "svc", "d1"))
	claim := mustClaim(t, st, w.ID)
	if prev, err := st.SetWorkloadRun(ctx, w.ID, claim, 1, run1); err != nil || prev != nil {
		t.Fatalf("first run recorded = %v, %v; want no previous run", prev, err)
	}
	if _, ok, err := st.ClaimWorkloadLaunch(ctx, w.ID); err != nil || ok {
		t.Fatalf("claim after the launch = %v, %v; want none owed", ok, err)
	}
	if v, err := st.GetWorkload(ctx, "sub-a", "svc"); err != nil || v.CurrentRunID == nil || *v.CurrentRunID != run1 || v.CurrentGeneration != 1 || v.RunState != types.RunRunning {
		t.Fatalf("after the launch = %+v (%v)", v, err)
	}

	// A changed spec: the claim comes back, and recording the new run hands back the old one.
	next := workloadPut("sub-a", "svc", "d2")
	mustPutWorkload(t, st, next)
	claim = mustClaim(t, st, w.ID)
	if prev, err := st.SetWorkloadRun(ctx, w.ID, claim, 2, run2); err != nil || prev == nil || *prev != run1 {
		t.Fatalf("second run recorded = %v, %v; want %s back", prev, err, run1)
	}
	for _, gen := range []int64{2, 1} {
		if _, err := st.SetWorkloadRun(ctx, w.ID, claim, gen, run3); !errors.Is(err, store.ErrWorkloadSuperseded) || !errors.Is(err, store.ErrConflict) {
			t.Fatalf("run recorded again for generation %d = %v, want ErrWorkloadSuperseded", gen, err)
		}
	}
	if _, err := st.SetWorkloadRun(ctx, uuid.New(), claim, 1, run3); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("run recorded for an unknown workload = %v, want ErrNotFound", err)
	}

	// A refused third generation keeps the running second and leaves the launch owed.
	next.Digest = "d3"
	mustPutWorkload(t, st, next)
	claim = mustClaim(t, st, w.ID)
	at := store.WorkloadRefusal{Generation: 3, Reason: "ceiling_refused", Message: "no"}
	if err := st.RecordWorkloadRefusal(ctx, w.ID, claim, at); err != nil {
		t.Fatal(err)
	}
	v, err := st.GetWorkload(ctx, "sub-a", "svc")
	if err != nil || v.CurrentRunID == nil || *v.CurrentRunID != run2 || v.CurrentGeneration != 2 || v.LaunchClaimUntil != nil ||
		v.LastRefusal == nil || v.LastRefusal.Generation != 3 || v.LastRefusal.Reason != "ceiling_refused" || v.LastRefusal.Message != "no" {
		t.Fatalf("after the refusal = %+v (%v)", v, err)
	}
	if err := st.RecordWorkloadRefusal(ctx, uuid.New(), claim, at); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("refusal of an unknown workload = %v, want ErrNotFound", err)
	}
	claim = mustClaim(t, st, w.ID)
	if prev, err := st.SetWorkloadRun(ctx, w.ID, claim, 3, run3); err != nil || prev == nil || *prev != run2 {
		t.Fatalf("third run recorded = %v, %v; want %s back", prev, err, run2)
	}
	if v, err = st.GetWorkload(ctx, "sub-a", "svc"); err != nil || v.LastRefusal != nil || v.CurrentGeneration != 3 {
		t.Fatalf("after the third launch = %+v (%v), want the refusal cleared", v, err)
	}

	// A restart owes a launch of the same spec; the run it replaces comes back.
	r, err := st.RestartWorkload(ctx, w.ID)
	if err != nil || r.CurrentGeneration != 0 || r.Generation != 3 || r.CurrentRunID == nil || *r.CurrentRunID != run3 {
		t.Fatalf("restart = %+v (%v)", r, err)
	}
	claim = mustClaim(t, st, w.ID)
	if prev, err := st.SetWorkloadRun(ctx, w.ID, claim, 3, run4); err != nil || prev == nil || *prev != run3 {
		t.Fatalf("restarted run recorded = %v, %v; want %s back", prev, err, run3)
	}
	if _, err := st.RestartWorkload(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("restart of an unknown workload = %v, want ErrNotFound", err)
	}

	// A deleted run row leaves the workload without a run.
	if _, err := pool.Exec(ctx, `DELETE FROM agent_runs WHERE id = $1`, run4); err != nil {
		t.Fatal(err)
	}
	if v, err = st.GetWorkload(ctx, "sub-a", "svc"); err != nil || v.CurrentRunID != nil || v.RunState != "" {
		t.Fatalf("after the run row is gone = %+v (%v)", v, err)
	}
}

// A claimer whose claim lapsed and was taken again records nothing: neither a refusal over the newer
// claim, nor one after the newer claimer's run, nor a run of its own.
func TestPG_Workload_StaleClaimRecordsNothing(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := t.Context()
	w := mustPutWorkload(t, st, workloadPut("sub-a", "svc", "d1"))
	lapse := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE workloads SET launch_claim_until = now() - interval '1 second' WHERE id = $1`, w.ID); err != nil {
			t.Fatal(err)
		}
	}
	refusal := store.WorkloadRefusal{Generation: 1, Reason: "ceiling_refused"}

	a := mustClaim(t, st, w.ID)
	lapse()
	b := mustClaim(t, st, w.ID)
	if err := st.RecordWorkloadRefusal(ctx, w.ID, a, refusal); !errors.Is(err, store.ErrWorkloadSuperseded) {
		t.Fatalf("stale refusal = %v, want ErrWorkloadSuperseded", err)
	}
	v, err := st.GetWorkload(ctx, "sub-a", "svc")
	if err != nil || v.LaunchClaimUntil == nil || v.LastRefusal != nil {
		t.Fatalf("after the stale refusal = %+v (%v), want the newer claim held and no refusal", v.Workload, err)
	}
	if _, ok, err := st.ClaimWorkloadLaunch(ctx, w.ID); err != nil || ok {
		t.Fatalf("claim while the newer one holds = %v, %v; want held", ok, err)
	}
	if _, err := st.SetWorkloadRun(ctx, w.ID, a, 1, persistRun(t, ctx, pool, newRun(types.RunRunning)).ID); !errors.Is(err, store.ErrWorkloadSuperseded) {
		t.Fatalf("stale run = %v, want ErrWorkloadSuperseded", err)
	}

	run := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID
	if _, err := st.SetWorkloadRun(ctx, w.ID, b, 1, run); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordWorkloadRefusal(ctx, w.ID, a, refusal); !errors.Is(err, store.ErrWorkloadSuperseded) {
		t.Fatalf("stale refusal after the newer run = %v, want ErrWorkloadSuperseded", err)
	}
	if v, err = st.GetWorkload(ctx, "sub-a", "svc"); err != nil || v.LastRefusal != nil || v.CurrentRunID == nil || *v.CurrentRunID != run || v.CurrentGeneration != 1 {
		t.Fatalf("after the newer run = %+v (%v), want its run and no refusal", v.Workload, err)
	}
}

// A late report of a resource at a lower metadata.generation than stored (a runner's cache behind
// another's) writes nothing, even with another spec; a new uid at a lower generation is a new resource.
func TestPG_Workload_LowerK8sGenerationIsIgnored(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := t.Context()
	poolA := ownPool(t, st, "sub-a", "A")
	runnerA, runnerB := uuid.New(), uuid.New()
	gen1, gen2 := int64(1), int64(2)

	in := workloadPut("service:bot", "svc", "d2")
	in.Intent = json.RawMessage(`{"experience":"background","task":"gen2"}`)
	in.Source, in.SourcePoolID, in.SourceNamespace, in.SourceRunnerID, in.K8sGeneration, in.K8sUID =
		store.WorkloadSourceK8s, &poolA.ID, "team-a", &runnerA, &gen2, "uid-1"
	w := mustPutWorkload(t, st, in)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID
	if _, err := st.SetWorkloadRun(ctx, w.ID, mustClaim(t, st, w.ID), 1, run); err != nil {
		t.Fatal(err)
	}

	late := in
	late.Intent, late.Digest, late.SourceRunnerID, late.K8sGeneration = json.RawMessage(`{"experience":"background","task":"gen1"}`), "d1", &runnerB, &gen1
	got, write, err := st.PutWorkload(ctx, late)
	if err != nil || write != store.WorkloadUnchanged || got.ID != w.ID || got.Digest != "d2" {
		t.Fatalf("late put = %+v, %v, %v; want the stored row back unchanged", got, write, err)
	}
	v, err := st.GetWorkload(ctx, "service:bot", "svc")
	if err != nil || v.Digest != "d2" || string(v.Intent) != string(w.Intent) || v.Generation != 1 || v.K8sGeneration == nil || *v.K8sGeneration != 2 ||
		v.SourceRunnerID == nil || *v.SourceRunnerID != runnerA || v.CurrentRunID == nil || *v.CurrentRunID != run || v.CurrentGeneration != 1 ||
		!v.UpdatedAt.Equal(w.UpdatedAt) {
		t.Fatalf("after the late put = %+v (%v), want the generation 2 row and its run untouched", v.Workload, err)
	}

	// The same uid at the same metadata.generation is still answered (a re-report), and a new uid is
	// accepted at any generation.
	if _, write, err := st.PutWorkload(ctx, in); err != nil || write != store.WorkloadUnchanged {
		t.Fatalf("re-report at the stored generation = %v, %v", write, err)
	}
	late.K8sUID = "uid-2"
	got, write, err = st.PutWorkload(ctx, late)
	if err != nil || write != store.WorkloadUpdated || got.Digest != "d1" || got.Generation != 2 || got.K8sUID != "uid-2" || *got.K8sGeneration != 1 {
		t.Fatalf("new uid at generation 1 = %+v, %v, %v; want it recorded", got, write, err)
	}
}

// A custom resource's workload is its pool's and namespace's alone, and an API one is the API's alone.
func TestPG_Workload_SourceConflict(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := t.Context()
	poolA, poolB := ownPool(t, st, "sub-a", "A"), ownPool(t, st, "sub-a", "B")
	runner1, runner2 := uuid.New(), uuid.New()
	gen1, gen2 := int64(1), int64(2)

	k8s := workloadPut("service:bot", "svc", "d1")
	k8s.Source, k8s.SourcePoolID, k8s.SourceNamespace, k8s.SourceRunnerID, k8s.K8sGeneration = store.WorkloadSourceK8s, &poolA.ID, "team-a", &runner1, &gen1
	first := mustPutWorkload(t, st, k8s)
	if first.Source != store.WorkloadSourceK8s || first.SourcePoolID == nil || *first.SourcePoolID != poolA.ID || first.SourceNamespace != "team-a" ||
		first.SourceRunnerID == nil || *first.SourceRunnerID != runner1 || first.K8sGeneration == nil || *first.K8sGeneration != 1 {
		t.Fatalf("k8s row = %+v", first)
	}

	for name, mutate := range map[string]func(*store.WorkloadPut){
		"the API":           func(p *store.WorkloadPut) { *p = workloadPut(p.Owner, p.Name, "d9") },
		"another pool":      func(p *store.WorkloadPut) { p.SourcePoolID, p.Digest = &poolB.ID, "d9" },
		"another namespace": func(p *store.WorkloadPut) { p.SourceNamespace, p.Digest = "team-b", "d9" },
	} {
		in := k8s
		mutate(&in)
		if _, _, err := st.PutWorkload(ctx, in); !errors.Is(err, store.ErrWorkloadSourceConflict) || !errors.Is(err, store.ErrConflict) {
			t.Errorf("put from %s = %v, want ErrWorkloadSourceConflict", name, err)
		}
	}
	if v, err := st.GetWorkload(ctx, "service:bot", "svc"); err != nil || v.Digest != "d1" || v.Generation != 1 {
		t.Fatalf("after the refused writes = %+v (%v), want it untouched", v.Workload, err)
	}

	// The same resource reported by another runner, at a newer metadata.generation, is unchanged.
	k8s.SourceRunnerID, k8s.K8sGeneration = &runner2, &gen2
	again, write, err := st.PutWorkload(ctx, k8s)
	if err != nil || write != store.WorkloadUnchanged || again.SourceRunnerID == nil || *again.SourceRunnerID != runner2 || *again.K8sGeneration != 2 || again.Generation != 1 {
		t.Fatalf("re-report = %+v, %v, %v", again, write, err)
	}

	api := workloadPut("sub-a", "svc", "d1")
	mustPutWorkload(t, st, api)
	api.Source, api.SourcePoolID, api.SourceNamespace = store.WorkloadSourceK8s, &poolA.ID, "team-a"
	if _, _, err := st.PutWorkload(ctx, api); !errors.Is(err, store.ErrWorkloadSourceConflict) {
		t.Errorf("custom resource over an API workload = %v, want ErrWorkloadSourceConflict", err)
	}
}

// One owner holds at most MaxWorkloadsPerOwner; rewriting one at the cap, and another owner, still work.
func TestPG_Workload_CapPerOwner(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `INSERT INTO workloads (owner, name, source, intent, digest, created_by)
		SELECT 'sub-a', 'w' || g, 'api', '{}', 'x', 'sub-a' FROM generate_series(1, $1::int) g`, store.MaxWorkloadsPerOwner-1); err != nil {
		t.Fatal(err)
	}
	if _, write, err := st.PutWorkload(ctx, workloadPut("sub-a", "last", "d1")); err != nil || write != store.WorkloadCreated {
		t.Fatalf("the workload that reaches the cap = %v, %v", write, err)
	}
	if _, _, err := st.PutWorkload(ctx, workloadPut("sub-a", "over", "d1")); !errors.Is(err, store.ErrWorkloadCapReached) {
		t.Fatalf("workload over the cap = %v, want ErrWorkloadCapReached", err)
	}
	if _, write, err := st.PutWorkload(ctx, workloadPut("sub-a", "last", "d2")); err != nil || write != store.WorkloadUpdated {
		t.Fatalf("rewrite at the cap = %v, %v", write, err)
	}
	if _, write, err := st.PutWorkload(ctx, workloadPut("sub-b", "over", "d1")); err != nil || write != store.WorkloadCreated {
		t.Fatalf("another owner at the cap = %v, %v", write, err)
	}
}

func TestPG_Workload_ReadsAndDelete(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := t.Context()
	for _, in := range []store.WorkloadPut{
		workloadPut("sub-a", "b", "d"), workloadPut("sub-a", "a", "d"), workloadPut("sub-b", "a", "d"), workloadPut("service:bot", "a", "d"),
	} {
		mustPutWorkload(t, st, in)
	}
	names := func(vs []store.WorkloadView) (out []string) {
		for _, v := range vs {
			out = append(out, v.Owner+"/"+v.Name)
		}
		return out
	}
	all, err := st.ListWorkloads(ctx, store.Page{})
	if err != nil || len(all) != 4 || all[0].Owner != "service:bot" || all[3].Owner != "sub-b" {
		t.Fatalf("all = %v (%v), want four by owner then name", names(all), err)
	}
	mine, err := st.ListWorkloadsOf(ctx, []string{"sub-a", "service:bot"}, store.Page{})
	if err != nil || len(mine) != 3 {
		t.Fatalf("owners' = %v (%v), want three", names(mine), err)
	}
	if none, err := st.ListWorkloadsOf(ctx, nil, store.Page{}); err != nil || len(none) != 0 {
		t.Fatalf("no owners = %v (%v), want none", names(none), err)
	}
	if page, err := st.ListWorkloads(ctx, store.Page{Limit: 2, Offset: 1}); err != nil || len(page) != 2 || page[0].Name != all[1].Name || page[0].Owner != all[1].Owner {
		t.Fatalf("page = %v (%v)", names(page), err)
	}
	if _, err := st.GetWorkload(ctx, "sub-b", "b"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get of another owner's name = %v, want ErrNotFound", err)
	}
	gone, err := st.DeleteWorkload(ctx, "sub-a", "a")
	if err != nil || gone.Owner != "sub-a" || gone.Name != "a" {
		t.Fatalf("delete = %+v (%v)", gone, err)
	}
	if _, err := st.DeleteWorkload(ctx, "sub-a", "a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
	if rest, _ := st.ListWorkloads(ctx, store.Page{}); len(rest) != 3 {
		t.Fatalf("%d left, want three", len(rest))
	}
}

// A custom resource's workload is deleted only while the row is still the one read: after an apply of a
// recreated resource (a new uid, with the same spec or another) moved it, or after the row was replaced by
// another of the same uid, the stale delete removes nothing and the run stays. The matching one removes it.
func TestPG_Workload_K8sDeleteHoldsOnlyForTheRowRead(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := t.Context()
	poolA := ownPool(t, st, "sub-a", "A")
	runner, gen := uuid.New(), int64(1)
	put := func(uid, digest string) store.Workload {
		in := workloadPut("service:bot", "svc", digest)
		in.Source, in.SourcePoolID, in.SourceNamespace, in.SourceRunnerID, in.K8sGeneration, in.K8sUID =
			store.WorkloadSourceK8s, &poolA.ID, "team-a", &runner, &gen, uid
		return mustPutWorkload(t, st, in)
	}
	read := func() store.Workload {
		v, err := st.GetWorkload(ctx, "service:bot", "svc")
		if err != nil {
			t.Fatal(err)
		}
		return v.Workload
	}
	w := put("uid-1", "d1")
	run := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID
	if _, err := st.SetWorkloadRun(ctx, w.ID, mustClaim(t, st, w.ID), 1, run); err != nil {
		t.Fatal(err)
	}

	stale := read()
	for _, next := range []struct{ uid, digest string }{{"uid-2", "d1"}, {"uid-3", "d2"}} {
		put(next.uid, next.digest)
		if _, err := st.DeleteK8sWorkload(ctx, stale); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("delete of %s after %s was applied = %v, want ErrNotFound", stale.K8sUID, next.uid, err)
		}
		if v := read(); v.ID != w.ID || v.K8sUID != next.uid || v.CurrentRunID == nil || *v.CurrentRunID != run {
			t.Fatalf("after the stale delete = %+v, want row %s of %s with run %s kept", v, w.ID, next.uid, run)
		}
		stale = read()
	}

	// The same uid on a row recreated since the read is another row.
	if _, err := st.DeleteWorkload(ctx, "service:bot", "svc"); err != nil {
		t.Fatal(err)
	}
	put(stale.K8sUID, "d2")
	if _, err := st.DeleteK8sWorkload(ctx, stale); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete of a replaced row = %v, want ErrNotFound", err)
	}

	cur := read()
	if _, err := st.SetWorkloadRun(ctx, cur.ID, mustClaim(t, st, cur.ID), cur.Generation, run); err != nil {
		t.Fatal(err)
	}
	gone, err := st.DeleteK8sWorkload(ctx, read())
	if err != nil || gone.ID != cur.ID || gone.CurrentRunID == nil || *gone.CurrentRunID != run {
		t.Fatalf("matching delete = %+v (%v), want row %s with run %s", gone, err, cur.ID, run)
	}
	if _, err := st.GetWorkload(ctx, "service:bot", "svc"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after the matching delete = %v, want ErrNotFound", err)
	}
}

// The status sweep reads only custom-resource workloads, with their run's state and service health.
func TestPG_Workload_K8sStatusesCarryRunStateAndHealth(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := t.Context()
	// The service health column arrives with another migration; adding it here makes the read provable
	// whichever order the two land in, and does nothing once both have.
	if _, err := pool.Exec(ctx, `ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS service_status jsonb`); err != nil {
		t.Fatal(err)
	}
	p := ownPool(t, st, "sub-a", "A")
	k8s := func(name string) store.WorkloadPut {
		in := workloadPut("service:bot", name, "d")
		in.Source, in.SourcePoolID, in.SourceNamespace, in.K8sUID = store.WorkloadSourceK8s, &p.ID, "team-a", "uid-"+name
		return in
	}
	withRun := mustPutWorkload(t, st, k8s("has-run"))
	mustPutWorkload(t, st, k8s("no-run"))
	// The same spec reported from a resource recreated under the name keeps the row and takes the new uid.
	recreated := k8s("no-run")
	recreated.K8sUID = "uid-recreated"
	if _, write, err := st.PutWorkload(ctx, recreated); err != nil || write != store.WorkloadUnchanged {
		t.Fatalf("recreated resource: %q (%v), want unchanged", write, err)
	}
	mustPutWorkload(t, st, workloadPut("sub-a", "api-one", "d"))
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET service_status = '{"health":"unhealthy","restarts":2}' WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetWorkloadRun(ctx, withRun.ID, mustClaim(t, st, withRun.ID), 1, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.SetWorkloadStatusPushed(ctx, withRun.ID, "h1"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetWorkloadStatusPushed(ctx, uuid.New(), "h1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("status pushed on an unknown workload = %v, want ErrNotFound", err)
	}

	got, err := st.ListK8sWorkloadStatuses(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("statuses = %+v (%v), want the two custom-resource workloads", got, err)
	}
	for _, v := range got {
		switch v.Name {
		case "has-run":
			if v.RunState != types.RunRunning || v.ServiceHealth != "unhealthy" || v.StatusPushed != "h1" || v.K8sUID != "uid-has-run" {
				t.Errorf("has-run = %+v", v)
			}
		case "no-run":
			if v.RunState != "" || v.ServiceHealth != "" || v.StatusPushed != "" || v.K8sUID != "uid-recreated" {
				t.Errorf("no-run = %+v", v)
			}
		default:
			t.Errorf("unexpected workload %q in the sweep", v.Name)
		}
	}
}

// A person's erasure removes their workloads, and only theirs.
func TestPG_Workload_ErasureDeletesTheOwnersWorkloads(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := t.Context()
	mustPutWorkload(t, st, workloadPut("sub-a", "one", "d"))
	mustPutWorkload(t, st, workloadPut("sub-a", "two", "d"))
	mustPutWorkload(t, st, workloadPut("sub-b", "one", "d"))
	if _, err := st.DeleteRunnerPoolsByOwner(ctx, "sub-a"); err != nil {
		t.Fatal(err)
	}
	left, err := st.ListWorkloads(ctx, store.Page{})
	if err != nil || len(left) != 1 || left[0].Owner != "sub-b" {
		t.Fatalf("left = %+v (%v), want only sub-b's", left, err)
	}
}

func TestPG_WorkloadBinding_CreateConflictAndPoolFences(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := t.Context()
	p := ownPool(t, st, "sub-a", "A")
	bind := func(poolID uuid.UUID, ns string) (store.WorkloadBinding, error) {
		return st.CreateWorkloadBinding(ctx, store.WorkloadBinding{PoolID: poolID, Namespace: ns, Service: "bot", Sponsor: "sub-a", CreatedBy: "secop"})
	}

	b, err := bind(p.ID, "team-a")
	if err != nil || b.ID == uuid.Nil || b.PoolID != p.ID || b.Namespace != "team-a" || b.Service != "bot" || b.Sponsor != "sub-a" || b.CreatedBy != "secop" || b.CreatedAt.IsZero() {
		t.Fatalf("binding = %+v (%v)", b, err)
	}
	if _, err := bind(p.ID, "team-a"); !errors.Is(err, store.ErrWorkloadBindingConflict) || !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second binding of the namespace = %v, want ErrWorkloadBindingConflict", err)
	}
	if _, err := bind(ownPool(t, st, "sub-a", "B").ID, "team-a"); err != nil {
		t.Fatalf("same namespace on another pool: %v", err)
	}
	if _, err := bind(uuid.New(), "team-b"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("binding on an unknown pool = %v, want ErrNotFound", err)
	}
	if _, err := bind(p.ID, "Not_A_Label"); err == nil || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("binding of a bad namespace = %v, want the schema's refusal", err)
	}
	gone := ownPool(t, st, "sub-a", "C")
	if _, err := st.DeleteRunnerPool(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := bind(gone.ID, "team-c"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("binding on a deleted pool = %v, want ErrNotFound", err)
	}

	listed, err := st.ListWorkloadBindings(ctx, store.Page{})
	if err != nil || len(listed) != 2 {
		t.Fatalf("bindings = %+v (%v), want two", listed, err)
	}
	if page, err := st.ListWorkloadBindings(ctx, store.Page{Limit: 1}); err != nil || len(page) != 1 {
		t.Fatalf("page = %+v (%v), want one", page, err)
	}
	removed, err := st.DeleteWorkloadBinding(ctx, b.ID)
	if err != nil || removed.ID != b.ID || removed.Namespace != "team-a" {
		t.Fatalf("delete = %+v (%v)", removed, err)
	}
	if _, err := st.DeleteWorkloadBinding(ctx, b.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
}

// A runner finds a namespace's binding only through a live pool it belongs to, and deleting the pool takes
// the binding with it.
func TestPG_WorkloadBinding_ForRunnerFollowsMembershipAndPoolDelete(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := t.Context()
	p := ownPool(t, st, "sub-a", "A")
	member, outsider := claimedRunnerOf(t, st, "sub-a"), claimedRunnerOf(t, st, "sub-b")
	if _, _, _, err := st.AddOwnRunnerToPool(ctx, p.ID, member, "sub-a", ownerTestOrgHash); err != nil {
		t.Fatal(err)
	}
	b, err := st.CreateWorkloadBinding(ctx, store.WorkloadBinding{PoolID: p.ID, Namespace: "team-a", Service: "bot", Sponsor: "sub-a", CreatedBy: "secop"})
	if err != nil {
		t.Fatal(err)
	}

	got, err := st.BindingForRunner(ctx, member, "team-a")
	if err != nil || got.ID != b.ID || got.Service != "bot" || got.Sponsor != "sub-a" {
		t.Fatalf("member's binding = %+v (%v)", got, err)
	}
	if _, err := st.BindingForRunner(ctx, member, "team-b"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unbound namespace = %v, want ErrNotFound", err)
	}
	if _, err := st.BindingForRunner(ctx, outsider, "team-a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("binding through a pool the runner is not in = %v, want ErrNotFound", err)
	}

	if _, err := st.DeleteRunnerPool(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BindingForRunner(ctx, member, "team-a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("binding after the pool is deleted = %v, want ErrNotFound", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workload_bindings WHERE pool_id = $1`, p.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d bindings left on the deleted pool (%v)", n, err)
	}
}

// Retiring a service identity deletes its namespace bindings in the same transaction, and leaves every
// other identity's.
func TestPG_WorkloadBinding_RetiredServiceLosesItsBindings(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := t.Context()
	if _, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: "sub-a", Issuer: "https://issuer.test", Email: "a@corp.example"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ci-bot", "other-bot"} {
		if _, err := st.CreateServiceIdentity(ctx, types.ServiceIdentity{Name: name, Sponsor: "sub-a", UserType: types.UserTypeStandard, CreatedBy: "sub-a"}); err != nil {
			t.Fatal(err)
		}
	}
	p := ownPool(t, st, "sub-a", "A")
	for ns, svc := range map[string]string{"team-a": "ci-bot", "team-b": "ci-bot", "team-c": "other-bot"} {
		if _, err := st.CreateWorkloadBinding(ctx, store.WorkloadBinding{PoolID: p.ID, Namespace: ns, Service: svc, Sponsor: "sub-a", CreatedBy: "secop"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.RetireServiceIdentity(ctx, "ci-bot", "secop"); err != nil {
		t.Fatal(err)
	}
	left, err := st.ListWorkloadBindings(ctx, store.Page{})
	if err != nil || len(left) != 1 || left[0].Service != "other-bot" || left[0].Namespace != "team-c" {
		t.Fatalf("bindings after the retire = %+v (%v), want only other-bot's", left, err)
	}
}
