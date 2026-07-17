// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

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
	root := rootCmd()
	root.SetArgs(args)
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

// approve/deny take exactly one positional arg.
func TestApproveCmd_RequiresExactlyOneArg(t *testing.T) {
	if err := execCmd(t, "approve", "--token", "tok"); err == nil {
		t.Error("expected error when approval id is missing, got nil")
	}
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
// secret commands (set via --value, ls, rm)
// --------------------------------------------------------------------------

func TestSecretSetCmd_WithValueFlag(t *testing.T) {
	srv := newCmdServer(t, http.StatusNoContent, nil)

	err := execCmd(t, "secret", "set", "gh-token", "--value", "s3cr3t", "--url", srv.URL, "--token", "tok")
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

// An empty value (no --value, nothing on stdin) is rejected without a request.
// We force the empty-stdin path by providing --value="" explicitly is not
// possible (it is the zero value), so we rely on the set command reading stdin;
// here we exercise the ls/rm requests which do not need stdin, and cover the
// empty-value guard at the helper level in secret_test.go.

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
