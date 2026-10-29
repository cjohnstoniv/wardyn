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
