// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// crEnv is svcEnv with the service identity created, one claimed runner attached over loopback (fake)
// and a member of an organization pool, and that pool binding namespace team-a to the service.
type crEnv struct {
	svcEnv
	runner uuid.UUID
	rp     types.RunnerPool // the runner's pool
	fake   *runnertest.Fake
	pol    string
}

func newCREnv(t *testing.T) crEnv {
	t.Helper()
	e := crEnv{svcEnv: newSvcEnv(t), fake: runnertest.NewFake()}
	e.mustCreate(t, svcName, svcSponsor)
	e.pol = e.plainPolicy(t, "wl-pol")
	e.rp = e.orgPool(t, "Cluster")
	ctx := context.Background()
	e.runner = uuid.New()
	fp := "fp-" + e.runner.String()
	if _, err := e.st.CreateRunner(ctx, types.Runner{ID: e.runner, Owner: svcSponsor, Name: "cluster", PublicKey: append(e.runner[:], e.runner[:]...),
		KeyFingerprint: fp, OrgURLSHA256: federation.OrgURLSHA256("https://org.example.com"), Limits: roomyRunnerLimits()}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.ClaimRunner(ctx, e.runner, svcSponsor, fp, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO runner_pool_members (pool_id, runner_id) VALUES ($1, $2)`, e.rp.ID, e.runner); err != nil {
		t.Fatal(err)
	}
	e.bindNamespace(t, e.rp.ID, "team-a")
	attachRunner(t, e.h.srv, e.runner, e.fake)
	return e
}

func (e crEnv) orgPool(t *testing.T, name string) types.RunnerPool {
	t.Helper()
	p, err := e.st.CreateRunnerPool(context.Background(), types.RunnerPool{ID: uuid.New(), Name: name, Scope: types.RunnerPoolOrganization}, "test")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (e crEnv) bindNamespace(t *testing.T, pool uuid.UUID, ns string) {
	t.Helper()
	if _, err := e.st.CreateWorkloadBinding(context.Background(), store.WorkloadBinding{PoolID: pool, Namespace: ns, Service: svcName,
		Sponsor: svcSponsor, CreatedBy: "sec-sub"}); err != nil {
		t.Fatal(err)
	}
}

// report sends rep as the runner's workload event.
func (e crEnv) report(t *testing.T, rep runnerwire.WorkloadReport) {
	t.Helper()
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.h.srv.WorkloadFromRunner(context.Background(), e.runner, runnerwire.Event{Kind: runnerwire.EventWorkload, Data: data}); err != nil {
		t.Fatalf("report %s %s/%s: %v", rep.Op, rep.Namespace, rep.Name, err)
	}
}

// apply is an apply report of name in ns whose intent runs under the env's policy; extra is spliced into the intent.
func (e crEnv) apply(ns, name, extra string) runnerwire.WorkloadReport {
	return runnerwire.WorkloadReport{Op: runnerwire.WorkloadOpApply, Namespace: ns, Name: name, UID: "uid-" + name, Generation: 1,
		Spec: json.RawMessage(`{"intent":{"agent":"claude-code","task":"t","policy_id":"` + e.pol + `"` + extra + `}}`)}
}

// statuses waits until the runner holds n statuses, and returns them.
func (e crEnv) statuses(t *testing.T, n int) []runnerwire.WorkloadStatusArgs {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if got := e.fake.WorkloadStatuses(); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("the runner holds %+v, want %d statuses", e.fake.WorkloadStatuses(), n)
		}
	}
}

// clientWorkloadDoc is a workload document of name whose intent is the JSON intent.
func clientWorkloadDoc(t *testing.T, name, intent string) client.WorkloadDocument {
	return client.WorkloadDocument{APIVersion: client.TemplateDocumentVersion, Kind: client.WorkloadDocumentKind, Name: name, Intent: decodeIntent(t, intent)}
}

// An applied resource in a namespace nothing binds reads NotBound and records nothing; one in a namespace
// bound on another pool (whose runners own it) gets no answer at all.
func TestPG_WorkloadFromRunner_UnboundIsNotBound(t *testing.T) {
	e := newCREnv(t)
	e.bindNamespace(t, e.orgPool(t, "Other cluster").ID, "team-b")
	e.report(t, e.apply("team-b", "web", ""))
	e.report(t, e.apply("team-x", "web", ""))
	e.statuses(t, 1)
	time.Sleep(200 * time.Millisecond) // a status for team-b would have arrived by now
	if got := e.fake.WorkloadStatuses(); len(got) != 1 || got[0].Namespace != "team-x" || got[0].Phase != runnerwire.WorkloadPhaseNotBound {
		t.Fatalf("statuses %+v, want one NotBound for team-x", got)
	}
	if _, err := e.st.GetWorkload(context.Background(), svcPrin, "web"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unbound resource was recorded: %v", err)
	}
}

// A resource naming a pool other than its binding's is refused workload_pool_mismatch and records nothing.
func TestPG_WorkloadFromRunner_PoolMismatchIsRefused(t *testing.T) {
	e := newCREnv(t)
	other := e.orgPool(t, "Elsewhere")
	e.report(t, e.apply("team-a", "web", `,"runner_pool_id":"`+other.ID.String()+`"`))
	got := e.statuses(t, 1)
	if got[0].Phase != runnerwire.WorkloadPhaseRefused || got[0].Reason != reasonWorkloadPoolMismatch || got[0].ObservedGeneration != 1 || got[0].UID != "uid-web" {
		t.Fatalf("status %+v, want Refused %s at generation 1", got[0], reasonWorkloadPoolMismatch)
	}
	if _, err := e.st.GetWorkload(context.Background(), svcPrin, "web"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a mismatched resource was recorded: %v", err)
	}
}

// A binding whose service changed sponsor since it was made records the resource but launches nothing: the
// launch is refused workload_binding_inactive and the resource reads BindingInactive.
func TestPG_WorkloadFromRunner_SponsorChangeIsBindingInactive(t *testing.T) {
	e := newCREnv(t)
	if w := doSSO(t, e.h.srv, http.MethodPatch, "/api/v1/service-identities/"+svcName, e.sec, `{"sponsor":"`+svcSponsor2+`"}`); w.Code != http.StatusOK {
		t.Fatalf("sponsor change: %d %s", w.Code, w.Body.String())
	}
	e.report(t, e.apply("team-a", "web", ""))
	got := e.statuses(t, 1)
	if got[0].Phase != runnerwire.WorkloadPhaseBindingInactive || got[0].Reason != reasonWorkloadBindingInactive || got[0].RunID != "" || got[0].UID != "uid-web" {
		t.Fatalf("status %+v, want BindingInactive for uid-web with no run", got[0])
	}
	v, err := e.st.GetWorkload(context.Background(), svcPrin, "web")
	if err != nil || v.Source != store.WorkloadSourceK8s || v.SourcePoolID == nil || *v.SourcePoolID != e.rp.ID || v.CurrentRunID != nil ||
		v.LastRefusal == nil || v.LastRefusal.Reason != reasonWorkloadBindingInactive {
		t.Fatalf("stored workload %+v (%v), want a k8s workload on the bound pool, refused %s, with no run", v, err, reasonWorkloadBindingInactive)
	}
	events := e.h.audit.snapshot()
	if countAudit(events, "workload.apply") != 1 || countAudit(events, "workload.launch_refused") != 1 || countAudit(events, "workload.launch") != 0 {
		t.Fatalf("audit apply/launch_refused/launch = %d/%d/%d, want 1/1/0", countAudit(events, "workload.apply"),
			countAudit(events, "workload.launch_refused"), countAudit(events, "workload.launch"))
	}
}

// A namespace's full list prunes the workloads its resources recorded on the bound pool that the list no
// longer names, each audited workload.delete via k8s_sync; another namespace's are untouched.
func TestPG_WorkloadFromRunner_SyncPrunes(t *testing.T) {
	e := newCREnv(t)
	e.bindNamespace(t, e.rp.ID, "team-b")
	ctx := context.Background()
	for _, w := range []struct{ ns, name string }{{"team-a", "keep"}, {"team-a", "gone"}, {"team-b", "other"}} {
		in, invalid := normalizeWorkload(clientWorkloadDoc(t, w.name, `{"agent":"claude-code","task":"t","policy_id":"`+e.pol+`"}`))
		if invalid != nil {
			t.Fatalf("normalize: %+v", invalid)
		}
		gen := int64(1)
		in.Owner, in.Source, in.By = svcPrin, store.WorkloadSourceK8s, svcPrin
		in.SourcePoolID, in.SourceNamespace, in.SourceRunnerID, in.K8sGeneration, in.K8sUID = &e.rp.ID, w.ns, &e.runner, &gen, "uid-"+w.name
		if _, _, err := e.st.PutWorkload(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	e.report(t, runnerwire.WorkloadReport{Op: runnerwire.WorkloadOpSync, Namespace: "team-a", Names: []string{"keep"}})
	if _, err := e.st.GetWorkload(ctx, svcPrin, "gone"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the workload the list omits is still there: %v", err)
	}
	for _, name := range []string{"keep", "other"} {
		if _, err := e.st.GetWorkload(ctx, svcPrin, name); err != nil {
			t.Fatalf("%s: %v, want it kept", name, err)
		}
	}
	var deletes []map[string]any
	for _, ev := range e.h.audit.snapshot() {
		if ev.Action == "workload.delete" {
			var d map[string]any
			_ = json.Unmarshal(ev.Data, &d)
			deletes = append(deletes, d)
		}
	}
	if len(deletes) != 1 || deletes[0]["name"] != "gone" || deletes[0]["via"] != workloadViaK8sSync || deletes[0]["namespace"] != "team-a" {
		t.Fatalf("workload.delete rows %+v, want one for gone via %s", deletes, workloadViaK8sSync)
	}
}

// A delete names the resource's uid: one for a resource the name has since been recreated as leaves the
// newer resource's workload, and the matching one deletes it (workload.delete via k8s).
func TestPG_WorkloadFromRunner_DeleteFollowsTheResourceUID(t *testing.T) {
	e := newCREnv(t)
	ctx := context.Background()
	in, invalid := normalizeWorkload(clientWorkloadDoc(t, "web", `{"agent":"claude-code","task":"t","policy_id":"`+e.pol+`"}`))
	if invalid != nil {
		t.Fatalf("normalize: %+v", invalid)
	}
	gen := int64(1)
	in.Owner, in.Source, in.By = svcPrin, store.WorkloadSourceK8s, svcPrin
	in.SourcePoolID, in.SourceNamespace, in.SourceRunnerID, in.K8sGeneration, in.K8sUID = &e.rp.ID, "team-a", &e.runner, &gen, "uid-new"
	if _, _, err := e.st.PutWorkload(ctx, in); err != nil {
		t.Fatal(err)
	}
	e.report(t, runnerwire.WorkloadReport{Op: runnerwire.WorkloadOpDelete, Namespace: "team-a", Name: "web", UID: "uid-old"})
	if _, err := e.st.GetWorkload(ctx, svcPrin, "web"); err != nil {
		t.Fatalf("a delete of the replaced resource removed the new one's workload: %v", err)
	}
	e.report(t, runnerwire.WorkloadReport{Op: runnerwire.WorkloadOpDelete, Namespace: "team-a", Name: "web", UID: "uid-new"})
	if _, err := e.st.GetWorkload(ctx, svcPrin, "web"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the matching delete left the workload: %v", err)
	}
	if n := countAudit(e.h.audit.snapshot(), "workload.delete"); n != 1 {
		t.Fatalf("%d workload.delete rows, want 1", n)
	}
}

// A report of the resource at a lower metadata.generation than recorded, with another spec, is a stale
// runner cache: the recorded spec stands and no workload.apply row is written for it.
func TestPG_WorkloadFromRunner_LowerGenerationIsIgnored(t *testing.T) {
	e := newCREnv(t)
	newer := e.apply("team-a", "web", "")
	newer.Generation = 2
	e.report(t, newer)
	first, err := e.st.GetWorkload(context.Background(), svcPrin, "web")
	if err != nil {
		t.Fatal(err)
	}
	older := e.apply("team-a", "web", "")
	older.Spec = json.RawMessage(`{"intent":{"agent":"claude-code","task":"older","policy_id":"` + e.pol + `"}}`)
	e.report(t, older)
	v, err := e.st.GetWorkload(context.Background(), svcPrin, "web")
	if err != nil || v.Digest != first.Digest || v.Generation != first.Generation || v.K8sGeneration == nil || *v.K8sGeneration != 2 {
		t.Fatalf("after the older report = %+v (%v), want the generation 2 spec kept", v.Workload, err)
	}
	if n := countAudit(e.h.audit.snapshot(), "workload.apply"); n != 1 {
		t.Fatalf("%d workload.apply rows, want 1", n)
	}
}

// putK8sWorkload records name in team-a as resource uid's workload on the env's pool, as an apply of it
// would, with an intent of task; it launches nothing.
func (e crEnv) putK8sWorkload(t *testing.T, name, uid, task string) store.Workload {
	t.Helper()
	in, invalid := normalizeWorkload(clientWorkloadDoc(t, name, `{"agent":"claude-code","task":"`+task+`","policy_id":"`+e.pol+`"}`))
	if invalid != nil {
		t.Fatalf("normalize: %+v", invalid)
	}
	gen := int64(1)
	in.Owner, in.Source, in.By = svcPrin, store.WorkloadSourceK8s, svcPrin
	in.SourcePoolID, in.SourceNamespace, in.SourceRunnerID, in.K8sGeneration, in.K8sUID = &e.rp.ID, "team-a", &e.runner, &gen, uid
	wl, _, err := e.st.PutWorkload(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return wl
}

// k8sWorkloadWithRun is putK8sWorkload with a live run recorded as the workload's current run. The run is
// one the service launches through the API, as the workload <name>-api.
func (e crEnv) k8sWorkloadWithRun(t *testing.T, name, uid string) (store.Workload, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tok := e.mustMint(t, e.sponsor, svcName)
	run := launchedRun(t, decodeApply(t, e.as(t, tok.Token, http.MethodPut, "/api/v1/workloads/"+name+"-api", workloadBody(e.pol, "t", "")), http.StatusCreated))
	wl := e.putK8sWorkload(t, name, uid, "t")
	claimed, ok, err := e.st.ClaimWorkloadLaunch(ctx, wl.ID)
	if err != nil || !ok {
		t.Fatalf("claim %s = %v, %v; want a claim", name, ok, err)
	}
	if _, err := e.st.SetWorkloadRun(ctx, wl.ID, claimed.ClaimToken, claimed.Generation, run); err != nil {
		t.Fatal(err)
	}
	wl.CurrentRunID, wl.CurrentGeneration = &run, claimed.Generation
	return wl, run
}

// A drop that read the resource's workload before an apply of the recreated resource moved the row to its
// own uid, with the same spec or another, leaves the row and its run and audits nothing; the drop of the
// row as it now is removes it and ends the run.
func TestPG_WorkloadFromRunner_DropLeavesARecreatedResource(t *testing.T) {
	e := newCREnv(t)
	ctx := context.Background()
	wl, run := e.k8sWorkloadWithRun(t, "web", "uid-1")
	b, err := e.st.BindingForRunner(ctx, e.runner, "team-a")
	if err != nil {
		t.Fatal(err)
	}
	si, err := e.h.srv.serviceRecord(ctx, svcName)
	if err != nil {
		t.Fatal(err)
	}
	sr, cancel, err := serviceLaunchRequest(si, b.Sponsor)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	drop := func(w store.Workload) {
		t.Helper()
		if err := e.h.srv.dropK8sWorkload(ctx, e.st, sr, e.runner, b, si, w, w.K8sUID, workloadViaK8s); err != nil {
			t.Fatal(err)
		}
	}

	stale := wl
	for _, next := range []struct{ uid, task string }{{"uid-2", "t"}, {"uid-3", "t2"}} {
		e.putK8sWorkload(t, "web", next.uid, next.task)
		drop(stale)
		v, err := e.st.GetWorkload(ctx, svcPrin, "web")
		if err != nil || v.ID != wl.ID || v.K8sUID != next.uid || v.CurrentRunID == nil || *v.CurrentRunID != run {
			t.Fatalf("after a drop of %s = %+v (%v), want row %s of %s with run %s kept", stale.K8sUID, v.Workload, err, wl.ID, next.uid, run)
		}
		stale = v.Workload
	}
	if r, err := e.st.GetRun(ctx, run); err != nil || r.State == types.RunKilled {
		t.Fatalf("run after the stale drops = %q (%v), want it running", r.State, err)
	}
	if n := countAudit(e.h.audit.snapshot(), "workload.delete"); n != 0 || owedEnds(t, e.svcEnv, run) != 0 {
		t.Fatalf("%d workload.delete rows and %d owed ends after the stale drops, want none", n, owedEnds(t, e.svcEnv, run))
	}

	drop(stale)
	if _, err := e.st.GetWorkload(ctx, svcPrin, "web"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the matching drop left the workload: %v", err)
	}
	if got := killReason(t, e.svcEnv, run); got != workloadKillDeleted {
		t.Fatalf("run.kill reason = %q, want %q", got, workloadKillDeleted)
	}
	if n := countAudit(e.h.audit.snapshot(), "workload.delete"); n != 1 {
		t.Fatalf("%d workload.delete rows, want 1", n)
	}
}

// A bound apply, with runners on and the binding live, launches the resource's workload as the bound
// service on the binding's pool, and the resource's status then names the run.
func TestPG_WorkloadFromRunner_BoundApplyLaunchesOnTheBoundPool(t *testing.T) {
	e := newCREnv(t)
	ctx := context.Background()
	site, err := e.st.GetSiteConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	site.Runners = &types.RunnerSettings{Enabled: true}
	if _, err := e.st.PutSiteConfig(ctx, site); err != nil {
		t.Fatal(err)
	}
	e.h.srv.cfg.RunnerOrgURL = "https://org.example.com" // the env's runner is bound to this organisation
	// Pools place runs only with a runner wired (poolsPlaceRuns), as the daemon always has; the env's runner,
	// attached before, is registered with it as the daemon registers each runner endpoint.
	orch := orchestrator.New()
	orch.Add(e.h.srv.runnerEndpoint(e.runner).sub)
	e.h.srv.cfg.Runner = orch
	e.report(t, e.apply("team-a", "web", ""))

	var got runnerwire.WorkloadStatusArgs
	for deadline := time.Now().Add(5 * time.Second); got.RunID == ""; time.Sleep(10 * time.Millisecond) {
		for _, s := range e.fake.WorkloadStatuses() {
			if s.RunID != "" {
				got = s
			}
		}
		if got.RunID == "" && time.Now().After(deadline) {
			v, _ := e.st.GetWorkload(ctx, svcPrin, "web")
			t.Fatalf("no status names a run: %+v (workload refusal %+v)", e.fake.WorkloadStatuses(), v.LastRefusal)
		}
	}
	v, err := e.st.GetWorkload(ctx, svcPrin, "web")
	if err != nil || v.CurrentRunID == nil || v.CurrentRunID.String() != got.RunID || v.CurrentGeneration != v.Generation {
		t.Fatalf("stored workload %+v (%v), want its current run to be the status's %s", v.Workload, err, got.RunID)
	}
	if got.Namespace != "team-a" || got.Name != "web" || got.UID != "uid-web" || got.ObservedGeneration != 1 {
		t.Fatalf("status %+v, want team-a/web uid-web at generation 1", got)
	}
	run, err := e.st.GetRun(ctx, *v.CurrentRunID)
	if err != nil || run.CreatedBy != svcPrin || run.RunnerPoolID == nil || *run.RunnerPoolID != e.rp.ID {
		t.Fatalf("run %+v (%v), want one of %s on pool %s", run, err, svcPrin, e.rp.ID)
	}
	events := e.h.audit.snapshot()
	if countAudit(events, "workload.launch") != 1 || countAudit(events, "workload.launch_refused") != 0 {
		t.Fatalf("audit launch/launch_refused = %d/%d, want 1/0", countAudit(events, "workload.launch"), countAudit(events, "workload.launch_refused"))
	}
}

// A binding whose sponsor was revoked has lost its authority: a delete report and a full list that omits the
// resource leave the workload row and its run alone, and audit nothing. A browser-only cut (CutSessions) would
// not do: it leaves the service's tokens working, and the binding is judged by the rules that end them.
func TestPG_WorkloadFromRunner_BindingWithoutAuthorityDeletesNothing(t *testing.T) {
	e := newCREnv(t)
	ctx := context.Background()
	_, run := e.k8sWorkloadWithRun(t, "web", "uid-web")
	if err := e.rev.RevokeSub(ctx, svcSponsor); err != nil {
		t.Fatal(err)
	}
	e.report(t, runnerwire.WorkloadReport{Op: runnerwire.WorkloadOpDelete, Namespace: "team-a", Name: "web", UID: "uid-web"})
	e.report(t, runnerwire.WorkloadReport{Op: runnerwire.WorkloadOpSync, Namespace: "team-a", Names: []string{}})
	if v, err := e.st.GetWorkload(ctx, svcPrin, "web"); err != nil || v.CurrentRunID == nil || *v.CurrentRunID != run {
		t.Fatalf("workload after the delete and the sync = %+v (%v), want it kept with run %s", v.Workload, err, run)
	}
	if r, err := e.st.GetRun(ctx, run); err != nil || r.State == types.RunKilled {
		t.Fatalf("run state = %q (%v), want it not ended", r.State, err)
	}
	if n := countAudit(e.h.audit.snapshot(), "workload.delete"); n != 0 || owedEnds(t, e.svcEnv, run) != 0 {
		t.Fatalf("%d workload.delete rows and %d owed ends, want none", n, owedEnds(t, e.svcEnv, run))
	}
}

// A resource's delete ends its workload's current run: the run is killed workload_deleted, and the
// workload.delete row is the run's and names it as ended_run_id.
func TestPG_WorkloadFromRunner_DeleteEndsTheCurrentRun(t *testing.T) {
	e := newCREnv(t)
	_, run := e.k8sWorkloadWithRun(t, "web", "uid-web")
	e.report(t, runnerwire.WorkloadReport{Op: runnerwire.WorkloadOpDelete, Namespace: "team-a", Name: "web", UID: "uid-web"})
	if got := killReason(t, e.svcEnv, run); got != workloadKillDeleted {
		t.Fatalf("run.kill reason = %q, want %q", got, workloadKillDeleted)
	}
	if r, err := e.st.GetRun(context.Background(), run); err != nil || r.State != types.RunKilled {
		t.Fatalf("run state = %q (%v), want KILLED", r.State, err)
	}
	var deletes []types.AuditEvent
	for _, ev := range e.h.audit.snapshot() {
		if ev.Action == "workload.delete" {
			deletes = append(deletes, ev)
		}
	}
	if len(deletes) != 1 || deletes[0].RunID == nil || *deletes[0].RunID != run {
		t.Fatalf("workload.delete rows %+v, want one on run %s", deletes, run)
	}
	var d map[string]any
	if err := json.Unmarshal(deletes[0].Data, &d); err != nil || d["ended_run_id"] != run.String() || d["via"] != workloadViaK8s || d["uid"] != "uid-web" {
		t.Fatalf("workload.delete data %s (%v), want ended_run_id %s via %s for uid-web", deletes[0].Data, err, run, workloadViaK8s)
	}
}
