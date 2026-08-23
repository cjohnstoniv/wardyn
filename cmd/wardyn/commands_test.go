// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// These tests drive the real cobra command tree built by rootCmd() end to end:
// they set argv, point --url at an httptest server, and assert the request the
// command actually produced (method/path/body) plus flag/env precedence,
// default values, and how API errors surface as a non-nil Execute() error
// (which main() turns into exit code 1). Everything is hermetic — the only
// "network" is a localhost httptest server.

// recordedReq is what the command-test server captures about a request.
type recordedReq struct {
	method string
	path   string
	query  string
	auth   string
	ctype  string
	body   []byte
}

// cmdServer is an httptest server that records every request it receives and
// replies with a canned status + JSON body.
type cmdServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []recordedReq
}

func (s *cmdServer) last() recordedReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reqs) == 0 {
		return recordedReq{}
	}
	return s.reqs[len(s.reqs)-1]
}

// newCmdServer starts a server that replies with respStatus and respBody for
// every request. respBody is JSON-encoded when non-nil.
func newCmdServer(t *testing.T, respStatus int, respBody any) *cmdServer {
	t.Helper()
	cs := &cmdServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cs.mu.Lock()
		cs.reqs = append(cs.reqs, recordedReq{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			auth: r.Header.Get("Authorization"), ctype: r.Header.Get("Content-Type"), body: body,
		})
		cs.mu.Unlock()
		if respBody != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(respStatus)
			_ = json.NewEncoder(w).Encode(respBody)
			return
		}
		w.WriteHeader(respStatus)
	}))
	t.Cleanup(cs.Close)
	return cs
}

// execCmd runs the wardyn root command with the given args, discarding output.
// It returns the error Execute() would return (which main() maps to exit 1).
func execCmd(t *testing.T, args ...string) error {
	t.Helper()
	return execCmdStdin(t, "", args...)
}

// execCmdStdin is execCmd with `in` as the command's stdin — what
// cmd.InOrStdin() reads, so a stdin-only command (secret set) is drivable
// without swapping the process's os.Stdin.
func execCmdStdin(t *testing.T, in string, args ...string) error {
	t.Helper()
	root := rootCmd()
	root.SetArgs(args)
	root.SetIn(strings.NewReader(in))
	root.SetOut(&strings.Builder{})
	root.SetErr(&strings.Builder{})
	return root.Execute()
}

// --------------------------------------------------------------------------
// run command
// --------------------------------------------------------------------------

func TestRunCmd_BuildsCreateRequest(t *testing.T) {
	srv := newCmdServer(t, http.StatusCreated, types.AgentRun{
		ID: uuid.New(), State: types.RunPending, ConfinementClass: types.CC2,
	})

	// A real UUID: policy_id is a *uuid.UUID on the server, so anything else
	// could only ever have produced an opaque "invalid JSON body" 400.
	policyID := uuid.New()
	err := execCmd(t, "run",
		"--url", srv.URL, "--token", "tok",
		"--repo", "org/name", "--agent", "claude-code",
		"--task", "do the thing", "--policy", policyID.String(),
		"--confinement", "CC2", "--interactive")
	if err != nil {
		t.Fatalf("run command returned error: %v", err)
	}
	got := srv.last()
	if got.method != http.MethodPost || got.path != "/api/v1/runs" {
		t.Errorf("got %s %s, want POST /api/v1/runs", got.method, got.path)
	}
	if got.auth != "Bearer tok" {
		t.Errorf("auth = %q, want Bearer tok", got.auth)
	}
	if got.ctype != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got.ctype)
	}
	var body map[string]any
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["repo"] != "org/name" || body["agent"] != "claude-code" || body["task"] != "do the thing" {
		t.Errorf("run body repo/agent/task wrong: %v", body)
	}
	if body["policy_id"] != policyID.String() || body["confinement_class"] != "CC2" || body["interactive"] != true {
		t.Errorf("run body policy/confinement/interactive wrong: %v", body)
	}
}

// --confinement also accepts the friendly UI names (fence/wall/vault, case-
// insensitive) as aliases for CC1/CC2/CC3, so a CLI/CI caller can script what
// the console shows instead of memorizing wire codes (IFC1).
func TestRunCmd_ConfinementAlias(t *testing.T) {
	srv := newCmdServer(t, http.StatusCreated, types.AgentRun{
		ID: uuid.New(), State: types.RunPending, ConfinementClass: types.CC1,
	})

	err := execCmd(t, "run", "--url", srv.URL, "--token", "tok",
		"--agent", "claude-code", "--confinement", "Fence")
	if err != nil {
		t.Fatalf("run command returned error: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(srv.last().body, &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["confinement_class"] != "CC1" {
		t.Errorf("confinement_class = %v, want CC1 (normalized from the fence alias)", body["confinement_class"])
	}
}

// --dry-run posts the SAME body to the preflight endpoint and launches nothing.
// The devcontainer flags ride along here because they are part of that body: a
// dry run that checked a different body than launch would post is worthless.
func TestRunCmd_DryRunPreflightsInsteadOfLaunching(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, map[string]any{
		"enforced_confinement_class": "CC3",
		"setup_items":                []map[string]string{{"kind": "secret", "status": "missing", "required_by": "claude-code"}},
	})

	err := execCmd(t, "run", "--url", srv.URL, "--token", "tok",
		"--agent", "claude-code", "--dry-run",
		"--devcontainer-repo", "org/env", "--devcontainer-ref", "v2")
	if err != nil {
		t.Fatalf("run --dry-run returned error: %v", err)
	}
	got := srv.last()
	if got.method != http.MethodPost || got.path != "/api/v1/runs/preflight" {
		t.Errorf("got %s %s, want POST /api/v1/runs/preflight", got.method, got.path)
	}
	var body map[string]any
	_ = json.Unmarshal(got.body, &body)
	if body["devcontainer_repo"] != "org/env" || body["devcontainer_ref"] != "v2" {
		t.Errorf("devcontainer fields missing from the preflight body: %v", body)
	}
	srv.mu.Lock()
	n := len(srv.reqs)
	srv.mu.Unlock()
	if n != 1 {
		t.Errorf("server saw %d requests, want exactly 1 (a dry run must never POST /runs)", n)
	}
}

// printPreflight's table must include LABEL: it's the only field the server
// populates with the actual identifying text for a setup item (e.g. "Workspace
// secret: <name>"); KIND is just a shared coarse category like "workspace_secret".
// Without LABEL, a dry-run listing two missing "secret" rows is indistinguishable.
func TestRunCmd_DryRunPrintsSetupItemLabel(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, map[string]any{
		"enforced_confinement_class": "CC3",
		"setup_items": []map[string]string{
			{"kind": "secret", "status": "missing", "required_by": "claude-code", "label": "Workspace secret: GITHUB_TOKEN"},
		},
	})

	out := captureStdout(t, func() {
		if err := execCmd(t, "run", "--url", srv.URL, "--token", "tok",
			"--agent", "claude-code", "--dry-run"); err != nil {
			t.Fatalf("run --dry-run returned error: %v", err)
		}
	})
	if !strings.Contains(out, "Workspace secret: GITHUB_TOKEN") {
		t.Errorf("printPreflight output missing setup item label, got:\n%s", out)
	}
}

// run grants reaches the SDK's ListGrants route (the eligibility records the
// console shows and the CLI previously could not reach at all).
func TestRunCmd_Grants(t *testing.T) {
	id := uuid.New()
	srv := newCmdServer(t, http.StatusOK, []types.CredentialGrant{
		{ID: uuid.New(), RunID: id, Spec: types.GrantSpec{Kind: types.GrantGitHubToken, RequiresApproval: true}},
	})

	if err := execCmd(t, "run", "grants", id.String(), "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("run grants returned error: %v", err)
	}
	got := srv.last()
	if got.method != http.MethodGet || got.path != "/api/v1/runs/"+id.String()+"/grants" {
		t.Errorf("got %s %s, want GET /api/v1/runs/{id}/grants", got.method, got.path)
	}
}

// A --policy that isn't a UUID fails with a clear error BEFORE any request,
// rather than posting a body the server can only reject as "invalid JSON body".
func TestRunCmd_RejectsMalformedPolicyID(t *testing.T) {
	srv := newCmdServer(t, http.StatusCreated, types.AgentRun{})

	err := execCmd(t, "run", "--url", srv.URL, "--token", "tok",
		"--agent", "claude-code", "--policy", "pol-9")
	if err == nil {
		t.Fatal("expected error for a non-UUID --policy, got nil")
	}
	if !strings.Contains(err.Error(), "parse --policy") {
		t.Errorf("error = %q, want it to name --policy", err)
	}
	srv.mu.Lock()
	n := len(srv.reqs)
	srv.mu.Unlock()
	if n != 0 {
		t.Errorf("server saw %d requests, want 0 (validation must short-circuit)", n)
	}
}

// run requires only --agent now (--repo is optional: an ephemeral scratch run).
// A missing --agent must fail BEFORE any request; a missing --repo must NOT.
func TestRunCmd_RequiresAgentOnly(t *testing.T) {
	srv := newCmdServer(t, http.StatusCreated, types.AgentRun{})

	// Missing --agent → error, no request. --agent is now a cobra required
	// flag, so the error is cobra's standard required-flag message.
	err := execCmd(t, "run", "--url", srv.URL, "--token", "tok", "--repo", "org/name")
	if err == nil {
		t.Fatal("expected error when --agent missing, got nil")
	}
	if !strings.Contains(err.Error(), `required flag(s) "agent" not set`) {
		t.Errorf("error = %q, want the required-flag message", err)
	}
	srv.mu.Lock()
	n := len(srv.reqs)
	srv.mu.Unlock()
	if n != 0 {
		t.Errorf("server saw %d requests, want 0 (validation must short-circuit)", n)
	}

	// Missing --repo but --agent present → the request fires (ephemeral run).
	if err := execCmd(t, "run", "--url", srv.URL, "--token", "tok", "--agent", "claude-code"); err != nil {
		t.Fatalf("run with no --repo should succeed (ephemeral), got: %v", err)
	}
	got := srv.last()
	if got.method != http.MethodPost || got.path != "/api/v1/runs" {
		t.Errorf("got %s %s, want POST /api/v1/runs", got.method, got.path)
	}
	var body map[string]any
	_ = json.Unmarshal(got.body, &body)
	if body["repo"] != "" {
		t.Errorf("repo = %v, want empty for an ephemeral run", body["repo"])
	}
}

// --policy-file reads a JSON RunPolicySpec and sends it as inline_policy on the body.
func TestRunCmd_PolicyFileInlinePolicy(t *testing.T) {
	srv := newCmdServer(t, http.StatusCreated, types.AgentRun{
		ID: uuid.New(), State: types.RunPending, ConfinementClass: types.CC1,
	})

	dir := t.TempDir()
	file := dir + "/spec.json"
	writeFile(t, file, `{"allowed_domains":["example.com"],"first_use_approval":"always_deny","min_confinement_class":"CC1"}`)

	if err := execCmd(t, "run", "--url", srv.URL, "--token", "tok",
		"--agent", "claude-code", "--policy-file", file); err != nil {
		t.Fatalf("run --policy-file returned error: %v", err)
	}
	got := srv.last()
	if got.method != http.MethodPost || got.path != "/api/v1/runs" {
		t.Errorf("got %s %s, want POST /api/v1/runs", got.method, got.path)
	}
	var body map[string]any
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	inline, ok := body["inline_policy"].(map[string]any)
	if !ok {
		t.Fatalf("inline_policy missing/not an object: %v", body["inline_policy"])
	}
	if inline["min_confinement_class"] != "CC1" {
		t.Errorf("inline_policy.min_confinement_class = %v, want CC1", inline["min_confinement_class"])
	}
	domains, _ := inline["allowed_domains"].([]any)
	if len(domains) != 1 || domains[0] != "example.com" {
		t.Errorf("inline_policy.allowed_domains = %v, want [example.com]", inline["allowed_domains"])
	}
}

// A --policy-file that doesn't parse fails with a clear error BEFORE any request.
func TestRunCmd_PolicyFileParseError(t *testing.T) {
	srv := newCmdServer(t, http.StatusCreated, types.AgentRun{})

	dir := t.TempDir()
	file := dir + "/bad.json"
	writeFile(t, file, `{not valid json`)

	err := execCmd(t, "run", "--url", srv.URL, "--token", "tok", "--agent", "claude-code", "--policy-file", file)
	if err == nil {
		t.Fatal("expected error for an unparseable --policy-file, got nil")
	}
	if !strings.Contains(err.Error(), "parse --policy-file") {
		t.Errorf("error = %q, want a parse error", err)
	}
	srv.mu.Lock()
	n := len(srv.reqs)
	srv.mu.Unlock()
	if n != 0 {
		t.Errorf("server saw %d requests, want 0 (parse must short-circuit)", n)
	}
}

// TestRunCmd_PolicyFileRejectsUnknownField is the W14-S1-2 regression: a
// misspelled/unknown spec field in --policy-file used to be silently dropped
// (json.Unmarshal ignores what it doesn't recognize), so the run launched
// under a policy the operator believed enforced a setting it never carried.
// It must now fail locally, before any request, exactly like `policy render`.
func TestRunCmd_PolicyFileRejectsUnknownField(t *testing.T) {
	srv := newCmdServer(t, http.StatusCreated, types.AgentRun{})

	dir := t.TempDir()
	file := dir + "/typo.json"
	writeFile(t, file, `{"allowed_domains":["example.com"],"min_confinement_klass":"CC1"}`)

	err := execCmd(t, "run", "--url", srv.URL, "--token", "tok",
		"--agent", "claude-code", "--policy-file", file)
	if err == nil {
		t.Fatal("expected an error for an unknown spec field, got nil")
	}
	if !strings.Contains(err.Error(), "parse --policy-file") {
		t.Errorf("error = %q, want a parse error", err)
	}
	srv.mu.Lock()
	n := len(srv.reqs)
	srv.mu.Unlock()
	if n != 0 {
		t.Errorf("server saw %d requests, want 0 (an unknown field must short-circuit before launch)", n)
	}
}

func TestRunCmd_ImageAndTaskModeInBody(t *testing.T) {
	srv := newCmdServer(t, http.StatusCreated, types.AgentRun{ID: uuid.New(), State: types.RunPending})

	err := execCmd(t, "run", "--url", srv.URL, "--token", "tok",
		"--agent", "byoa", "--image", "ubuntu:24.04", "--task", "make test", "--task-mode", "exec")
	if err != nil {
		t.Fatalf("run command returned error: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(srv.last().body, &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["image"] != "ubuntu:24.04" || body["task_mode"] != "exec" {
		t.Errorf("run body image/task_mode wrong: %v", body)
	}
}

func TestRunCmd_WaitInteractiveConflict(t *testing.T) {
	srv := newCmdServer(t, http.StatusCreated, types.AgentRun{})

	err := execCmd(t, "run", "--url", srv.URL, "--token", "tok",
		"--agent", "claude-code", "--interactive", "--wait")
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v, want a --wait/--interactive conflict error", err)
	}
	srv.mu.Lock()
	n := len(srv.reqs)
	srv.mu.Unlock()
	if n != 0 {
		t.Errorf("server saw %d requests, want 0 (conflict must short-circuit)", n)
	}
}

// waitServer routes create/get/audit like the real API so --wait's poll loop
// can be driven through a scripted sequence of run states.
func waitServer(t *testing.T, runID uuid.UUID, states []types.RunState, audit []types.AuditEvent) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/runs":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(types.AgentRun{ID: runID, State: types.RunPending})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runs/"+runID.String():
			mu.Lock()
			i := polls
			if i >= len(states) {
				i = len(states) - 1 // pin on the last scripted state
			}
			polls++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(types.AgentRun{ID: runID, State: states[i]})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/audit":
			_ = json.NewEncoder(w).Encode(audit)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func setWaitPollInterval(t *testing.T, d time.Duration) {
	t.Helper()
	old := waitPollInterval
	waitPollInterval = d
	t.Cleanup(func() { waitPollInterval = old })
}

func TestRunCmd_WaitRunningThenCompleted(t *testing.T) {
	setWaitPollInterval(t, time.Millisecond)
	runID := uuid.New()
	srv := waitServer(t, runID, []types.RunState{types.RunRunning, types.RunCompleted}, nil)

	err := execCmd(t, "run", "--url", srv.URL, "--token", "tok", "--agent", "claude-code", "--wait")
	if err != nil {
		t.Fatalf("run --wait on a COMPLETED run returned error: %v", err)
	}
}

func TestRunCmd_WaitFailedPropagatesAgentExitCode(t *testing.T) {
	setWaitPollInterval(t, time.Millisecond)
	runID := uuid.New()
	srv := waitServer(t, runID, []types.RunState{types.RunRunning, types.RunFailed}, []types.AuditEvent{
		{Action: "run.exec", Data: json.RawMessage(`{}`)},
		{Action: "run.complete", Data: json.RawMessage(`{"exit_code":3,"state":"FAILED"}`)},
	})

	err := execCmd(t, "run", "--url", srv.URL, "--token", "tok", "--agent", "claude-code", "--wait")
	var ee *exitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *exitError", err)
	}
	if ee.code != 3 {
		t.Errorf("exit code = %d, want the agent's real exit code 3", ee.code)
	}
}

func TestRunFailureReason(t *testing.T) {
	runID := uuid.New()

	// A dispatch failure (e.g. an unpullable image from an unknown --agent) records
	// outcome "failure" with a human error in Data — the reason otherwise buried in
	// `audit --json` and invisible on run get/--wait.
	srv := waitServer(t, runID, []types.RunState{types.RunFailed}, []types.AuditEvent{
		{Action: "run.exec", Outcome: "success", Data: json.RawMessage(`{}`)},
		{Action: "run.dispatch", Outcome: "failure", Data: json.RawMessage(`{"error":"pull ghcr.io/x/agent-oracle:latest: not found"}`)},
	})
	got := runFailureReason(t.Context(), sdk.New(srv.URL, "tok"), runID)
	if want := "run.dispatch: pull ghcr.io/x/agent-oracle:latest: not found"; got != want {
		t.Errorf("runFailureReason = %q, want %q", got, want)
	}

	// A plain nonzero agent exit emits a run.complete FAILURE event, but its Data
	// carries only exit_code/state — no error/reason/detail — so the helper stays
	// silent (the exit code is the whole story; don't manufacture noise).
	srv2 := waitServer(t, runID, []types.RunState{types.RunFailed}, []types.AuditEvent{
		{Action: "run.complete", Outcome: "failure", Data: json.RawMessage(`{"exit_code":3,"state":"FAILED"}`)},
	})
	if got := runFailureReason(t.Context(), sdk.New(srv2.URL, "tok"), runID); got != "" {
		t.Errorf("runFailureReason on a plain exit = %q, want empty", got)
	}
}

func TestRunCmd_WaitFailedNoAuditFallsBackTo1(t *testing.T) {
	setWaitPollInterval(t, time.Millisecond)
	runID := uuid.New()
	srv := waitServer(t, runID, []types.RunState{types.RunFailed}, nil)

	err := execCmd(t, "run", "--url", srv.URL, "--token", "tok", "--agent", "claude-code", "--wait")
	var ee *exitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *exitError", err)
	}
	if ee.code != 1 {
		t.Errorf("exit code = %d, want fallback 1", ee.code)
	}
}

func TestRunCmd_WaitKilledExits2(t *testing.T) {
	setWaitPollInterval(t, time.Millisecond)
	runID := uuid.New()
	srv := waitServer(t, runID, []types.RunState{types.RunKilled}, nil)

	err := execCmd(t, "run", "--url", srv.URL, "--token", "tok", "--agent", "claude-code", "--wait")
	var ee *exitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *exitError", err)
	}
	if ee.code != 2 {
		t.Errorf("exit code = %d, want 2 for lifecycle termination", ee.code)
	}
}

func TestRunCmd_WaitPersistentPollErrorAborts(t *testing.T) {
	setWaitPollInterval(t, time.Millisecond)
	runID := uuid.New()
	var mu sync.Mutex
	created := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !created && r.Method == http.MethodPost {
			created = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(types.AgentRun{ID: runID, State: types.RunPending})
			return
		}
		w.WriteHeader(http.StatusInternalServerError) // every poll fails
	}))
	t.Cleanup(srv.Close)

	err := execCmd(t, "run", "--url", srv.URL, "--token", "tok", "--agent", "claude-code", "--wait")
	if err == nil || !strings.Contains(err.Error(), "failed 5 times in a row") {
		t.Fatalf("err = %v, want a persistent-poll-failure abort", err)
	}
	var ee *exitError
	if errors.As(err, &ee) {
		t.Errorf("persistent poll failure should be a plain error (exit 1), got *exitError code %d", ee.code)
	}
}

func TestRunCmd_WaitTimeoutExits124(t *testing.T) {
	setWaitPollInterval(t, time.Millisecond)
	runID := uuid.New()
	srv := waitServer(t, runID, []types.RunState{types.RunRunning}, nil)

	err := execCmd(t, "run", "--url", srv.URL, "--token", "tok", "--agent", "claude-code",
		"--wait", "--timeout", "1ms")
	var ee *exitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *exitError", err)
	}
	if ee.code != 124 {
		t.Errorf("exit code = %d, want 124 on timeout", ee.code)
	}
}

// --------------------------------------------------------------------------
// runs list / get commands
// --------------------------------------------------------------------------

func TestRunsGetCmd(t *testing.T) {
	id := uuid.New()
	srv := newCmdServer(t, http.StatusOK, types.AgentRun{
		ID: id, Agent: "claude-code", State: types.RunCompleted, Image: "wardyn-byoi/x:latest",
	})

	if err := execCmd(t, "runs", "get", id.String(), "--url", srv.URL, "--token", "tok", "--json"); err != nil {
		t.Fatalf("runs get returned error: %v", err)
	}
	got := srv.last()
	if got.method != http.MethodGet || got.path != "/api/v1/runs/"+id.String() {
		t.Errorf("got %s %s, want GET /api/v1/runs/%s", got.method, got.path, id)
	}
}

func TestRunsListCmd(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, []types.AgentRun{
		{ID: uuid.New(), Agent: "claude-code", Repo: "o/r", State: types.RunRunning},
	})

	if err := execCmd(t, "runs", "list", "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("runs list returned error: %v", err)
	}
	got := srv.last()
	if got.method != http.MethodGet || got.path != "/api/v1/runs" {
		t.Errorf("got %s %s, want GET /api/v1/runs", got.method, got.path)
	}
}

// --------------------------------------------------------------------------
// approve / deny commands
// --------------------------------------------------------------------------

func TestApproveCmd_PostsApproveWithReason(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, types.ApprovalRequest{
		ID: uuid.New(), State: types.ApprovalApproved,
	})

	apID := uuid.New()
	err := execCmd(t, "approve", apID.String(), "--url", srv.URL, "--token", "tok", "--reason", "ok by me")
	if err != nil {
		t.Fatalf("approve returned error: %v", err)
	}
	got := srv.last()
	want := "/api/v1/approvals/" + apID.String() + "/approve"
	if got.method != http.MethodPost || got.path != want {
		t.Errorf("got %s %s, want POST %s", got.method, got.path, want)
	}
	var body map[string]any
	_ = json.Unmarshal(got.body, &body)
	if body["reason"] != "ok by me" {
		t.Errorf("reason body = %v, want %q", body["reason"], "ok by me")
	}
}

func TestDenyCmd_PostsDeny(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, types.ApprovalRequest{
		ID: uuid.New(), State: types.ApprovalDenied,
	})

	apID := uuid.New()
	if err := execCmd(t, "deny", apID.String(), "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("deny returned error: %v", err)
	}
	got := srv.last()
	want := "/api/v1/approvals/" + apID.String() + "/deny"
	if got.method != http.MethodPost || got.path != want {
		t.Errorf("got %s %s, want POST %s", got.method, got.path, want)
	}
}

// TestApprovalsListCmd_RunFlagReachesServer pins that `approvals list --run`
// actually uses the server's ?run_id= filter (W19-S1-4 / W20-hold-fsm-7)
// instead of silently discarding it.
func TestApprovalsListCmd_RunFlagReachesServer(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, []types.ApprovalRequest{})
	runID := uuid.New()
	if err := execCmd(t, "approvals", "list", "--run", runID.String(), "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("approvals list returned error: %v", err)
	}
	q, err := url.ParseQuery(srv.last().query)
	if err != nil {
		t.Fatalf("parse query %q: %v", srv.last().query, err)
	}
	if got := q.Get("run_id"); got != runID.String() {
		t.Errorf("run_id query = %q, want %s", got, runID)
	}
}

// TestApprovalsListCmd_PrintsHostAndHoldHint pins the HOST and HOLD columns:
// a live wait_for_review egress hold must show its requested host and a
// "time left" hint, not the pre-fix blank cells that left the CLI decide
// loop unable to tell a live 30s hold from an ordinary up-to-24h pendency.
func TestApprovalsListCmd_PrintsHostAndHoldHint(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, []types.ApprovalRequest{{
		ID: uuid.New(), RunID: uuid.New(), Kind: types.ApprovalEgressDomain,
		State: types.ApprovalPending, RequestedAt: time.Now(),
		RequestedScope: json.RawMessage(`{"host":"pkg.example.com","mode":"wait_for_review"}`),
	}})
	got := captureStdout(t, func() {
		if err := execCmd(t, "approvals", "list", "--url", srv.URL, "--token", "tok"); err != nil {
			t.Fatalf("approvals list returned error: %v", err)
		}
	})
	if !strings.Contains(got, "pkg.example.com") {
		t.Errorf("output = %q, want it to contain the requested host", got)
	}
	if !strings.Contains(got, "left") {
		t.Errorf("output = %q, want a live-hold time-left hint", got)
	}
}

// TestApprovalsGetCmd_RequiresRunFlag: `approvals get` has no server-side
// get-by-id endpoint to fall back on, so --run is mandatory, not optional.
func TestApprovalsGetCmd_RequiresRunFlag(t *testing.T) {
	if err := execCmd(t, "approvals", "get", uuid.New().String(), "--token", "tok"); err == nil {
		t.Error("expected error when --run is missing, got nil")
	}
}

// TestApprovalsGetCmd_FindsByIDWithinRun exercises the actual lookup: the
// server has no GET /approvals/{id}, so `get` must list --run's approvals
// and find the matching ID itself.
func TestApprovalsGetCmd_FindsByIDWithinRun(t *testing.T) {
	runID := uuid.New()
	wantID := uuid.New()
	srv := newCmdServer(t, http.StatusOK, []types.ApprovalRequest{
		{ID: uuid.New(), RunID: runID, State: types.ApprovalPending},
		{ID: wantID, RunID: runID, Kind: types.ApprovalEgressDomain, State: types.ApprovalPending,
			RequestedScope: json.RawMessage(`{"host":"api.example.com"}`)},
	})
	out := &strings.Builder{}
	root := rootCmd()
	root.SetArgs([]string{"approvals", "get", wantID.String(), "--run", runID.String(), "--url", srv.URL, "--token", "tok"})
	root.SetOut(out)
	root.SetErr(&strings.Builder{})
	if err := root.Execute(); err != nil {
		t.Fatalf("approvals get returned error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, wantID.String()) || !strings.Contains(got, "api.example.com") {
		t.Errorf("output = %q, want it to name %s and its host", got, wantID)
	}

	q, err := url.ParseQuery(srv.last().query)
	if err != nil {
		t.Fatalf("parse query %q: %v", srv.last().query, err)
	}
	if got := q.Get("run_id"); got != runID.String() {
		t.Errorf("run_id query = %q, want %s", got, runID)
	}
}

// TestApprovalsGetCmd_NotFound: the ID isn't in --run's approvals.
func TestApprovalsGetCmd_NotFound(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, []types.ApprovalRequest{
		{ID: uuid.New(), RunID: uuid.New(), State: types.ApprovalPending},
	})
	err := execCmd(t, "approvals", "get", uuid.New().String(), "--run", uuid.New().String(),
		"--url", srv.URL, "--token", "tok")
	if err == nil {
		t.Error("expected error for an approval id not found in --run, got nil")
	}
}

// approve/deny take exactly one positional arg.
func TestApproveCmd_RequiresExactlyOneArg(t *testing.T) {
	if err := execCmd(t, "approve", "--token", "tok"); err == nil {
		t.Error("expected error when approval id is missing, got nil")
	}
}

// --scope/--until must reach the wire as decision_scope/decision_expires_at —
// the same two-line assignment exists in both approvalDecisionCmd's RunE and
// the SDK's Approve/Deny, so this is the one check that would catch either
// dropping it.
func TestApproveCmd_WithScopeAndUntil(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, types.ApprovalRequest{
		ID: uuid.New(), State: types.ApprovalApproved,
	})

	apID := uuid.New()
	before := time.Now()
	err := execCmd(t, "approve", apID.String(), "--url", srv.URL, "--token", "tok",
		"--scope", "until", "--until", "2h")
	if err != nil {
		t.Fatalf("approve returned error: %v", err)
	}
	var body map[string]any
	_ = json.Unmarshal(srv.last().body, &body)
	if body["decision_scope"] != "until" {
		t.Errorf("decision_scope = %v, want %q", body["decision_scope"], "until")
	}
	got, _ := body["decision_expires_at"].(string)
	expiresAt, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("decision_expires_at %q did not parse as RFC3339: %v", got, err)
	}
	if d := expiresAt.Sub(before); d < 90*time.Minute || d > 150*time.Minute {
		t.Errorf("decision_expires_at = %s, want ~2h after %s", expiresAt, before)
	}
}

// A bare --scope (no --until) must not touch decision_expires_at — the server
// rejects an until-less until, but ScopeOnce/ScopeRun/ScopeAlways have none.
func TestApproveCmd_WithScopeOnly(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, types.ApprovalRequest{
		ID: uuid.New(), State: types.ApprovalApproved,
	})

	if err := execCmd(t, "approve", uuid.New().String(), "--url", srv.URL, "--token", "tok",
		"--scope", "once"); err != nil {
		t.Fatalf("approve returned error: %v", err)
	}
	var body map[string]any
	_ = json.Unmarshal(srv.last().body, &body)
	if body["decision_scope"] != "once" {
		t.Errorf("decision_scope = %v, want %q", body["decision_scope"], "once")
	}
	if _, present := body["decision_expires_at"]; present {
		t.Errorf("decision_expires_at present with no --until: %v", body["decision_expires_at"])
	}
}

func TestParseDecisionUntil(t *testing.T) {
	t.Run("duration", func(t *testing.T) {
		before := time.Now()
		got, err := parseDecisionUntil("2h")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d := got.Sub(before); d < 90*time.Minute || d > 150*time.Minute {
			t.Errorf("got %s, want ~2h after %s", got, before)
		}
	})
	t.Run("RFC3339", func(t *testing.T) {
		want := time.Now().Add(48 * time.Hour).Truncate(time.Second).UTC()
		got, err := parseDecisionUntil(want.Format(time.RFC3339))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.Equal(want) {
			t.Errorf("got %s, want %s", got, want)
		}
	})
	t.Run("garbage", func(t *testing.T) {
		if _, err := parseDecisionUntil("not-a-time"); err == nil {
			t.Error("expected error for unparseable --until, got nil")
		}
	})
}

// --------------------------------------------------------------------------
// audit command
// --------------------------------------------------------------------------

func TestAuditCmd_BuildsQuery(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, []types.AuditEvent{{Action: "run.create", Outcome: "success"}})

	runID := uuid.New()
	// Positional run id, like the sibling commands (run get, approve, attach).
	if err := execCmd(t, "audit", runID.String(), "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("audit returned error: %v", err)
	}
	got := srv.last()
	if got.method != http.MethodGet || got.path != "/api/v1/audit" {
		t.Errorf("got %s %s, want GET /api/v1/audit", got.method, got.path)
	}
	if got.query != "run_id="+runID.String() {
		t.Errorf("query = %q, want run_id=%s", got.query, runID)
	}
}

// The deprecated --run flag still resolves the same run id for existing scripts.
func TestAuditCmd_DeprecatedRunFlagStillWorks(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, []types.AuditEvent{{Action: "run.create", Outcome: "success"}})

	runID := uuid.New()
	if err := execCmd(t, "audit", "--run", runID.String(), "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("audit returned error: %v", err)
	}
	if got := srv.last().query; got != "run_id="+runID.String() {
		t.Errorf("query = %q, want run_id=%s", got, runID)
	}
}

// audit needs a run id; without one (neither positional nor --run) it fails
// before any request.
func TestAuditCmd_RequiresRun(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, []types.AuditEvent{})

	err := execCmd(t, "audit", "--url", srv.URL, "--token", "tok")
	if err == nil {
		t.Fatal("expected error when run id missing, got nil")
	}
	if !strings.Contains(err.Error(), "run id is required") {
		t.Errorf("error = %q, want run id is required", err)
	}
	srv.mu.Lock()
	n := len(srv.reqs)
	srv.mu.Unlock()
	if n != 0 {
		t.Errorf("server saw %d requests, want 0", n)
	}
}

// TestAuditCmd_LimitOffsetFlagsPage pins W16-S1-2's core fix: before this,
// `wardyn audit` had no way to page past the per-run 1000-event cap, so a run
// with more events than that silently dropped its newest ones (including
// run.complete) with no flag to ask for the rest.
func TestAuditCmd_LimitOffsetFlagsPage(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, []types.AuditEvent{{Action: "run.create", Outcome: "success"}})
	runID := uuid.New()
	if err := execCmd(t, "audit", runID.String(), "--limit", "5", "--offset", "10", "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("audit returned error: %v", err)
	}
	got := srv.last().query
	if !strings.Contains(got, "limit=5") || !strings.Contains(got, "offset=10") {
		t.Errorf("query = %q, want limit=5 and offset=10", got)
	}
}

// TestAuditCmd_FilterFlagsReachServer pins the "documented filter flags" half
// of W16-S1-2's fix: docs/sdk.md already claimed the CLI mirrors the server's
// since/until/action_prefix/actor_type/outcome predicates, but auditCmd had no
// such flags at all — the doc overclaimed. This locks the flags to the wire.
func TestAuditCmd_FilterFlagsReachServer(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, []types.AuditEvent{})
	runID := uuid.New()
	err := execCmd(t, "audit", runID.String(),
		"--since", "2026-01-01T00:00:00Z", "--until", "2026-02-01T00:00:00Z",
		"--action-prefix", "egress.", "--actor-type", "agent", "--outcome", "denied",
		"--url", srv.URL, "--token", "tok")
	if err != nil {
		t.Fatalf("audit returned error: %v", err)
	}
	q, perr := url.ParseQuery(srv.last().query)
	if perr != nil {
		t.Fatalf("parse query %q: %v", srv.last().query, perr)
	}
	for k, want := range map[string]string{
		"since": "2026-01-01T00:00:00Z", "until": "2026-02-01T00:00:00Z",
		"action_prefix": "egress.", "actor_type": "agent", "outcome": "denied",
	} {
		if got := q.Get(k); got != want {
			t.Errorf("query %s = %q, want %q (full query: %s)", k, got, want, srv.last().query)
		}
	}
}

// TestAuditCmd_TruncatedPageWarnsOnStderr pins that a truncated page (server
// sets X-Wardyn-Truncated) is surfaced, not silently indistinguishable from a
// complete trail — the exact harm W16-S1-2 named. The warning goes to
// cmd.ErrOrStderr(), never mixed into the events themselves: emitJSON encodes
// straight from the server-decoded slice, so there is no string path by which
// this text could land inside the --json array.
func TestAuditCmd_TruncatedPageWarnsOnStderr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Wardyn-Truncated", "true")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]types.AuditEvent{{Action: "run.create", Outcome: "success"}})
	}))
	t.Cleanup(srv.Close)

	root := rootCmd()
	errBuf := &strings.Builder{}
	root.SetArgs([]string{"audit", uuid.New().String(), "--limit", "1", "--json", "--url", srv.URL, "--token", "tok"})
	root.SetOut(&strings.Builder{})
	root.SetErr(errBuf)
	if err := root.Execute(); err != nil {
		t.Fatalf("audit returned error: %v", err)
	}
	if !strings.Contains(errBuf.String(), "truncated") || !strings.Contains(errBuf.String(), "--offset=1") {
		t.Errorf("stderr = %q, want a truncation warning naming --offset=1", errBuf.String())
	}
}

// --------------------------------------------------------------------------
// logTail (W22-S1-4: `wardyn logs`)
// --------------------------------------------------------------------------

// TestLogTail_Filter_DedupesSameSecondBoundary is the real bug this type
// exists to prevent: the server's Since filter round-trips through RFC3339
// (1-second resolution), so re-polling with since=<last event's second> can
// legitimately return that same event again. filter must drop it, not
// re-print it, while still admitting a genuinely new event landing in that
// same second.
func TestLogTail_Filter_DedupesSameSecondBoundary(t *testing.T) {
	t0 := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	e1 := types.AuditEvent{ID: uuid.New(), Time: t0, Action: "run.dispatch"}
	e2 := types.AuditEvent{ID: uuid.New(), Time: t0, Action: "egress.allow"} // same second, different event

	var tail logTail
	first, tail := tail.filter([]types.AuditEvent{e1})
	if len(first) != 1 || first[0].ID != e1.ID {
		t.Fatalf("first poll = %v, want just e1", first)
	}

	// Re-poll returns the server's whole since-inclusive page: e1 again
	// (same second) plus the genuinely new e2.
	second, tail := tail.filter([]types.AuditEvent{e1, e2})
	if len(second) != 1 || second[0].ID != e2.ID {
		t.Fatalf("second poll = %v, want just the new event e2 (e1 must be deduped)", second)
	}

	// A third poll with nothing new yields nothing.
	third, _ := tail.filter([]types.AuditEvent{e1, e2})
	if len(third) != 0 {
		t.Errorf("third poll = %v, want no events (both already seen)", third)
	}
}

// TestLogTail_Filter_AdvancesPastSecondBoundary: a later-second event resets
// the dedup set to just that event, so an even-later re-poll of the SAME
// later event is also correctly deduped (not just the original second).
func TestLogTail_Filter_AdvancesPastSecondBoundary(t *testing.T) {
	t0 := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Second)
	e1 := types.AuditEvent{ID: uuid.New(), Time: t0}
	e2 := types.AuditEvent{ID: uuid.New(), Time: t1}

	var tail logTail
	_, tail = tail.filter([]types.AuditEvent{e1, e2})
	if !tail.since.Equal(t1) {
		t.Fatalf("tail.since = %v, want %v (the later event's time)", tail.since, t1)
	}
	again, _ := tail.filter([]types.AuditEvent{e1, e2})
	if len(again) != 0 {
		t.Errorf("re-poll of the same page = %v, want nothing new", again)
	}
}

// TestLogsCmd_FollowStopsAtTerminalState drives the real cobra command: two
// audit-event polls plus a run whose state flips to COMPLETED must print
// both events, exactly once each, and return without hanging.
func TestLogsCmd_FollowStopsAtTerminalState(t *testing.T) {
	runID := uuid.New()
	t0 := time.Now().UTC().Truncate(time.Second)
	var pollN int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/audit"):
			mu.Lock()
			n := pollN
			pollN++
			mu.Unlock()
			if n == 0 {
				_ = json.NewEncoder(w).Encode([]types.AuditEvent{{ID: uuid.New(), Time: t0, Action: "run.dispatch", Outcome: "success"}})
				return
			}
			_ = json.NewEncoder(w).Encode([]types.AuditEvent{})
		case strings.Contains(r.URL.Path, "/api/v1/runs/"):
			_ = json.NewEncoder(w).Encode(types.AgentRun{ID: runID, State: types.RunCompleted})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	out, err := runCmdWithTimeout(t, func(root *cobra.Command) {
		root.SetArgs([]string{"logs", runID.String(), "--interval", "1ms", "--url", srv.URL, "--token", "tok"})
	})
	if err != nil {
		t.Fatalf("logs returned error: %v", err)
	}
	if !strings.Contains(out, "run.dispatch") {
		t.Errorf("logs output = %q, want it to contain the dispatched event", out)
	}
	if got := strings.Count(out, "run.dispatch"); got != 1 {
		t.Errorf("run.dispatch printed %d times, want exactly 1 (no duplicate re-poll)", got)
	}
}

// TestLogsCmd_UnknownRunErrorsInsteadOfHanging: the audit endpoint answers
// 200 [] for a run id it has never seen, so an unknown/typo'd id — or an
// unauthorized caller, or a persistently 5xx-ing server — only ever surfaces
// through GetRun. Swallowing that error left the follow loop polling an empty
// trail forever with nothing printed and no exit.
func TestLogsCmd_UnknownRunErrorsInsteadOfHanging(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/api/v1/audit") {
			_ = json.NewEncoder(w).Encode([]types.AuditEvent{})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "run not found"})
	}))
	t.Cleanup(srv.Close)

	if _, err := runCmdWithTimeout(t, func(root *cobra.Command) {
		root.SetArgs([]string{"logs", uuid.New().String(), "--interval", "1ms", "--url", srv.URL, "--token", "tok"})
	}); err == nil {
		t.Fatal("logs returned nil for an unknown run id, want the GetRun 404 propagated")
	}
}

// runCmdWithTimeout builds rootCmd(), lets configure set its args/flags, and
// executes it with output captured (logsCmd writes via cmd.OutOrStdout()).
// It doubles as the timeout backstop for a follow loop that fails to exit.
func runCmdWithTimeout(t *testing.T, configure func(root *cobra.Command)) (string, error) {
	t.Helper()
	root := rootCmd()
	configure(root)
	out := &strings.Builder{}
	root.SetOut(out)
	root.SetErr(&strings.Builder{})
	done := make(chan error, 1)
	go func() { done <- root.Execute() }()
	select {
	case err := <-done:
		return out.String(), err
	case <-time.After(5 * time.Second):
		t.Fatal("command did not return within 5s — follow loop likely never exited")
		return "", nil
	}
}

// --------------------------------------------------------------------------
// run kill subcommand (kill is now a child of the consolidated `run` noun)
// --------------------------------------------------------------------------

func TestKillCmd(t *testing.T) {
	// KillRun replies with a JSON body (id + final state), which the SDK decodes;
	// an empty 202 body would leave the decode a no-op, so return a real one.
	runID := uuid.New()
	srv := newCmdServer(t, http.StatusAccepted, map[string]any{"id": runID, "state": types.RunKilled})

	if err := execCmd(t, "run", "kill", runID.String(), "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("run kill returned error: %v", err)
	}
	got := srv.last()
	want := "/api/v1/runs/" + runID.String() + "/kill"
	if got.method != http.MethodPost || got.path != want {
		t.Errorf("got %s %s, want POST %s", got.method, got.path, want)
	}
}

// A non-UUID run id is rejected client-side before any request (the SDK's typed
// path takes a uuid.UUID; the CLI parses the positional arg up front).
func TestKillCmd_RejectsNonUUID(t *testing.T) {
	srv := newCmdServer(t, http.StatusAccepted, nil)

	err := execCmd(t, "run", "kill", "run-77", "--url", srv.URL, "--token", "tok")
	if err == nil || !strings.Contains(err.Error(), "invalid run id") {
		t.Fatalf("err = %v, want a client-side invalid-run-id error", err)
	}
	srv.mu.Lock()
	n := len(srv.reqs)
	srv.mu.Unlock()
	if n != 0 {
		t.Errorf("server saw %d requests, want 0 (parse must short-circuit)", n)
	}
}

// --------------------------------------------------------------------------
// run recording command
// --------------------------------------------------------------------------

func TestRunRecordingCmd_DefaultsToBareRunID(t *testing.T) {
	runID := uuid.New()
	srv := newCmdServer(t, http.StatusOK, "cast-bytes")
	outPath := t.TempDir() + "/out.cast"

	err := execCmd(t, "run", "recording", runID.String(), "-o", outPath, "--url", srv.URL, "--token", "tok")
	if err != nil {
		t.Fatalf("run recording returned error: %v", err)
	}
	got := srv.last()
	want := "/api/v1/runs/" + runID.String() + "/recording/" + runID.String()
	if got.method != http.MethodGet || got.path != want {
		t.Errorf("got %s %s, want GET %s", got.method, got.path, want)
	}
}

// W21-S1-6: --session fetches an interactive run's OTHER recordings — an
// attach session's cast is stored server-side under the composite key
// "<run-id>~<session>" (recording.CastKey), which the server has always
// served, but nothing on the CLI/SDK side could ever request one before this.
func TestRunRecordingCmd_SessionFlagUsesCompositeKey(t *testing.T) {
	runID := uuid.New()
	srv := newCmdServer(t, http.StatusOK, "cast-bytes")
	outPath := t.TempDir() + "/out.cast"

	err := execCmd(t, "run", "recording", runID.String(), "--session", "attach-1", "-o", outPath, "--url", srv.URL, "--token", "tok")
	if err != nil {
		t.Fatalf("run recording --session returned error: %v", err)
	}
	got := srv.last()
	want := "/api/v1/runs/" + runID.String() + "/recording/" + runID.String() + "~attach-1"
	if got.method != http.MethodGet || got.path != want {
		t.Errorf("got %s %s, want GET %s", got.method, got.path, want)
	}
}

// --------------------------------------------------------------------------
// secret commands (set from stdin, ls, rm)
// --------------------------------------------------------------------------

// The value comes from stdin and ONLY stdin — there is no --value flag, because
// argv is world-readable in `ps` and this is the write path for every platform
// secret.
func TestSecretSetCmd_ReadsStdin(t *testing.T) {
	srv := newCmdServer(t, http.StatusNoContent, nil)

	err := execCmdStdin(t, "s3cr3t", "secret", "set", "gh-token", "--url", srv.URL, "--token", "tok")
	if err != nil {
		t.Fatalf("secret set returned error: %v", err)
	}
	got := srv.last()
	if got.method != http.MethodPut || got.path != "/api/v1/secrets/gh-token" {
		t.Errorf("got %s %s, want PUT /api/v1/secrets/gh-token", got.method, got.path)
	}
	var body map[string]any
	_ = json.Unmarshal(got.body, &body)
	if body["value"] != "s3cr3t" {
		t.Errorf("value body = %v, want s3cr3t", body["value"])
	}
}

// An empty stdin is rejected client-side; the empty-value guard is covered at
// the helper level in secret_test.go.

func TestSecretLsCmd(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, map[string][]string{"names": {"alpha", "beta"}})

	if err := execCmd(t, "secret", "ls", "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("secret ls returned error: %v", err)
	}
	got := srv.last()
	if got.method != http.MethodGet || got.path != "/api/v1/secrets" {
		t.Errorf("got %s %s, want GET /api/v1/secrets", got.method, got.path)
	}
}

func TestSecretRmCmd(t *testing.T) {
	srv := newCmdServer(t, http.StatusNoContent, nil)

	if err := execCmd(t, "secret", "rm", "gh-token", "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("secret rm returned error: %v", err)
	}
	got := srv.last()
	if got.method != http.MethodDelete || got.path != "/api/v1/secrets/gh-token" {
		t.Errorf("got %s %s, want DELETE /api/v1/secrets/gh-token", got.method, got.path)
	}
}

// --------------------------------------------------------------------------
// policy commands (list/get/delete; create/update via -f file)
// --------------------------------------------------------------------------

func TestPolicyListCmd(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, []types.RunPolicy{
		{ID: uuid.New(), Name: "default", Spec: types.RunPolicySpec{MinConfinementClass: types.CC2}},
	})

	if err := execCmd(t, "policy", "list", "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("policy list returned error: %v", err)
	}
	got := srv.last()
	if got.method != http.MethodGet || got.path != "/api/v1/policies" {
		t.Errorf("got %s %s, want GET /api/v1/policies", got.method, got.path)
	}
}

func TestPolicyGetCmd(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, types.RunPolicy{ID: uuid.New(), Name: "p"})

	polID := uuid.New()
	if err := execCmd(t, "policy", "get", polID.String(), "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("policy get returned error: %v", err)
	}
	got := srv.last()
	want := "/api/v1/policies/" + polID.String()
	if got.method != http.MethodGet || got.path != want {
		t.Errorf("got %s %s, want GET %s", got.method, got.path, want)
	}
}

func TestPolicyDeleteCmd(t *testing.T) {
	srv := newCmdServer(t, http.StatusNoContent, nil)

	polID := uuid.New()
	if err := execCmd(t, "policy", "delete", polID.String(), "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("policy delete returned error: %v", err)
	}
	got := srv.last()
	want := "/api/v1/policies/" + polID.String()
	if got.method != http.MethodDelete || got.path != want {
		t.Errorf("got %s %s, want DELETE %s", got.method, got.path, want)
	}
}

func TestPolicyCreateCmd_FromFile(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, types.RunPolicy{ID: uuid.New(), Name: "from-file"})

	// A full-body JSON file ({"name":..., "spec":{...}}).
	dir := t.TempDir()
	file := dir + "/policy.json"
	writeFile(t, file, `{"name":"from-file","spec":{"min_confinement_class":"CC2","first_use_approval":true}}`)

	if err := execCmd(t, "policy", "create", "-f", file, "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("policy create returned error: %v", err)
	}
	got := srv.last()
	if got.method != http.MethodPost || got.path != "/api/v1/policies" {
		t.Errorf("got %s %s, want POST /api/v1/policies", got.method, got.path)
	}
	var body map[string]any
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatalf("create body not JSON: %v", err)
	}
	if body["name"] != "from-file" {
		t.Errorf("name = %v, want from-file", body["name"])
	}
	spec, _ := body["spec"].(map[string]any)
	if spec["min_confinement_class"] != "CC2" {
		t.Errorf("spec min_confinement_class = %v, want CC2", spec["min_confinement_class"])
	}
}

func TestPolicyUpdateCmd_NameOverride(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, types.RunPolicy{ID: uuid.New(), Name: "renamed"})

	dir := t.TempDir()
	file := dir + "/policy.json"
	// A bare spec (no top-level "name"); --name must supply it.
	writeFile(t, file, `{"min_confinement_class":"CC1"}`)

	polID := uuid.New()
	if err := execCmd(t, "policy", "update", polID.String(), "-f", file, "--name", "renamed", "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("policy update returned error: %v", err)
	}
	got := srv.last()
	want := "/api/v1/policies/" + polID.String()
	if got.method != http.MethodPut || got.path != want {
		t.Errorf("got %s %s, want PUT %s", got.method, got.path, want)
	}
	var body map[string]any
	_ = json.Unmarshal(got.body, &body)
	if body["name"] != "renamed" {
		t.Errorf("name = %v, want renamed (the --name override)", body["name"])
	}
}

// create requires -f; cobra MarkFlagRequired must reject its absence.
func TestPolicyCreateCmd_RequiresFile(t *testing.T) {
	if err := execCmd(t, "policy", "create", "--token", "tok"); err == nil {
		t.Error("expected error when -f is missing, got nil")
	}
}

// A bare-spec file with no name and no --name override must error.
func TestPolicyCreateCmd_NameRequired(t *testing.T) {
	dir := t.TempDir()
	file := dir + "/policy.json"
	writeFile(t, file, `{"min_confinement_class":"CC1"}`)

	err := execCmd(t, "policy", "create", "-f", file, "--token", "tok")
	if err == nil {
		t.Fatal("expected error when no name is provided, got nil")
	}
	if !strings.Contains(err.Error(), "policy name is required") {
		t.Errorf("error = %q, want policy name is required", err)
	}
}

// --------------------------------------------------------------------------
// flag / env precedence + defaults
// --------------------------------------------------------------------------

// When WARDYN_URL is set and no --url flag is given, the command targets the env
// URL. When --url is also given, the flag wins.
func TestURL_FlagOverridesEnv(t *testing.T) {
	envSrv := newCmdServer(t, http.StatusOK, []types.AgentRun{})
	flagSrv := newCmdServer(t, http.StatusOK, []types.AgentRun{})

	t.Setenv("WARDYN_URL", envSrv.URL)
	t.Setenv("WARDYN_ADMIN_TOKEN", "env-tok")

	// No --url: env URL is used.
	if err := execCmd(t, "runs", "list"); err != nil {
		t.Fatalf("runs list (env url) error: %v", err)
	}
	if got := envSrv.last(); got.path != "/api/v1/runs" || got.auth != "Bearer env-tok" {
		t.Errorf("env-url request wrong: path=%s auth=%s", got.path, got.auth)
	}

	// --url given: flag URL wins over env, and so does --token.
	if err := execCmd(t, "runs", "list", "--url", flagSrv.URL, "--token", "flag-tok"); err != nil {
		t.Fatalf("runs list (flag url) error: %v", err)
	}
	if got := flagSrv.last(); got.path != "/api/v1/runs" || got.auth != "Bearer flag-tok" {
		t.Errorf("flag-url request wrong: path=%s auth=%s", got.path, got.auth)
	}
}

// With no WARDYN_ADMIN_TOKEN and no --token, do() proceeds WITHOUT an
// Authorization header rather than erroring client-side — a loopback wardynd in
// LOCAL HOST MODE accepts unauthenticated requests; an auth-gated server returns
// a clear 401 instead.
func TestToken_MissingProceedsUnauthenticated(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, []types.AgentRun{})
	t.Setenv("WARDYN_ADMIN_TOKEN", "")

	if err := execCmd(t, "runs", "list", "--url", srv.URL); err != nil {
		t.Fatalf("expected no-token request to proceed (local mode), got error: %v", err)
	}
	if got := srv.last(); got.auth != "" {
		t.Errorf("expected NO Authorization header without a token, got %q", got.auth)
	}
	srv.mu.Lock()
	n := len(srv.reqs)
	srv.mu.Unlock()
	if n != 1 {
		t.Errorf("server saw %d requests, want 1", n)
	}
}

// The --url default is the localhost fallback when WARDYN_URL is unset.
func TestURL_DefaultWhenUnset(t *testing.T) {
	t.Setenv("WARDYN_URL", "")
	root := rootCmd()
	f := root.PersistentFlags().Lookup("url")
	if f == nil {
		t.Fatal("url flag not registered")
	}
	if f.DefValue != "http://localhost:8080" {
		t.Errorf("url default = %q, want http://localhost:8080", f.DefValue)
	}
}

// --------------------------------------------------------------------------
// error surfacing: a non-2xx API response makes Execute() return non-nil
// (which main maps to exit code 1).
// --------------------------------------------------------------------------

func TestCmd_APIErrorSurfacesNonNil(t *testing.T) {
	srv := newCmdServer(t, http.StatusInternalServerError, map[string]string{"error": "boom"})

	err := execCmd(t, "runs", "list", "--url", srv.URL, "--token", "tok")
	if err == nil {
		t.Fatal("expected Execute() to return an error on a 500, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %q, want it to carry the 500 status", err)
	}
}

func TestCmd_UnknownCommandErrors(t *testing.T) {
	if err := execCmd(t, "definitely-not-a-command"); err == nil {
		t.Error("expected error for unknown subcommand, got nil")
	}
}

// --------------------------------------------------------------------------
// --json flag: accepted on the list/create/read commands; the request still
// fires unchanged (the JSON shaping is downstream of the wire call).
// --------------------------------------------------------------------------

func TestRunListCmd_JSON(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, []types.AgentRun{{ID: uuid.New(), Agent: "claude-code"}})

	if err := execCmd(t, "run", "list", "--json", "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("run list --json returned error: %v", err)
	}
	got := srv.last()
	if got.method != http.MethodGet || got.path != "/api/v1/runs" {
		t.Errorf("got %s %s, want GET /api/v1/runs", got.method, got.path)
	}
}

func TestPolicyListCmd_JSON(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, []types.RunPolicy{{ID: uuid.New(), Name: "p"}})

	if err := execCmd(t, "policy", "list", "--json", "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("policy list --json returned error: %v", err)
	}
	got := srv.last()
	if got.method != http.MethodGet || got.path != "/api/v1/policies" {
		t.Errorf("got %s %s, want GET /api/v1/policies", got.method, got.path)
	}
}

func TestPolicyCreateCmd_JSON(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, types.RunPolicy{ID: uuid.New(), Name: "from-file"})

	dir := t.TempDir()
	file := dir + "/policy.json"
	writeFile(t, file, `{"name":"from-file","spec":{"min_confinement_class":"CC2"}}`)

	if err := execCmd(t, "policy", "create", "-f", file, "--json", "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("policy create --json returned error: %v", err)
	}
	got := srv.last()
	if got.method != http.MethodPost || got.path != "/api/v1/policies" {
		t.Errorf("got %s %s, want POST /api/v1/policies", got.method, got.path)
	}
}

func TestSecretListCmd_JSON(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, map[string][]string{"names": {"alpha"}})

	if err := execCmd(t, "secret", "list", "--json", "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("secret list --json returned error: %v", err)
	}
	got := srv.last()
	if got.method != http.MethodGet || got.path != "/api/v1/secrets" {
		t.Errorf("got %s %s, want GET /api/v1/secrets", got.method, got.path)
	}
}

func TestRecordSynthesizeCmd_JSON(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, sdk.ProfileResult{OverallRisk: "low"})

	runID := uuid.New()
	if err := execCmd(t, "record", "synthesize", runID.String(), "--json", "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("record synthesize --json returned error: %v", err)
	}
	got := srv.last()
	want := "/api/v1/runs/" + runID.String() + "/profile"
	if got.method != http.MethodPost || got.path != want {
		t.Errorf("got %s %s, want POST %s", got.method, got.path, want)
	}
}

// --------------------------------------------------------------------------
// approvals list (surfaces the client's listApprovals; approve/deny decide one)
// --------------------------------------------------------------------------

func TestApprovalsListCmd(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, []types.ApprovalRequest{
		{ID: uuid.New(), RunID: uuid.New(), State: types.ApprovalPending},
	})

	if err := execCmd(t, "approvals", "list", "--state", "PENDING", "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("approvals list returned error: %v", err)
	}
	got := srv.last()
	if got.method != http.MethodGet || got.path != "/api/v1/approvals" {
		t.Errorf("got %s %s, want GET /api/v1/approvals", got.method, got.path)
	}
	if got.query != "state=PENDING" {
		t.Errorf("query = %q, want state=PENDING", got.query)
	}
}

func TestApprovalsListCmd_JSON(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, []types.ApprovalRequest{
		{ID: uuid.New(), RunID: uuid.New(), State: types.ApprovalPending},
	})

	if err := execCmd(t, "approvals", "list", "--json", "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("approvals list --json returned error: %v", err)
	}
	got := srv.last()
	if got.method != http.MethodGet || got.path != "/api/v1/approvals" {
		t.Errorf("got %s %s, want GET /api/v1/approvals", got.method, got.path)
	}
}

// --------------------------------------------------------------------------
// secret list / delete (the renamed ls/rm; ls/rm live on as aliases above)
// --------------------------------------------------------------------------

func TestSecretListCmd(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, map[string][]string{"names": {"alpha", "beta"}})

	if err := execCmd(t, "secret", "list", "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("secret list returned error: %v", err)
	}
	got := srv.last()
	if got.method != http.MethodGet || got.path != "/api/v1/secrets" {
		t.Errorf("got %s %s, want GET /api/v1/secrets", got.method, got.path)
	}
}

func TestSecretDeleteCmd(t *testing.T) {
	srv := newCmdServer(t, http.StatusNoContent, nil)

	if err := execCmd(t, "secret", "delete", "gh-token", "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("secret delete returned error: %v", err)
	}
	got := srv.last()
	if got.method != http.MethodDelete || got.path != "/api/v1/secrets/gh-token" {
		t.Errorf("got %s %s, want DELETE /api/v1/secrets/gh-token", got.method, got.path)
	}
}

// --------------------------------------------------------------------------
// record save --name is now a cobra required flag
// --------------------------------------------------------------------------

func TestRecordSaveCmd_RequiresName(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, sdk.ProfileResult{})

	err := execCmd(t, "record", "save", uuid.New().String(), "--url", srv.URL, "--token", "tok")
	if err == nil {
		t.Fatal("expected error when --name is missing, got nil")
	}
	if !strings.Contains(err.Error(), `required flag(s) "name" not set`) {
		t.Errorf("error = %q, want the required-flag message", err)
	}
	srv.mu.Lock()
	n := len(srv.reqs)
	srv.mu.Unlock()
	if n != 0 {
		t.Errorf("server saw %d requests, want 0 (required-flag check must short-circuit)", n)
	}
}

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
// The table writers target os.Stdout directly (newTab), not cobra's out sink,
// so the cobra SetOut in execCmd cannot see them.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

// An ACTIONABLE id must print in full. `run kill` / `approve` / `deny` / `--policy`
// all parse a full UUID and reject a truncated one ("invalid UUID length: 8"), so
// truncating the ID column here would break the obvious list → copy → act flow.
// Found by running the CLI e2e: `run list` printed 790047a8 and `run kill 790047a8`
// then failed. Context-only columns (an approval's RUN, an audit target) may stay short.
func TestListCmds_PrintFullActionableIDs(t *testing.T) {
	runID := uuid.New()
	apprID, apprRun := uuid.New(), uuid.New()
	polID := uuid.New()

	for _, tc := range []struct {
		name string
		body any
		args []string
		want uuid.UUID
		// notWant, when set, must NOT appear in full (a context-only column).
		notWant *uuid.UUID
	}{
		{"run list", []types.AgentRun{{ID: runID, Agent: "claude-code"}},
			[]string{"run", "list"}, runID, nil},
		{"approvals list", []types.ApprovalRequest{{ID: apprID, RunID: apprRun, State: types.ApprovalPending}},
			[]string{"approvals", "list"}, apprID, &apprRun},
		{"policy list", []types.RunPolicy{{ID: polID, Name: "p"}},
			[]string{"policy", "list"}, polID, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newCmdServer(t, http.StatusOK, tc.body)
			var execErr error
			out := captureStdout(t, func() {
				execErr = execCmd(t, append(tc.args, "--url", srv.URL, "--token", "tok")...)
			})
			if execErr != nil {
				t.Fatalf("%s: %v", tc.name, execErr)
			}
			if !strings.Contains(out, tc.want.String()) {
				t.Errorf("%s must print the FULL actionable id %s (a truncated id is rejected by the action subcommands); got:\n%s",
					tc.name, tc.want, out)
			}
			if tc.notWant != nil && strings.Contains(out, tc.notWant.String()) {
				t.Errorf("%s: context-only column should stay truncated, but printed %s in full:\n%s",
					tc.name, tc.notWant, out)
			}
		})
	}
}

// --------------------------------------------------------------------------
// workspace commands
// --------------------------------------------------------------------------

// `workspace create` is what clears the run-create onboarding gate, so its body
// must carry the fields the server gates on. The name defaults to the source.
func TestWorkspaceCreateCmd(t *testing.T) {
	srv := newCmdServer(t, http.StatusCreated, types.Workspace{
		ID: uuid.New(), Kind: types.WorkspaceKindLocalDir, Source: "/home/you/svc",
	})

	err := execCmd(t, "workspace", "create", "--url", srv.URL, "--token", "tok",
		"--kind", "local_dir", "--source", "/home/you/svc", "--writable")
	if err != nil {
		t.Fatalf("workspace create returned error: %v", err)
	}
	got := srv.last()
	if got.method != http.MethodPost || got.path != "/api/v1/workspaces" {
		t.Errorf("got %s %s, want POST /api/v1/workspaces", got.method, got.path)
	}
	var body map[string]any
	_ = json.Unmarshal(got.body, &body)
	if body["kind"] != "local_dir" || body["source"] != "/home/you/svc" || body["writable"] != true {
		t.Errorf("workspace body kind/source/writable wrong: %v", body)
	}
	if body["name"] != "/home/you/svc" {
		t.Errorf("name = %v, want it defaulted to --source", body["name"])
	}
}

// TestApprovalsGetCmd_PagesPastTheFirstPage: `approvals get` has no
// server-side get-by-id, so it scans --run's approvals — and an unparameterised
// list is ONE server-default page (200). An approval past that read as "not
// found on run", which a long-running run with a busy egress lane reaches
// easily. The scan pages until it finds the row or the page comes back short.
func TestApprovalsGetCmd_PagesPastTheFirstPage(t *testing.T) {
	runID := uuid.New()
	wantID := uuid.New()
	// 201 approvals: a full first page, then the one we are looking for.
	all := make([]types.ApprovalRequest, 0, 201)
	for range 200 {
		all = append(all, types.ApprovalRequest{ID: uuid.New(), RunID: runID, State: types.ApprovalPending})
	}
	all = append(all, types.ApprovalRequest{
		ID: wantID, RunID: runID, Kind: types.ApprovalEgressDomain, State: types.ApprovalPending,
		RequestedScope: json.RawMessage(`{"host":"api.example.com"}`),
	})

	var offsets []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		offsets = append(offsets, q.Get("offset"))
		off, _ := strconv.Atoi(q.Get("offset"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit == 0 {
			limit = 200
		}
		page := []types.ApprovalRequest{}
		if off < len(all) {
			page = all[off:min(off+limit, len(all))]
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(page)
	}))
	t.Cleanup(srv.Close)

	out := &strings.Builder{}
	root := rootCmd()
	root.SetArgs([]string{"approvals", "get", wantID.String(), "--run", runID.String(), "--url", srv.URL, "--token", "tok"})
	root.SetOut(out)
	root.SetErr(&strings.Builder{})
	if err := root.Execute(); err != nil {
		t.Fatalf("approvals get returned error: %v (offsets requested: %v)", err, offsets)
	}
	if !strings.Contains(out.String(), wantID.String()) {
		t.Errorf("output = %q, want it to name %s", out.String(), wantID)
	}
	if len(offsets) < 2 {
		t.Errorf("requested offsets = %v, want a second page to have been fetched", offsets)
	}
}

// TestLogsCmd_NonFollowUnknownRunErrors: `logs --follow=false` used to skip
// GetRun entirely, so a typo'd run id printed nothing and exited 0 — the audit
// endpoint answers 200 [] for an id that does not exist. Both modes now check
// the run first, so an unknown or unauthorized id is an error in both.
func TestLogsCmd_NonFollowUnknownRunErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/api/v1/audit") {
			_ = json.NewEncoder(w).Encode([]types.AuditEvent{})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "run not found"})
	}))
	t.Cleanup(srv.Close)

	if _, err := runCmdWithTimeout(t, func(root *cobra.Command) {
		root.SetArgs([]string{"logs", uuid.New().String(), "--follow=false", "--url", srv.URL, "--token", "tok"})
	}); err == nil {
		t.Fatal("logs --follow=false returned nil for an unknown run id, want the GetRun 404 propagated")
	}
}

// TestLogsCmd_NonFollowFollowsTruncatedPages: the per-run audit page caps at
// 1000 events server-side. --follow recovers from that for free on its next
// poll (`since` has advanced); one-shot mode returned after the first page and
// printed a silently cut-off log.
func TestLogsCmd_NonFollowFollowsTruncatedPages(t *testing.T) {
	runID := uuid.New()
	t0 := time.Now().UTC().Truncate(time.Second)
	var pollN int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/audit"):
			n := pollN
			pollN++
			if n == 0 {
				w.Header().Set("X-Wardyn-Truncated", "true")
				_ = json.NewEncoder(w).Encode([]types.AuditEvent{
					{ID: uuid.New(), Time: t0, Action: "run.dispatch", Outcome: "success"},
				})
				return
			}
			_ = json.NewEncoder(w).Encode([]types.AuditEvent{
				{ID: uuid.New(), Time: t0.Add(time.Second), Action: "run.complete", Outcome: "success"},
			})
		default:
			_ = json.NewEncoder(w).Encode(types.AgentRun{ID: runID, State: types.RunCompleted})
		}
	}))
	t.Cleanup(srv.Close)

	out, err := runCmdWithTimeout(t, func(root *cobra.Command) {
		root.SetArgs([]string{"logs", runID.String(), "--follow=false", "--url", srv.URL, "--token", "tok"})
	})
	if err != nil {
		t.Fatalf("logs returned error: %v", err)
	}
	if !strings.Contains(out, "run.dispatch") || !strings.Contains(out, "run.complete") {
		t.Errorf("logs output = %q, want both the truncated page and the one after it", out)
	}
}

// TestLogsCmd_FollowDrainsAuditsAfterTerminal: the completion watcher flips the
// run terminal BEFORE run.complete is written, and the revoke/teardown audits
// land after that again. Returning on the first terminal read dropped exactly
// the completion line the command's help promises.
func TestLogsCmd_FollowDrainsAuditsAfterTerminal(t *testing.T) {
	runID := uuid.New()
	t0 := time.Now().UTC().Truncate(time.Second)
	var mu sync.Mutex
	var pollN int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/audit"):
			mu.Lock()
			n := pollN
			pollN++
			mu.Unlock()
			if n == 1 { // lands only AFTER the state has already flipped
				_ = json.NewEncoder(w).Encode([]types.AuditEvent{
					{ID: uuid.New(), Time: t0.Add(time.Second), Action: "run.complete", Outcome: "success"},
				})
				return
			}
			_ = json.NewEncoder(w).Encode([]types.AuditEvent{})
		default:
			_ = json.NewEncoder(w).Encode(types.AgentRun{ID: runID, State: types.RunCompleted})
		}
	}))
	t.Cleanup(srv.Close)

	out, err := runCmdWithTimeout(t, func(root *cobra.Command) {
		root.SetArgs([]string{"logs", runID.String(), "--interval", "1ms", "--url", srv.URL, "--token", "tok"})
	})
	if err != nil {
		t.Fatalf("logs returned error: %v", err)
	}
	if !strings.Contains(out, "run.complete") {
		t.Errorf("logs output = %q, want the completion line written after the state flip", out)
	}
}
