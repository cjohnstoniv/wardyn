// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// previewLaunch is what POST /workloads/web/preview of body says the launch would meet, as c.
func previewLaunch(t *testing.T, e svcEnv, c *http.Cookie, body string) client.WorkloadPreviewLaunch {
	t.Helper()
	w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/workloads/web/preview", c, body)
	var pv client.WorkloadPreview
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &pv) != nil {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}
	return pv.Launch
}

// pinWeb pins the caller's workload web to digest and returns it as the answer reads.
func pinWeb(t *testing.T, e svcEnv, c *http.Cookie, digest string) client.Workload {
	t.Helper()
	w := doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web/policy-pin", c, `{"digest":"`+digest+`"}`)
	var out client.Workload
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("pin: %d %s", w.Code, w.Body.String())
	}
	return out
}

// TestPG_WorkloadPolicyPin_DriftIsRefusedBeforeTheRunningRunIsTouched: once the policy under a pinned
// workload's policy_id changes, its next launch is refused with the digest it now resolves to: no run is
// made, the running run keeps running and nothing replaces it, and the preview says so first. Pinning the
// new digest lets the owed launch go ahead.
func TestPG_WorkloadPolicyPin_DriftIsRefusedBeforeTheRunningRunIsTouched(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	pol := e.plainPolicy(t, "wl-pol")
	oldRun := launchedRun(t, decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t", "")), http.StatusCreated))
	next := workloadBody(pol, "t2", "")
	planned := previewLaunch(t, e, e.mem, next)
	if planned.State != client.WorkloadPreviewWouldLaunch || !workloadPolicyPinPattern.MatchString(planned.PolicyDigest) {
		t.Fatalf("preview of the next spec = %+v, want would_launch with a launch policy digest", planned)
	}
	if got := pinWeb(t, e, e.mem, planned.PolicyDigest); got.PolicyPin != planned.PolicyDigest {
		t.Fatalf("pinned workload reads pin %q, want %q", got.PolicyPin, planned.PolicyDigest)
	}

	stored, err := e.st.GetPolicy(ctx, uuid.MustParse(pol))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.UpdatePolicy(ctx, stored.ID, stored.UpdatedAt, stored.Name,
		types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com", "pypi.org"}}); err != nil {
		t.Fatal(err)
	}
	drifted := previewLaunch(t, e, e.mem, next)
	if drifted.State != client.WorkloadPreviewWouldRefuse || drifted.Reason != reasonWorkloadPolicyPinMismatch ||
		!workloadPolicyPinPattern.MatchString(drifted.PolicyDigest) || drifted.PolicyDigest == planned.PolicyDigest {
		t.Fatalf("preview after the policy changed = %+v, want would_refuse %s with another digest", drifted, reasonWorkloadPolicyPinMismatch)
	}
	var runsBefore, runsAfter int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM agent_runs`).Scan(&runsBefore); err != nil {
		t.Fatal(err)
	}
	res := decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, next), http.StatusOK)
	if res.Launch.State != client.WorkloadRefused || res.Launch.Reason != reasonWorkloadPolicyPinMismatch ||
		!strings.Contains(res.Launch.Message, planned.PolicyDigest) || !strings.Contains(res.Launch.Message, drifted.PolicyDigest) {
		t.Fatalf("launch = %+v, want refused %s naming the pin and the digest it resolves to", res.Launch, reasonWorkloadPolicyPinMismatch)
	}
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM agent_runs`).Scan(&runsAfter); err != nil || runsAfter != runsBefore {
		t.Fatalf("runs = %d after the refused launch, %d before (%v): want no run made", runsAfter, runsBefore, err)
	}
	if res.Workload.RunID == nil || *res.Workload.RunID != oldRun || res.Workload.LastRefusal == nil || res.Workload.LastRefusal.Reason != reasonWorkloadPolicyPinMismatch {
		t.Fatalf("workload run=%v last_refusal=%+v, want %s kept and the refusal recorded", res.Workload.RunID, res.Workload.LastRefusal, oldRun)
	}
	if run, err := e.st.GetRun(ctx, oldRun); err != nil || run.State == types.RunKilled {
		t.Fatalf("old run state = %q (%v), want it still running", run.State, err)
	}
	events := e.h.audit.snapshot()
	if countAudit(events, "workload.launch_refused") != 1 || countAudit(events, "workload.replace") != 0 || workloadAuditIndex(events, oldRun, "run.kill") >= 0 {
		t.Fatal("want one workload.launch_refused row, no workload.replace row and no run.kill of the running run")
	}

	pinWeb(t, e, e.mem, drifted.PolicyDigest)
	again := decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, next), http.StatusOK)
	newRun := launchedRun(t, again)
	if newRun == oldRun || workloadAuditIndex(e.h.audit.snapshot(), newRun, "workload.replace") < 0 {
		t.Fatalf("after re-pinning: run %s, want a new run that replaces %s", newRun, oldRun)
	}
}

// TestPG_WorkloadPolicyPin_ThePreviewThePinAndTheCreateAgree: a launch that takes hosts from a workspace and
// a component and asks for a size resolves one digest at the preview and at the create, though the preview
// holds the workspace's hosts and the main drive in its spec when the pin step runs and the create adds them
// later. Pinned off it, the launch is refused naming the preview's digest as the one it resolved; pinned to
// the preview's digest, it launches. A host the workspace gains afterwards refuses the next launch.
func TestPG_WorkloadPolicyPin_ThePreviewThePinAndTheCreateAgree(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	pol := e.plainPolicy(t, "wl-pol")
	ws, err := e.st.CreateWorkspace(ctx, types.Workspace{ID: uuid.New(), Name: "pin-ws", Status: types.WorkspaceScanned,
		Sources:        []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/pin-ws"}},
		ApprovedEgress: []string{"ws.pin.example"}})
	if err != nil {
		t.Fatal(err)
	}
	carries := `,"workspace_id":"` + ws.ID.String() + `","components":[{"name":"Pin Tool","inline":{"hosts":["tool.pin.example"]}}],` +
		`"resources":{"cpu_millis":1000,"memory_mib":1024,"disk_mib":2048}`
	apply := func(task string) client.WorkloadApplyResult {
		t.Helper()
		return decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, task, carries)), http.StatusOK)
	}
	first := launchedRun(t, decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t", carries)), http.StatusCreated))
	planned := previewLaunch(t, e, e.mem, workloadBody(pol, "t2", carries))
	if planned.State != client.WorkloadPreviewWouldLaunch || !workloadPolicyPinPattern.MatchString(planned.PolicyDigest) {
		t.Fatalf("preview = %+v, want would_launch with a launch policy digest", planned)
	}
	pinWeb(t, e, e.mem, strings.Repeat("0", 64))
	if res := apply("t2"); res.Launch.State != client.WorkloadRefused || res.Launch.Reason != reasonWorkloadPolicyPinMismatch ||
		!strings.Contains(res.Launch.Message, planned.PolicyDigest) {
		t.Fatalf("launch pinned off its policy = %+v, want refused %s naming %s, the preview's digest, as the one it resolved",
			res.Launch, reasonWorkloadPolicyPinMismatch, planned.PolicyDigest)
	}
	pinWeb(t, e, e.mem, planned.PolicyDigest)
	second := launchedRun(t, apply("t2"))
	if second == first {
		t.Fatalf("on the preview's digest: run %s, want a new run", second)
	}

	if _, err := e.st.SetWorkspaceApprovedEgress(ctx, ws.ID, []string{"ws.pin.example", "more.pin.example"}); err != nil {
		t.Fatal(err)
	}
	drifted := previewLaunch(t, e, e.mem, workloadBody(pol, "t3", carries))
	if drifted.State != client.WorkloadPreviewWouldRefuse || drifted.Reason != reasonWorkloadPolicyPinMismatch || drifted.PolicyDigest == planned.PolicyDigest {
		t.Fatalf("preview after the workspace gained a host = %+v, want would_refuse %s with another digest", drifted, reasonWorkloadPolicyPinMismatch)
	}
	res := apply("t3")
	if res.Launch.State != client.WorkloadRefused || res.Launch.Reason != reasonWorkloadPolicyPinMismatch || res.Workload.RunID == nil || *res.Workload.RunID != second {
		t.Fatalf("launch after the workspace gained a host = %+v, run %v; want refused %s and %s kept", res.Launch, res.Workload.RunID, reasonWorkloadPolicyPinMismatch, second)
	}
}

// TestPG_WorkloadPolicyPin_ARunnerCutLeavesTheDigest: a workload asks more CPU and memory than the one runner
// of its pool gives a run, so placement cuts the launch to the runner's per-run limit. The digest holds the
// size asked, not the cut: once the runner gives more, the same launch resolves the same digest and its pin
// still launches it. Another size asked is another digest.
func TestPG_WorkloadPolicyPin_ARunnerCutLeavesTheDigest(t *testing.T) {
	e := newPlacementEnv(t)
	ctx := context.Background()
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	id := e.laptop(e.personP, 1000, 1024, types.CC1, types.CC2)
	e.addToPool(e.person, laptops.ID, id)
	e.stream(id, types.CC1, types.CC2)
	e.personDefaults(e.person, `{"pool_id":"`+laptops.ID.String()+`"}`)
	body := func(task string, cpu int) string {
		return `{"intent":{"agent":"claude-code","task":"` + task + `","resources":{"cpu_millis":` + strconv.Itoa(cpu) + `}}}`
	}
	preview := func(b string) client.WorkloadPreviewLaunch {
		t.Helper()
		var pv client.WorkloadPreview
		if w := e.call(e.person, http.MethodPost, "/api/v1/workloads/web/preview", b); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &pv) != nil {
			t.Fatalf("preview: %d %s", w.Code, w.Body.String())
		}
		return pv.Launch
	}
	cpuApplied := func(l client.WorkloadPreviewLaunch) int {
		if l.Preflight == nil || l.Preflight.ResourcesApplied == nil {
			t.Fatalf("preview %+v carries no applied size", l)
		}
		return l.Preflight.ResourcesApplied.CPUMillis.Applied
	}
	launchedRun(t, decodeApply(t, e.call(e.person, http.MethodPut, "/api/v1/workloads/web", body("t", 1500)), http.StatusCreated))
	cut := preview(body("t2", 1500))
	if cut.State != client.WorkloadPreviewWouldLaunch || !workloadPolicyPinPattern.MatchString(cut.PolicyDigest) || cpuApplied(cut) != 1000 {
		t.Fatalf("preview = %+v, want would_launch at the runner's 1000 millicores with a launch policy digest", cut)
	}
	if w := e.call(e.person, http.MethodPut, "/api/v1/workloads/web/policy-pin", `{"digest":"`+cut.PolicyDigest+`"}`); w.Code != http.StatusOK {
		t.Fatalf("pin: %d %s", w.Code, w.Body.String())
	}

	e.runner.runnerFacts[id.String()] = runner.TargetFacts{Classes: []types.ConfinementClass{types.CC1, types.CC2}, CapacityAdvertised: true, CPUMillisMax: 4000, MemoryMiBMax: 8192}
	if _, err := e.pool.Exec(ctx, `UPDATE runners SET run_cpu_millis = 4000, run_memory_mib = 8192 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	whole := preview(body("t2", 1500))
	if cpuApplied(whole) != 1500 || whole.State != client.WorkloadPreviewWouldLaunch || whole.PolicyDigest != cut.PolicyDigest {
		t.Fatalf("preview once the runner gives more = %+v, want would_launch at 1500 millicores with the pinned digest %s", whole, cut.PolicyDigest)
	}
	launchedRun(t, decodeApply(t, e.call(e.person, http.MethodPut, "/api/v1/workloads/web", body("t2", 1500)), http.StatusOK))
	if asked := preview(body("t3", 1200)); asked.State != client.WorkloadPreviewWouldRefuse || asked.Reason != reasonWorkloadPolicyPinMismatch {
		t.Fatalf("preview of another size asked = %+v, want would_refuse %s", asked, reasonWorkloadPolicyPinMismatch)
	}
}

// TestPG_WorkloadPolicyPin_TheRouteIsTheOwnersAndRecordsEachChange: the owner pins and unpins; each change
// is one workload.policy_pin.set row naming the pin it replaced, and a repeat writes none. A malformed digest
// is refused, another person reaches no workload of the owner's, even one who holds a workload of that name
// (theirs is not written either), and a reader who sees the intent with its host axis withheld does not see
// the pin.
func TestPG_WorkloadPolicyPin_TheRouteIsTheOwnersAndRecordsEachChange(t *testing.T) {
	e := newSvcEnv(t)
	pol := e.plainPolicy(t, "wl-pol")
	decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t", "")), http.StatusCreated)
	pin := strings.Repeat("ab", 32)
	pinWeb(t, e, e.mem, pin)
	if got := pinWeb(t, e, e.mem, pin); got.PolicyPin != pin {
		t.Fatalf("repeated pin reads %q, want %q", got.PolicyPin, pin)
	}
	for _, bad := range []string{strings.ToUpper(pin), pin[:63], ""} {
		w := doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web/policy-pin", e.mem, `{"digest":"`+bad+`"}`)
		if w.Code != http.StatusBadRequest || respReason(w) != reasonWorkloadPolicyPinInvalid {
			t.Fatalf("pin %q: %d %s, want 400 %s", bad, w.Code, w.Body.String(), reasonWorkloadPolicyPinInvalid)
		}
	}
	decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.other, workloadBody(pol, "t", "")), http.StatusCreated)
	for who, c := range map[string]*http.Cookie{"another person": e.other, "the security tier": e.sec, "an administrator": e.admin} {
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			w := doSSO(t, e.h.srv, method, "/api/v1/workloads/web/policy-pin?owner=mem", c, `{"digest":"`+strings.Repeat("0", 64)+`"}`)
			if w.Code != http.StatusNotFound || respReason(w) != reasonWorkloadNotFound {
				t.Fatalf("%s by %s naming the owner: %d %s, want 404 %s", method, who, w.Code, w.Body.String(), reasonWorkloadNotFound)
			}
		}
	}
	read := func(c *http.Cookie, path string) client.Workload {
		t.Helper()
		var out client.Workload
		if w := doSSO(t, e.h.srv, http.MethodGet, path, c, ""); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
		}
		return out
	}
	if got := read(e.mem, "/api/v1/workloads/web").PolicyPin; got != pin {
		t.Fatalf("owner reads pin %q, want %q", got, pin)
	}
	if got := read(e.other, "/api/v1/workloads/web").PolicyPin; got != "" {
		t.Fatalf("the other person's own web reads pin %q, want none: a route naming another owner wrote it", got)
	}
	if got := read(e.sec, "/api/v1/workloads/web?owner=mem").PolicyPin; got != "" {
		t.Fatalf("security tier reads pin %q, want it withheld", got)
	}
	for range 2 {
		w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/workloads/web/policy-pin", e.mem, "")
		var out client.Workload
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.PolicyPin != "" {
			t.Fatalf("unpin: %d %s, want 200 with no pin", w.Code, w.Body.String())
		}
	}
	type pinRow struct {
		PolicyPin string `json:"policy_pin"`
		Previous  string `json:"previous"`
	}
	var rows []pinRow
	for _, ev := range e.h.audit.snapshot() {
		if ev.Action == "workload.policy_pin.set" {
			var d pinRow
			if err := json.Unmarshal(ev.Data, &d); err != nil || ev.Outcome != "success" || ev.Target != "mem/web" {
				t.Fatalf("workload.policy_pin.set row %+v: %v", ev, err)
			}
			rows = append(rows, d)
		}
	}
	if len(rows) != 2 || rows[0] != (pinRow{PolicyPin: pin}) || rows[1] != (pinRow{Previous: pin}) {
		t.Fatalf("workload.policy_pin.set rows = %+v, want the pin over none, then none over the pin", rows)
	}
}

// TestPG_WorkloadPolicyPin_AQueuedReplacementIsHeldToThePinAtItsStart: a replacement that waits in its pool's
// queue keeps the pin it was created under, and its start refuses it when its policy no longer resolves to
// that pin: the running run keeps running, and the workload takes it back and records the refusal.
func TestPG_WorkloadPolicyPin_AQueuedReplacementIsHeldToThePinAtItsStart(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	occupant, running := e.occupy(pool), e.occupy(e.cappedPool("Old", 1))
	intent := e.holdingWorkload(pool, running)
	next := `{"intent":` + intent("t2") + `}`
	var pv client.WorkloadPreview
	if w := e.call(e.person, http.MethodPost, "/api/v1/workloads/web/preview", next); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &pv) != nil ||
		!workloadPolicyPinPattern.MatchString(pv.Launch.PolicyDigest) {
		t.Fatalf("preview: %d %s, want a launch policy digest", w.Code, w.Body.String())
	}
	if w := e.call(e.person, http.MethodPut, "/api/v1/workloads/web/policy-pin", `{"digest":"`+pv.Launch.PolicyDigest+`"}`); w.Code != http.StatusOK {
		t.Fatalf("pin: %d %s", w.Code, w.Body.String())
	}
	queued := launchedRun(t, decodeApply(t, e.call(e.person, http.MethodPut, "/api/v1/workloads/web", next), http.StatusOK))
	if got := e.run(queued).State; got != types.RunQueued {
		t.Fatalf("replacement state = %s, want QUEUED in the full pool", got)
	}
	// What the start resolves is no longer the pin the launch carried.
	e.exec(`UPDATE run_queue SET authority = jsonb_set(authority, '{policy_pin}', to_jsonb($2::text)) WHERE run_id = $1`, queued, strings.Repeat("0", 64))
	e.free(occupant)
	if got := e.waitFailed(queued); got.StatusDetail != "queue: "+reasonWorkloadPolicyPinMismatch {
		t.Fatalf("replacement status_detail = %q, want its start refused for the pin", got.StatusDetail)
	}
	e.settleWorkloadRunEnds()
	if got := e.run(running.ID).State; got != types.RunRunning {
		t.Fatalf("old run state = %s after its replacement was refused, want RUNNING", got)
	}
	v, err := e.pg.GetWorkload(context.Background(), e.personP, "web")
	if err != nil {
		t.Fatal(err)
	}
	if v.CurrentRunID == nil || *v.CurrentRunID != running.ID || v.LastRefusal == nil || v.LastRefusal.Reason != reasonWorkloadPolicyPinMismatch {
		t.Fatalf("workload run=%v last_refusal=%+v, want %s back and the pin refusal recorded", v.CurrentRunID, v.LastRefusal, running.ID)
	}
}

// TestPG_WorkloadPolicyPin_AQueuedReplacementOnItsPinStarts: a replacement that waits in its pool's queue
// under the pin its policy resolves to starts once a slot frees. The create and the start resolve one digest,
// so a pin never refuses a queued launch whose policy has not moved.
func TestPG_WorkloadPolicyPin_AQueuedReplacementOnItsPinStarts(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	occupant, running := e.occupy(pool), e.occupy(e.cappedPool("Old", 1))
	next := `{"intent":` + e.holdingWorkload(pool, running)("t2") + `}`
	var pv client.WorkloadPreview
	if w := e.call(e.person, http.MethodPost, "/api/v1/workloads/web/preview", next); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &pv) != nil {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}
	if w := e.call(e.person, http.MethodPut, "/api/v1/workloads/web/policy-pin", `{"digest":"`+pv.Launch.PolicyDigest+`"}`); w.Code != http.StatusOK {
		t.Fatalf("pin: %d %s", w.Code, w.Body.String())
	}
	queued := launchedRun(t, decodeApply(t, e.call(e.person, http.MethodPut, "/api/v1/workloads/web", next), http.StatusOK))
	if n := e.count(`SELECT count(*) FROM run_queue WHERE run_id = $1 AND authority ->> 'policy_pin' = $2`, queued, pv.Launch.PolicyDigest); n != 1 {
		t.Fatalf("queue rows that carry the pin = %d, want 1", n)
	}
	e.free(occupant)
	if got := e.waitStarted(queued); got.State == types.RunFailed {
		t.Fatalf("replacement = %s %q, want it started under its pin", got.State, got.StatusDetail)
	}
}
