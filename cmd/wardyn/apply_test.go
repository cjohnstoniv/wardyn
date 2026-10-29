// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

const (
	putWeb    = "PUT /api/v1/workloads/web"
	putWorker = "PUT /api/v1/workloads/worker"
	getWeb    = "GET /api/v1/workloads/web"
	preflight = "POST /api/v1/runs/preflight"
)

const webYAML = `api_version: wardyn/v1
kind: Workload
name: web
description: the web tier
intent:
  experience: background
  workload: {kind: command, command: make serve}
  no_repositories_or_drives: true
`

// workerCR is the same workload kind under the Kubernetes header.
const workerCR = `apiVersion: wardyn.dev/v1alpha1
kind: WardynWorkload
metadata:
  name: worker
  namespace: team-a
  labels: {app: worker}
spec:
  intent:
    experience: background
    workload: {kind: agent_task, agent: claude-code, task: triage the queue}
    tools:
      - {id: claude-code, kind: harness}
    no_repositories_or_drives: true
`

func applied(outcome sdk.WorkloadOutcome, launch sdk.WorkloadLaunch) func(http.ResponseWriter, []byte) {
	return replyJSON(http.StatusOK, sdk.WorkloadApplyResult{Workload: sdk.Workload{Name: "x"}, Outcome: outcome, Launch: launch})
}

func launchedRun(id uuid.UUID) sdk.WorkloadLaunch {
	return sdk.WorkloadLaunch{State: sdk.WorkloadLaunched, RunID: &id}
}

// TestApplyPutsEachDocumentUnderItsOwnName: a multi-document file with both headers makes one PUT
// per document, the custom resource's namespace and metadata stay out of the body, and the lines
// name the outcome and the run.
func TestApplyPutsEachDocumentUnderItsOwnName(t *testing.T) {
	webRun, workerRun := uuid.New(), uuid.New()
	rig := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
		putWeb:    applied(sdk.WorkloadCreated, launchedRun(webRun)),
		putWorker: applied(sdk.WorkloadUnchanged, sdk.WorkloadLaunch{State: sdk.WorkloadNoLaunch}),
	})
	file := writeTemplateFile(t, "w.yaml", webYAML+"---\n"+workerCR)
	out, err := rig.run(t, "apply", "-f", file)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	want := "workload/web created run " + webRun.String() + "\nworkload/worker unchanged\n"
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	var web, worker map[string]json.RawMessage
	if got := rig.sent(putWeb); len(got) != 1 || json.Unmarshal([]byte(got[0]), &web) != nil {
		t.Fatalf("web PUTs = %q", got)
	}
	if got := rig.sent(putWorker); len(got) != 1 || json.Unmarshal([]byte(got[0]), &worker) != nil {
		t.Fatalf("worker PUTs = %q", got)
	}
	if string(web["description"]) != `"the web tier"` || !strings.Contains(string(web["intent"]), `"make serve"`) {
		t.Errorf("web body = %v", web)
	}
	for _, k := range []string{"name", "kind", "api_version", "metadata", "namespace", "spec"} {
		if _, ok := web[k]; ok {
			t.Errorf("web body carries %q", k)
		}
		if _, ok := worker[k]; ok {
			t.Errorf("worker body carries %q", k)
		}
	}
	if !strings.Contains(string(worker["intent"]), "triage the queue") {
		t.Errorf("worker intent = %s", worker["intent"])
	}
}

// TestApplyKeepsADateAsWritten: an unquoted date reaches the body as the string the file wrote, not
// a time re-encoded as RFC 3339.
func TestApplyKeepsADateAsWritten(t *testing.T) {
	rig := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
		putWeb: applied(sdk.WorkloadUnchanged, sdk.WorkloadLaunch{State: sdk.WorkloadNoLaunch}),
	})
	if _, err := rig.run(t, "apply", "-f", writeTemplateFile(t, "w.yaml", webYAML+"restart_key: 2020-01-02\n")); err != nil {
		t.Fatal(err)
	}
	var web map[string]json.RawMessage
	if got := rig.sent(putWeb); len(got) != 1 || json.Unmarshal([]byte(got[0]), &web) != nil || string(web["restart_key"]) != `"2020-01-02"` {
		t.Fatalf("web PUTs = %q", got)
	}
}

// TestApplyReadsStdinAndJSON: '-' reads the file from stdin, and a JSON document reads as well.
func TestApplyReadsStdinAndJSON(t *testing.T) {
	rig := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
		putWeb: applied(sdk.WorkloadUpdated, sdk.WorkloadLaunch{State: sdk.WorkloadNoLaunch}),
	})
	doc := `{"api_version":"wardyn/v1","kind":"Workload","name":"web","intent":{"experience":"background","workload":{"kind":"command","command":"make serve"},"no_repositories_or_drives":true}}`
	root := rootCmd()
	root.SetArgs([]string{"apply", "-f", "-", "--url", rig.URL, "--token", "tok"})
	root.SetIn(strings.NewReader(doc))
	var out strings.Builder
	root.SetOut(&out)
	root.SetErr(&strings.Builder{})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "workload/web updated\n" {
		t.Errorf("output = %q", out.String())
	}
}

// TestApplyRefusedLaunchExitsOne: the spec is stored (a 200), the line says why the run was not
// started, and the exit status is 1 once every document has been applied.
func TestApplyRefusedLaunchExitsOne(t *testing.T) {
	rig := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
		putWeb:    applied(sdk.WorkloadUpdated, sdk.WorkloadLaunch{State: sdk.WorkloadRefused, Reason: "egress_ceiling", Message: "the policy asks for more egress than your ceiling"}),
		putWorker: applied(sdk.WorkloadCreated, launchedRun(uuid.New())),
	})
	out, err := rig.run(t, "apply", "-f", writeTemplateFile(t, "w.yaml", webYAML+"---\n"+workerCR))
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("err = %v, want exit 1", err)
	}
	if !strings.Contains(out, "workload/web refused egress_ceiling: the policy asks for more egress than your ceiling\n") {
		t.Errorf("output = %q", out)
	}
	if len(rig.sent(putWorker)) != 1 {
		t.Error("a refusal stopped the documents after it")
	}
}

// TestApplyStopsAtAnAPIError: a 4xx is the CLI-to-organisation exit (3), named by workload, and the
// documents after it are not sent.
func TestApplyStopsAtAnAPIError(t *testing.T) {
	rig := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
		putWeb: replyJSON(http.StatusBadRequest, map[string]string{"error": "workload_invalid"}),
	})
	_, err := rig.run(t, "apply", "-f", writeTemplateFile(t, "w.yaml", webYAML+"---\n"+workerCR))
	if err == nil || !strings.Contains(err.Error(), "workload/web") || exitCodeFor(err) != 3 {
		t.Fatalf("err = %v (exit %d)", err, exitCodeFor(err))
	}
	if n := len(rig.sent(putWorker)); n != 0 {
		t.Errorf("the document after the error was sent (%d)", n)
	}
}

// TestApplyRefusesAFileBeforeSendingAnything: a document the client cannot read, a repeated name,
// an empty file and a flag clash are all refused with no request made.
func TestApplyRefusesAFileBeforeSendingAnything(t *testing.T) {
	rig := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){})
	for name, tc := range map[string]struct{ src, want string }{
		"unknown field":  {webYAML + "colour: red\n", "document 1"},
		"second bad":     {webYAML + "---\nkind: Workload\n", "document 2"},
		"repeated name":  {webYAML + "---\n" + webYAML, "named twice"},
		"empty":          {"---\n---\n", "holds no document"},
		"not a workload": {"api_version: wardyn/v1\nkind: Run\nintent: {}\n", "is not a workload"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := rig.run(t, "apply", "-f", writeTemplateFile(t, "w.yaml", tc.src))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
	_, err := rig.run(t, "apply", "-f", writeTemplateFile(t, "w.yaml", webYAML), "--dry-run", "--wait")
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("--dry-run --wait = %v", err)
	}
	if _, err := rig.run(t, "apply"); err == nil || !strings.Contains(err.Error(), `"file"`) {
		t.Errorf("apply without -f = %v", err)
	}
	for _, timeout := range []string{"0s", "-1s"} {
		if _, err := rig.run(t, "apply", "-f", writeTemplateFile(t, "w.yaml", webYAML), "--wait", "--timeout="+timeout); err == nil || exitCodeFor(err) != 124 {
			t.Errorf("--wait --timeout %s = %v (exit %d), want 124", timeout, err, exitCodeFor(err))
		}
	}
	for key, bodies := range rig.bodies {
		t.Errorf("%s was requested: %q", key, bodies)
	}
}

// doorRoutes answers the import door as the server does for a document that decodes: the
// document it was given.
func doorRoutes(t *testing.T, routes map[string]func(http.ResponseWriter, []byte)) map[string]func(http.ResponseWriter, []byte) {
	t.Helper()
	routes[importPath] = func(w http.ResponseWriter, body []byte) {
		var req sdk.TemplateImportRequest
		_ = json.Unmarshal(body, &req)
		if req.Kind != sdk.RunDocumentKind || req.Format != sdk.TemplateFormatJSON {
			t.Errorf("the door was asked for %+v", req)
		}
		var doc sdk.TemplateDocument
		if err := json.Unmarshal([]byte(req.Source), &doc); err != nil {
			t.Errorf("stub door: %v", err)
		}
		replyJSON(http.StatusOK, sdk.TemplateImportResult{Document: &doc, Diagnostics: []sdk.TemplateDiagnostic{}})(w, nil)
	}
	return routes
}

// storedWeb is the workload `web` as the organisation holds it, from a document's YAML.
func storedWeb(t *testing.T, src string) sdk.Workload {
	t.Helper()
	doc, err := sdk.ParseWorkload(yamlToJSONForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	return sdk.Workload{Name: doc.Name, Owner: "person:me", Source: sdk.WorkloadSourceAPI, Description: doc.Description, RunIn: doc.RunIn, RestartKey: doc.RestartKey, Intent: doc.Intent}
}

// TestDiffExitCodes: equal is exit 0 with no output; a changed spec and a workload not stored yet
// are exit 1 with a unified diff, and nothing is written.
func TestDiffExitCodes(t *testing.T) {
	stored := storedWeb(t, webYAML)
	file := writeTemplateFile(t, "w.yaml", webYAML)
	run := func(get func(http.ResponseWriter, []byte)) (string, error, *routeRig) {
		rig := newRouteRig(t, doorRoutes(t, map[string]func(http.ResponseWriter, []byte){getWeb: get}))
		out, err := rig.run(t, "diff", "-f", file)
		return out, err, rig
	}

	out, err, rig := run(replyJSON(http.StatusOK, stored))
	if err != nil || out != "" {
		t.Errorf("equal: err = %v, output = %q", err, out)
	}
	for key := range rig.bodies {
		if strings.HasPrefix(key, "PUT ") || strings.HasPrefix(key, "POST /api/v1/runs") {
			t.Errorf("diff wrote with %s", key)
		}
	}

	changed := stored
	changed.Description = "an older description"
	out, err, _ = run(replyJSON(http.StatusOK, changed))
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Errorf("changed: err = %v, want exit 1", err)
	}
	for _, want := range []string{"--- workload/web (stored)\n+++ workload/web (file)\n", "-description: an older description\n", "+description: the web tier\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("changed: output lacks %q:\n%s", want, out)
		}
	}

	out, err, _ = run(replyJSON(http.StatusNotFound, map[string]string{"error": "workload_not_found"}))
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Errorf("not stored: err = %v, want exit 1", err)
	}
	if !strings.Contains(out, "--- workload/web (not stored)\n") || !strings.Contains(out, "+name: web\n") {
		t.Errorf("not stored: output = %q", out)
	}

	if _, err, _ = run(replyJSON(http.StatusInternalServerError, map[string]string{"error": "boom"})); err == nil || exitCodeFor(err) != 4 {
		t.Errorf("a server error = %v, want exit 4", err)
	}

	// A file that does not parse is an error, not a difference.
	bad := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){})
	if _, err := bad.run(t, "diff", "-f", writeTemplateFile(t, "w.yaml", webYAML+"colour: red\n")); err == nil || exitCodeFor(err) != 2 {
		t.Errorf("an unknown field = %v (exit %d), want exit 2", err, exitCodeFor(err))
	}
}

// TestDiffWritesNumbersAsNumbers: a number in the intent reads as the number in both sides of the
// diff, not as a quoted string.
func TestDiffWritesNumbersAsNumbers(t *testing.T) {
	src := func(sec int) string {
		return strings.Replace(webYAML, "  no_repositories_or_drives: true\n", fmt.Sprintf("  no_repositories_or_drives: true\n  ends_after_sec: %d\n", sec), 1)
	}
	rig := newRouteRig(t, doorRoutes(t, map[string]func(http.ResponseWriter, []byte){getWeb: replyJSON(http.StatusOK, storedWeb(t, src(7200)))}))
	out, err := rig.run(t, "diff", "-f", writeTemplateFile(t, "w.yaml", src(3600)))
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("err = %v, want exit 1", err)
	}
	for _, want := range []string{"-    ends_after_sec: 7200\n", "+    ends_after_sec: 3600\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// TestDiffReportsTheDoorsDiagnostics: a spec the door refuses is not compared.
func TestDiffReportsTheDoorsDiagnostics(t *testing.T) {
	rig := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
		importPath: replyJSON(http.StatusOK, sdk.TemplateImportResult{Diagnostics: []sdk.TemplateDiagnostic{
			{Path: "intent.title", Reason: "template_field_excluded", Message: "A run is named when it is launched."},
		}}),
	})
	_, err := rig.run(t, "diff", "-f", writeTemplateFile(t, "w.yaml", webYAML))
	if err == nil || !strings.Contains(err.Error(), "1 problem") || !strings.Contains(err.Error(), "workload/web") || exitCodeFor(err) != 2 {
		t.Fatalf("err = %v (exit %d), want exit 2", err, exitCodeFor(err))
	}
	if n := len(rig.sent(getWeb)); n != 0 {
		t.Errorf("the stored workload was read after a refusal (%d)", n)
	}
}

// TestApplyDryRunWritesNothing: --dry-run prints the diff and the run's setup checklist, with the
// workload's name as the run's title, and never PUTs.
func TestApplyDryRunWritesNothing(t *testing.T) {
	rig := newRouteRig(t, doorRoutes(t, map[string]func(http.ResponseWriter, []byte){
		getWeb:    replyJSON(http.StatusNotFound, map[string]string{"error": "workload_not_found"}),
		preflight: replyJSON(http.StatusOK, sdk.PreflightResult{EnforcedConfinementClass: types.CC2, SetupItems: []sdk.PreflightItem{}}),
	}))
	file := writeTemplateFile(t, "w.yaml", webYAML)
	out, err := rig.run(t, "apply", "-f", file, "--dry-run")
	if err != nil {
		t.Fatalf("apply --dry-run: %v", err)
	}
	for _, want := range []string{"+name: web\n", "dry run: not launched"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if n := len(rig.sent(putWeb)); n != 0 {
		t.Errorf("a dry run PUT the workload (%d)", n)
	}
	sent := rig.sent(preflight)
	if len(sent) != 1 {
		t.Fatalf("preflights = %d", len(sent))
	}
	var req sdk.CreateRunRequest
	if err := json.Unmarshal([]byte(sent[0]), &req); err != nil || req.Title != "web" || req.Description != "the web tier" || req.Workload == nil || req.Workload.Command != "make serve" {
		t.Errorf("preflight request = %+v (%v)", req, err)
	}

	out, err = rig.run(t, "apply", "-f", file, "--dry-run", "--json")
	var reports []workloadDryRunReport
	if err != nil || json.Unmarshal([]byte(out), &reports) != nil || len(reports) != 1 || !reports[0].Differs || reports[0].Preflight.EnforcedConfinementClass != types.CC2 {
		t.Errorf("--json: err = %v, output = %s", err, out)
	}
}

// TestDeleteFileDeletesEachWorkload.
func TestDeleteFileDeletesEachWorkload(t *testing.T) {
	rig := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
		"DELETE /api/v1/workloads/web":    replyJSON(http.StatusNoContent, nil),
		"DELETE /api/v1/workloads/worker": replyJSON(http.StatusNoContent, nil),
	})
	out, err := rig.run(t, "delete", "-f", writeTemplateFile(t, "w.yaml", webYAML+"---\n"+workerCR))
	if err != nil || out != "workload/web deleted\nworkload/worker deleted\n" {
		t.Errorf("delete -f: err = %v, output = %q", err, out)
	}
	rig = newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
		"DELETE /api/v1/workloads/web": replyJSON(http.StatusNotFound, map[string]string{"error": "workload_not_found"}),
	})
	if _, err := rig.run(t, "delete", "-f", writeTemplateFile(t, "w.yaml", webYAML)); err == nil || exitCodeFor(err) != 3 {
		t.Errorf("deleting a workload that is not there = %v", err)
	}
}

// TestApplyWaitFollowsTheRunToWhereItBelongs: a service is followed to RUNNING; an agent task and a
// command without a service block, which end by themselves, to COMPLETED; and a run that fails
// first exits 1 with its state.
func TestApplyWaitFollowsTheRunToWhereItBelongs(t *testing.T) {
	setWaitPollInterval(t, time.Millisecond)
	serviceYAML := strings.Replace(webYAML, "  no_repositories_or_drives: true\n", "  no_repositories_or_drives: true\n  service: {restart: on_failure}\n", 1)
	timeout := "30s"
	run := func(src string, state types.RunState) (string, error) {
		id := uuid.New()
		rig := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
			putWeb:                            applied(sdk.WorkloadCreated, launchedRun(id)),
			putWorker:                         applied(sdk.WorkloadCreated, launchedRun(id)),
			"GET /api/v1/runs/" + id.String(): replyJSON(http.StatusOK, types.AgentRun{ID: id, State: state}),
		})
		return rig.run(t, "apply", "-f", writeTemplateFile(t, "w.yaml", src), "--wait", "--timeout", timeout)
	}
	if _, err := run(serviceYAML, types.RunRunning); err != nil {
		t.Errorf("service running: %v", err)
	}
	if _, err := run(workerCR, types.RunCompleted); err != nil {
		t.Errorf("task completed: %v", err)
	}
	if _, err := run(webYAML, types.RunCompleted); err != nil {
		t.Errorf("command completed: %v", err)
	}
	var ee *exitError
	if _, err := run(serviceYAML, types.RunFailed); !errors.As(err, &ee) || ee.code != 1 || !strings.Contains(err.Error(), "FAILED") {
		t.Errorf("service failed: %v, want exit 1", err)
	}
	if _, err := run(serviceYAML, types.RunKilled); !errors.As(err, &ee) || ee.code != 2 {
		t.Errorf("service killed: %v, want exit 2", err)
	}
	if _, err := run(webYAML, types.RunFailed); !errors.As(err, &ee) || ee.code != 1 {
		t.Errorf("command failed: %v, want exit 1", err)
	}
	// A running command without a service block is not done: the wait goes on until it ends.
	timeout = "50ms"
	if _, err := run(webYAML, types.RunRunning); !errors.As(err, &ee) || ee.code != 124 {
		t.Errorf("command still running: %v, want the wait to time out (124)", err)
	}
}

// TestApplyWaitSkipsAWorkloadThatLaunchedNothing: --wait follows only the runs the apply started.
func TestApplyWaitSkipsAWorkloadThatLaunchedNothing(t *testing.T) {
	rig := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){
		putWeb: applied(sdk.WorkloadUnchanged, sdk.WorkloadLaunch{State: sdk.WorkloadNoLaunch}),
	})
	if _, err := rig.run(t, "apply", "-f", writeTemplateFile(t, "w.yaml", webYAML), "--wait"); err != nil {
		t.Fatal(err)
	}
}

// TestUnifiedDiff pins the text: line numbers, three lines of context, and a hunk per cluster of
// changes.
func TestUnifiedDiff(t *testing.T) {
	lines := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "line" + string(rune('A'+i%26)) + strings.Repeat("x", i/26)
		}
		return out
	}
	a := lines(30)
	if got := unifiedDiff("a", "b", a, a); got != "" {
		t.Errorf("equal text diffs as %q", got)
	}
	b := append([]string(nil), a...)
	b[2] = "changed2"
	b[25] = "changed25"
	got := unifiedDiff("a", "b", a, b)
	if strings.Count(got, "@@ -") != 2 {
		t.Errorf("want two hunks:\n%s", got)
	}
	for _, want := range []string{"--- a\n+++ b\n", "@@ -1,6 +1,6 @@\n", "-lineC\n+changed2\n", "@@ -23,7 +23,7 @@\n", "-lineZ\n+changed25\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("diff lacks %q:\n%s", want, got)
		}
	}
	// Two changes six lines apart share one hunk.
	b = append([]string(nil), a...)
	b[10], b[17] = "p", "q"
	if got := unifiedDiff("a", "b", a, b); strings.Count(got, "@@ -") != 1 {
		t.Errorf("changes within twice the context should share a hunk:\n%s", got)
	}
	// Insertion into nothing, and a pure deletion.
	if got := unifiedDiff("a", "b", nil, []string{"x", "y"}); !strings.Contains(got, "@@ -0,0 +1,2 @@\n+x\n+y\n") {
		t.Errorf("insertion = %q", got)
	}
	if got := unifiedDiff("a", "b", []string{"x", "y"}, []string{"y"}); !strings.Contains(got, "@@ -1,2 +1,1 @@\n-x\n y\n") {
		t.Errorf("deletion = %q", got)
	}
}
