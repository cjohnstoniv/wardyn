// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
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

// runFilesResp is one scripted response for GET /api/v1/runs/{id}/files;
// status 0 defaults to 200.
type runFilesResp struct {
	status int
	body   string
}

// runReadyServer scripts GET /api/v1/runs/{id} (a run-state sequence, pinned
// on the last entry once exhausted, mirroring waitServer in
// commands_test.go) and GET /api/v1/runs/{id}/files (a response sequence,
// same pinning) so waitForRunReady's two independent polling loops — one
// over run state, one over the workspace read — can each be driven on its
// own schedule.
func runReadyServer(t *testing.T, runID uuid.UUID, repo string, states []types.RunState, files []runFilesResp, audit []types.AuditEvent) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	runPolls, filePolls := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runs/"+runID.String():
			mu.Lock()
			i := runPolls
			if i >= len(states) {
				i = len(states) - 1
			}
			runPolls++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(types.AgentRun{ID: runID, State: states[i], Repo: repo})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runs/"+runID.String()+"/files":
			mu.Lock()
			i := filePolls
			if i >= len(files) {
				i = len(files) - 1
			}
			filePolls++
			mu.Unlock()
			resp := files[i]
			status := resp.status
			if status == 0 {
				status = http.StatusOK
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, resp.body)
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

// execWaitReadyText runs `run wait-ready <args>` (non-JSON) and returns
// cobra's own out sink — the text-mode success line goes through
// cmd.OutOrStdout(), unlike --json (see execWaitReadyJSON below).
func execWaitReadyText(t *testing.T, srv *httptest.Server, args ...string) (string, error) {
	t.Helper()
	cmd := runWaitReadyCmd(func() *sdk.Client { return &sdk.Client{BaseURL: srv.URL} })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// execWaitReadyJSON runs `run wait-ready <args> --json` and decodes stdout.
// --json goes through emitJSON, which targets os.Stdout directly rather than
// cobra's out sink (captureStdout is commands_test.go's fixture for exactly
// that).
func execWaitReadyJSON(t *testing.T, srv *httptest.Server, args ...string) (waitReadyResult, error) {
	t.Helper()
	cmd := runWaitReadyCmd(func() *sdk.Client { return &sdk.Client{BaseURL: srv.URL} })
	cmd.SetArgs(append(args, "--json"))
	var err error
	out := captureStdout(t, func() { err = cmd.Execute() })
	if err != nil {
		return waitReadyResult{}, err
	}
	var res waitReadyResult
	if uerr := json.Unmarshal([]byte(out), &res); uerr != nil {
		t.Fatalf("--json output not valid JSON: %v (%q)", uerr, out)
	}
	return res, nil
}

func TestRunWaitReady_NoRepo_ReadyAtNone(t *testing.T) {
	setWaitPollInterval(t, time.Millisecond)
	runID := uuid.New()
	srv := runReadyServer(t, runID, "",
		[]types.RunState{types.RunPending, types.RunRunning},
		[]runFilesResp{{status: http.StatusConflict}, {body: `{"vcs":"none","path":"/home/agent/work"}`}},
		nil)

	out, err := execWaitReadyText(t, srv, runID.String())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "/home/agent/work") || !strings.Contains(out, "(none)") {
		t.Errorf("output = %q, want it to name the workspace path and vcs=none", out)
	}
}

// TestRunWaitReady_RepoSet_WaitsForGit proves the loop does NOT settle for
// the intermediate vcs:"none" response once the run names a repo: if it did,
// the output would say "(none)" instead of "(git)".
func TestRunWaitReady_RepoSet_WaitsForGit(t *testing.T) {
	setWaitPollInterval(t, time.Millisecond)
	runID := uuid.New()
	srv := runReadyServer(t, runID, "octocat/hello-world",
		[]types.RunState{types.RunRunning},
		[]runFilesResp{
			{status: http.StatusConflict},
			{body: `{"vcs":"none","path":"/home/agent/work"}`},
			{body: `{"vcs":"git","path":"/home/agent/work"}`},
		}, nil)

	out, err := execWaitReadyText(t, srv, runID.String())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "(git)") {
		t.Errorf("output = %q, want it to have waited for vcs=git rather than settling for the earlier vcs=none response", out)
	}
}

func TestRunWaitReady_FailedBeforeReady_ExitCode1(t *testing.T) {
	setWaitPollInterval(t, time.Millisecond)
	runID := uuid.New()
	srv := runReadyServer(t, runID, "",
		[]types.RunState{types.RunFailed},
		[]runFilesResp{{status: http.StatusConflict}},
		[]types.AuditEvent{
			{Action: "run.dispatch", Outcome: "failure", Data: json.RawMessage(`{"error":"pull ghcr.io/x/agent:latest: not found"}`)},
		})

	_, err := execWaitReadyText(t, srv, runID.String())
	var ee *exitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *exitError", err)
	}
	if ee.code != 1 {
		t.Errorf("exit code = %d, want 1 for FAILED", ee.code)
	}
	if !strings.Contains(err.Error(), "run.dispatch: pull ghcr.io/x/agent:latest: not found") {
		t.Errorf("error = %q, want it to carry the dispatch failure reason (same lookup run --wait uses)", err.Error())
	}
}

func TestRunWaitReady_Killed_ExitCode2(t *testing.T) {
	setWaitPollInterval(t, time.Millisecond)
	runID := uuid.New()
	srv := runReadyServer(t, runID, "",
		[]types.RunState{types.RunKilled},
		[]runFilesResp{{status: http.StatusConflict}}, nil)

	_, err := execWaitReadyText(t, srv, runID.String())
	var ee *exitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *exitError", err)
	}
	if ee.code != 2 {
		t.Errorf("exit code = %d, want 2 for a non-FAILED terminal state", ee.code)
	}
}

func TestRunWaitReady_Timeout_ExitCode124(t *testing.T) {
	setWaitPollInterval(t, time.Millisecond)
	runID := uuid.New()
	srv := runReadyServer(t, runID, "",
		[]types.RunState{types.RunPending},
		[]runFilesResp{{status: http.StatusConflict}}, nil)

	_, err := execWaitReadyText(t, srv, runID.String(), "--timeout", "20ms")
	var ee *exitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *exitError", err)
	}
	if ee.code != 124 {
		t.Errorf("exit code = %d, want 124 on timeout", ee.code)
	}
}

func TestRunWaitReady_JSON(t *testing.T) {
	setWaitPollInterval(t, time.Millisecond)
	runID := uuid.New()
	srv := runReadyServer(t, runID, "",
		[]types.RunState{types.RunRunning},
		[]runFilesResp{{status: http.StatusConflict}, {body: `{"vcs":"none","path":"/home/agent/work"}`}}, nil)

	res, err := execWaitReadyJSON(t, srv, runID.String())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ID != runID {
		t.Errorf("id = %s, want %s", res.ID, runID)
	}
	if res.State != types.RunRunning {
		t.Errorf("state = %s, want RUNNING", res.State)
	}
	if res.Workspace.VCS != "none" || res.Workspace.Path != "/home/agent/work" {
		t.Errorf("workspace = %+v, want {vcs:none path:/home/agent/work}", res.Workspace)
	}
}

// exitCodeOf unwraps the CLI's exitError; -1 when err is not one.
func exitCodeOf(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return -1
}

func fastPoll(t *testing.T) {
	t.Helper()
	prev := waitPollInterval
	waitPollInterval = time.Millisecond
	t.Cleanup(func() { waitPollInterval = prev })
}

// A run that dies AFTER it was seen RUNNING — the exact window --expect-git
// waits in — must exit 2 at once, not burn the timeout to 124. Before the
// state was re-read on every tick this spun on /files until the deadline.
func TestWaitReady_RunDiesAfterRunningExitsTwo(t *testing.T) {
	fastPoll(t)
	id := uuid.New()
	srv := runReadyServer(t, id, "octocat/hello", []types.RunState{types.RunRunning, types.RunKilled},
		[]runFilesResp{{status: http.StatusConflict, body: `{"error":"no sandbox yet"}`}}, nil)
	_, err := execWaitReadyText(t, srv, id.String(), "--timeout", "5s")
	if code := exitCodeOf(err); code != 2 {
		t.Fatalf("exit code = %d (err=%v), want 2 for a run killed after RUNNING", code, err)
	}
}

// 501 (the runner cannot exec into a sandbox) is permanent: waiting cannot
// change it, so the command fails immediately and names the error.
func TestWaitReady_PermanentFilesErrorFailsFast(t *testing.T) {
	fastPoll(t)
	id := uuid.New()
	srv := runReadyServer(t, id, "", []types.RunState{types.RunRunning},
		[]runFilesResp{{status: http.StatusNotImplemented, body: `{"error":"runner has no exec stream"}`}}, nil)
	start := time.Now()
	_, err := execWaitReadyText(t, srv, id.String(), "--timeout", "5s")
	if err == nil || !strings.Contains(err.Error(), "cannot read its workspace") {
		t.Fatalf("err = %v, want an immediate 'cannot read its workspace' error", err)
	}
	if exitCodeOf(err) == 124 || time.Since(start) > 2*time.Second {
		t.Fatalf("a permanent error must not wait for the deadline (err=%v, took %s)", err, time.Since(start))
	}
}

// --expect-git makes a repo-less run wait for a git work tree; without the
// flag the same responses would have returned at vcs:"none".
func TestWaitReady_ExpectGitFlagWaitsForGit(t *testing.T) {
	fastPoll(t)
	id := uuid.New()
	files := []runFilesResp{
		{body: `{"vcs":"none","files":[],"path":"/home/agent/work","truncated":false}`},
		{body: `{"vcs":"git","files":[],"path":"/home/agent/work/hello","truncated":false}`},
	}
	srv := runReadyServer(t, id, "", []types.RunState{types.RunRunning}, files, nil)
	out, err := execWaitReadyText(t, srv, id.String(), "--expect-git", "--timeout", "5s")
	if err != nil || !strings.Contains(out, "/home/agent/work/hello (git)") {
		t.Fatalf("out=%q err=%v, want readiness at the git work tree", out, err)
	}
}

// vcs:"unknown" is what wait-ready must decide about, and the answer differs
// by arm.
//
// Without --expect-git (and no repo) the caller asked only for a USABLE
// sandbox: the exec ran, so the sandbox is up, and Path names the directory it
// settled on — that is exactly the "workspace inspectable" this command
// promises. Waiting cannot improve it; the loop used to poll the state to the
// full 5-minute deadline and exit 124 on a sandbox an editor could already
// open.
//
// With --expect-git it must still wait: "unknown" is git confirming a work
// tree and then a later git command failing (internal/api/run_files.go's
// runFilesScript exits 3 — vcs:"none" — when git is missing outright), which
// is precisely the shape a clone still landing has.
func TestWaitReady_UnknownVCS_ReadyWithoutGit_WaitsWithIt(t *testing.T) {
	const unknown = `{"vcs":"unknown","files":[],"path":"/home/agent/work","truncated":false}`

	t.Run("no-git-wanted-is-ready", func(t *testing.T) {
		fastPoll(t)
		id := uuid.New()
		srv := runReadyServer(t, id, "", []types.RunState{types.RunRunning},
			[]runFilesResp{{body: unknown}}, nil)
		out, err := execWaitReadyText(t, srv, id.String(), "--timeout", "30ms")
		if err != nil {
			t.Fatalf("err = %v, want readiness: the sandbox is up and its workspace path is known", err)
		}
		if !strings.Contains(out, "/home/agent/work") || !strings.Contains(out, "(unknown)") {
			t.Errorf("output = %q, want it to name the path and report vcs unknown honestly", out)
		}
	})

	t.Run("expect-git-still-waits", func(t *testing.T) {
		fastPoll(t)
		id := uuid.New()
		srv := runReadyServer(t, id, "", []types.RunState{types.RunRunning},
			[]runFilesResp{{body: unknown}}, nil)
		_, err := execWaitReadyText(t, srv, id.String(), "--expect-git", "--timeout", "30ms")
		if code := exitCodeOf(err); code != 124 || !strings.Contains(err.Error(), `vcs "unknown"`) {
			t.Fatalf("exit=%d err=%v, want 124 naming vcs \"unknown\"", code, err)
		}
	})
}
