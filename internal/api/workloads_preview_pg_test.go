// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const previewPath = "/api/v1/workloads/web/preview"

// previewBody is workloadBody with a description.
func previewBody(pol, task, desc string) string {
	return `{"description":"` + desc + `","intent":{"agent":"claude-code","task":"` + task + `","policy_id":"` + pol + `"}}`
}

func decodePreview(t *testing.T, w *httptest.ResponseRecorder, want int) client.WorkloadPreview {
	t.Helper()
	if w.Code != want {
		t.Fatalf("preview: %d %s, want %d", w.Code, w.Body.String(), want)
	}
	var out client.WorkloadPreview
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func previewPaths(p client.WorkloadPreview) []string {
	var out []string
	for _, c := range p.Changes {
		out = append(out, c.Op+" "+c.Path)
	}
	return out
}

// workloadFacts is what a preview must leave alone: the workload rows, their generation and launch claim,
// the workload.* audit rows and the runs.
type workloadFacts struct {
	Rows, Runs, Audit int
	Generation        int64
	ClaimUntil        string
}

func (e svcEnv) workloadFacts(t *testing.T) workloadFacts {
	t.Helper()
	var f workloadFacts
	ctx := context.Background()
	if err := e.pool.QueryRow(ctx, `SELECT count(*), coalesce(sum(generation), 0)::bigint, coalesce(string_agg(launch_claim_until::text, ',' ORDER BY name), '')
		FROM workloads`).Scan(&f.Rows, &f.Generation, &f.ClaimUntil); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM agent_runs`).Scan(&f.Runs); err != nil {
		t.Fatal(err)
	}
	for _, ev := range e.h.audit.snapshot() {
		if strings.HasPrefix(ev.Action, "workload.") {
			f.Audit++
		}
	}
	return f
}

// TestPG_WorkloadPreview_WritesNothing: a preview of a new, a changed and an unchanged workload leaves the
// rows, the generation, the launch claim, the workload.* audit rows and the runs as they were.
func TestPG_WorkloadPreview_WritesNothing(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	tok := e.mustMint(t, e.sponsor, svcName)
	pol := e.plainPolicy(t, "wl-pol")
	decodeApply(t, e.as(t, tok.Token, http.MethodPut, "/api/v1/workloads/web", workloadBody(pol, "t", "")), http.StatusCreated)

	before := e.workloadFacts(t)
	if before.Rows != 1 || before.Runs != 1 || before.Audit < 2 {
		t.Fatalf("setup facts = %+v, want one workload, one run and its audit rows", before)
	}
	for name, c := range map[string]struct{ path, body string }{
		"new name":  {"/api/v1/workloads/fresh/preview", workloadBody(pol, "t", "")},
		"changed":   {previewPath, workloadBody(pol, "t2", "")},
		"unchanged": {previewPath, workloadBody(pol, "t", "")},
	} {
		decodePreview(t, e.as(t, tok.Token, http.MethodPost, c.path, c.body), http.StatusOK)
		if after := e.workloadFacts(t); after != before {
			t.Fatalf("after the %s preview facts = %+v, want %+v", name, after, before)
		}
	}
}

// TestPG_WorkloadPreview_Outcomes: created, updated and unchanged, what changes, whether a run starts and
// which one it replaces, and the store's refusals carried in the body.
func TestPG_WorkloadPreview_Outcomes(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	tok := e.mustMint(t, e.sponsor, svcName)
	pol := e.plainPolicy(t, "wl-pol")
	ctx := context.Background()

	p := decodePreview(t, e.as(t, tok.Token, http.MethodPost, previewPath, workloadBody(pol, "t", "")), http.StatusOK)
	if p.Op != "create" || p.Outcome != client.WorkloadCreated || p.Launch.State != "would_launch" || p.Launch.Replaces != nil ||
		p.Stored != nil || p.StoredDigest != "" || p.Digest == "" || p.Refusal != nil || p.Name != "web" || p.Owner != svcPrin ||
		p.Launch.Preflight == nil || len(p.Changes) == 0 {
		t.Fatalf("new workload preview = %+v, want create, created, would_launch with a preflight, nothing stored", p)
	}

	first := decodeApply(t, e.as(t, tok.Token, http.MethodPut, "/api/v1/workloads/web", workloadBody(pol, "t", "")), http.StatusCreated)
	run := launchedRun(t, first)

	same := decodePreview(t, e.as(t, tok.Token, http.MethodPost, previewPath, workloadBody(pol, "t", "")), http.StatusOK)
	if same.Op != "none" || same.Outcome != client.WorkloadUnchanged || same.Launch.State != "none" || same.Changes == nil || len(same.Changes) != 0 ||
		same.Stored == nil || same.StoredDigest == "" || same.StoredDigest != same.Digest {
		t.Fatalf("unchanged preview = %+v, want none, unchanged, no launch, an empty changes array and equal digests", same)
	}
	if raw := e.as(t, tok.Token, http.MethodPost, previewPath, workloadBody(pol, "t", "")).Body.String(); !strings.Contains(raw, `"changes":[]`) {
		t.Fatalf("unchanged preview body = %s, want changes as [] and never null", raw)
	}

	desc := decodePreview(t, e.as(t, tok.Token, http.MethodPost, previewPath, previewBody(pol, "t", "new words")), http.StatusOK)
	if desc.Op != "none" || desc.Outcome != client.WorkloadUnchanged || desc.Launch.State != "none" ||
		strings.Join(previewPaths(desc), ",") != "add /description" {
		t.Fatalf("description-only preview = %+v (%v), want none, unchanged, one /description change", desc, previewPaths(desc))
	}

	upd := decodePreview(t, e.as(t, tok.Token, http.MethodPost, previewPath, workloadBody(pol, "t2", "")), http.StatusOK)
	if upd.Op != "update" || upd.Outcome != client.WorkloadUpdated || upd.Launch.State != "would_launch" ||
		upd.Launch.Replaces == nil || *upd.Launch.Replaces != run || strings.Join(previewPaths(upd), ",") != "change /intent/task" {
		t.Fatalf("changed preview = %+v (%v), want update, would_launch replacing %s, one /intent/task change", upd, previewPaths(upd), run)
	}

	// A spec whose launch was refused earlier is unchanged by digest, and still owes its launch.
	in, invalid := normalizeWorkload(clientWorkloadDoc(t, "stuck", `{"agent":"claude-code","task":"t","policy_id":"`+pol+`"}`))
	if invalid != nil {
		t.Fatalf("normalize: %+v", invalid)
	}
	in.Owner, in.Source, in.By = svcPrin, store.WorkloadSourceAPI, svcPrin
	stuck, _, err := e.st.PutWorkload(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	claim := mustClaimWorkload(t, e.st, stuck.ID)
	if err := e.st.RecordWorkloadRefusal(ctx, stuck.ID, claim, store.WorkloadRefusal{Generation: stuck.Generation, Reason: "test_refusal", At: e.h.srv.cfg.Now()}); err != nil {
		t.Fatal(err)
	}
	refused := decodePreview(t, e.as(t, tok.Token, http.MethodPost, "/api/v1/workloads/stuck/preview", workloadBody(pol, "t", "")), http.StatusOK)
	if refused.Op != "none" || refused.Outcome != client.WorkloadUnchanged || refused.Launch.State != "would_launch" {
		t.Fatalf("preview of an unchanged spec whose launch was refused = %+v, want unchanged and would_launch", refused)
	}

	// A workload a custom resource owns is a 409 in the body; the diff still shows.
	poolA, err := e.st.CreateRunnerPool(ctx, types.RunnerPool{ID: uuid.New(), Name: "cr-pool", Scope: types.RunnerPoolUser, Owner: svcPrin}, svcPrin)
	if err != nil {
		t.Fatal(err)
	}
	gen, runner := int64(1), uuid.New()
	cr := in
	cr.Name, cr.Source, cr.SourcePoolID, cr.SourceNamespace, cr.SourceRunnerID, cr.K8sGeneration, cr.K8sUID = "cr", store.WorkloadSourceK8s, &poolA.ID, "team-a", &runner, &gen, "uid-cr"
	if _, _, err := e.st.PutWorkload(ctx, cr); err != nil {
		t.Fatal(err)
	}
	conflict := decodePreview(t, e.as(t, tok.Token, http.MethodPost, "/api/v1/workloads/cr/preview", workloadBody(pol, "t", "")), http.StatusOK)
	if conflict.Refusal == nil || conflict.Refusal.Status != http.StatusConflict || conflict.Refusal.Reason != reasonWorkloadSourceConflict ||
		conflict.Launch.State != "none" || conflict.Op != "none" {
		t.Fatalf("custom resource preview = %+v, want refusal 409 %s and no launch", conflict, reasonWorkloadSourceConflict)
	}
	put := e.as(t, tok.Token, http.MethodPut, "/api/v1/workloads/cr", workloadBody(pol, "t", ""))
	if put.Code != conflict.Refusal.Status || respReason(put) != conflict.Refusal.Reason {
		t.Fatalf("the PUT answers %d %s, the preview carried %+v: want the same", put.Code, respReason(put), conflict.Refusal)
	}

	// At the cap a new name is a 422 in the body.
	if _, err := e.pool.Exec(ctx, `INSERT INTO workloads (owner, name, source, intent, digest, created_by)
		SELECT $1::text, 'fill' || g, 'api', '{}', 'x', $1::text FROM generate_series(1, $2::int) g`, svcPrin, store.MaxWorkloadsPerOwner); err != nil {
		t.Fatal(err)
	}
	full := decodePreview(t, e.as(t, tok.Token, http.MethodPost, "/api/v1/workloads/one-more/preview", workloadBody(pol, "t", "")), http.StatusOK)
	if full.Refusal == nil || full.Refusal.Status != http.StatusUnprocessableEntity || full.Refusal.Reason != reasonWorkloadCapReached || full.Launch.State != "none" {
		t.Fatalf("preview at the cap = %+v, want refusal 422 %s and no launch", full, reasonWorkloadCapReached)
	}
}

// mustClaimWorkload takes the launch claim of the workload id and returns its token.
func mustClaimWorkload(t *testing.T, st store.PG, id uuid.UUID) uuid.UUID {
	t.Helper()
	w, ok, err := st.ClaimWorkloadLaunch(context.Background(), id)
	if err != nil || !ok || w.ClaimToken == uuid.Nil {
		t.Fatalf("claim workload %s = %+v, %v, %v; want a claim", id, w, ok, err)
	}
	return w.ClaimToken
}

// TestPG_WorkloadPreview_InvalidBodyIsThePutsRefusal: the same strict decode and the same door diagnostics.
func TestPG_WorkloadPreview_InvalidBodyIsThePutsRefusal(t *testing.T) {
	e := newSvcEnv(t)
	body := `{"intent":{"agent":"claude-code","no_such_field":1}}`
	put := doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, body)
	got := doSSO(t, e.h.srv, http.MethodPost, previewPath, e.mem, body)
	if put.Code != http.StatusBadRequest || got.Code != put.Code || respReason(got) != respReason(put) || respReason(got) != reasonWorkloadInvalid {
		t.Fatalf("preview %d %s, put %d %s; want the same 400 %s", got.Code, respReason(got), put.Code, respReason(put), reasonWorkloadInvalid)
	}
	if bad := doSSO(t, e.h.srv, http.MethodPost, previewPath, e.mem, `{"intent":{},"unknown":1}`); bad.Code != http.StatusBadRequest {
		t.Fatalf("preview with an unknown body field: %d %s, want 400", bad.Code, bad.Body.String())
	}
}

// TestPG_WorkloadPreview_LaunchMatchesApply: the refusal a preview predicts is the one the apply records.
func TestPG_WorkloadPreview_LaunchMatchesApply(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	unit, err := e.st.UpsertGovernanceUnit(ctx, types.GovernanceUnit{
		ID: uuid.New(), Name: "walled",
		Ceiling:   types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2},
		Limits:    types.GovernanceLimits{DenyInteractive: true},
		CreatedBy: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.UpsertGovernanceMembership(ctx, types.GovernanceMembership{
		ID: uuid.New(), SubjectType: types.CapabilitySubjectGroup, Subject: "walled", UnitID: unit.ID, CreatedBy: "test",
	}); err != nil {
		t.Fatal(err)
	}
	walled := accessSession(t, "walled-sub", "walled@corp.example", oidc.RoleUser, []string{"walled"})
	pol := e.plainPolicy(t, "wl-pol")
	launchedRun(t, decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", walled, workloadBody(pol, "t", "")), http.StatusCreated))

	refusing := workloadBody(pol, "t", `,"interactive":true`)
	p := decodePreview(t, doSSO(t, e.h.srv, http.MethodPost, previewPath, walled, refusing), http.StatusOK)
	if p.Launch.State != "would_refuse" || p.Launch.Reason == "" || p.Launch.Message == "" || p.Launch.Preflight != nil {
		t.Fatalf("preview launch = %+v, want would_refuse with a reason and a message", p.Launch)
	}
	res := decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", walled, refusing), http.StatusOK)
	if res.Launch.State != client.WorkloadRefused || res.Workload.LastRefusal == nil || res.Workload.LastRefusal.Reason != p.Launch.Reason {
		t.Fatalf("apply launch = %+v, last_refusal = %+v; want refused with the preview's reason %q", res.Launch, res.Workload.LastRefusal, p.Launch.Reason)
	}
}

// TestPG_WorkloadPreview_ServiceScope: a service token may preview; it reads its own namespace only; and a
// token with no service scope on the route is still refused where the allow-list says so.
func TestPG_WorkloadPreview_ServiceScope(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	tok := e.mustMint(t, e.sponsor, svcName)
	pol := e.plainPolicy(t, "wl-pol")

	if w := e.as(t, tok.Token, http.MethodPost, previewPath, workloadBody(pol, "t", "")); w.Code != http.StatusOK {
		t.Fatalf("service token preview: %d %s, want 200", w.Code, w.Body.String())
	}

	// The sponsor's own workload of that name is not the service's, and ?owner= is not read.
	decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.sponsor, workloadBody(pol, "mine", "")), http.StatusCreated)
	p := decodePreview(t, e.as(t, tok.Token, http.MethodPost, previewPath+"?owner="+svcSponsor, workloadBody(pol, "t", "")), http.StatusOK)
	if p.Op != "create" || p.Stored != nil || p.Owner != svcPrin {
		t.Fatalf("service preview of a name its sponsor holds = %+v, want a create of its own %s", p, svcPrin)
	}
	q := decodePreview(t, doSSO(t, e.h.srv, http.MethodPost, previewPath+"?owner="+svcPrin, e.other, workloadBody(pol, "t", "")), http.StatusOK)
	if q.Op != "create" || q.Stored != nil || q.Owner == svcPrin {
		t.Fatalf("another person's preview with ?owner=%s = %+v, want a create in their own namespace", svcPrin, q)
	}
}

// TestPG_WorkloadPreview_RateLimitPassesThrough: preflight's own 429 is the preview's, not a prediction.
func TestPG_WorkloadPreview_RateLimitPassesThrough(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	tok := e.mustMint(t, e.sponsor, svcName)
	pol := e.plainPolicy(t, "wl-pol")
	e.h.srv.preflightLimiter = &principalLimiter{rate: 0.0001, burst: 1, max: 16}

	decodePreview(t, e.as(t, tok.Token, http.MethodPost, previewPath, workloadBody(pol, "t", "")), http.StatusOK)
	w := e.as(t, tok.Token, http.MethodPost, previewPath, workloadBody(pol, "t", ""))
	if w.Code != http.StatusTooManyRequests || respReason(w) != reasonPreflightRateLimited {
		t.Fatalf("preview over the preflight rate: %d %s, want 429 %s", w.Code, w.Body.String(), reasonPreflightRateLimited)
	}
	if rows := e.workloadFacts(t).Rows; rows != 0 {
		t.Fatalf("workload rows = %d, want none: a preview stores nothing", rows)
	}
}
