// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// execWaitStall is how long the fake session's Wait pretends the driver's own
// deadline takes to give up on an undrained stream. The real docker driver's
// pollExecExit selects on the handler's 5 s ctx; a test does not need five
// seconds to prove the stall happened.
const execWaitStall = 300 * time.Millisecond

// stallingExecSession models the runner's STREAMING CONTRACT honestly: Stdout
// is an unbuffered io.Pipe written by one goroutine, and Wait cannot return
// until that goroutine is done — exactly like the single demux goroutine the
// real driver runs. A handler that stops reading Stdout short of EOF and then
// calls Wait therefore deadlocks until the deadline, which is the bug.
//
// Close releases the writer, which is what makes "skip Wait and let the
// deferred Close tear the exec down" a correct thing for a handler to do.
func stallingExecSession(stdout string) *runner.ExecSession {
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		_, _ = io.WriteString(stdoutW, stdout)
		_ = stdoutW.Close()
		_ = stderrW.Close()
	}()
	return &runner.ExecSession{
		Stdout: stdoutR,
		Stderr: stderrR,
		Wait: func() (int, error) {
			select {
			case <-writerDone:
				return 0, nil
			case <-time.After(execWaitStall):
				return 0, context.DeadlineExceeded
			}
		},
		Close: func() error {
			_ = stdoutR.CloseWithError(io.ErrClosedPipe)
			_ = stderrR.CloseWithError(io.ErrClosedPipe)
			return nil
		},
	}
}

// overCapNumstat builds git numstat output that exceeds the BYTE cap while
// staying well under the ROW cap, so the only thing that can set truncated is
// the byte cap under test.
func overCapNumstat() string {
	var b strings.Builder
	fmt.Fprintf(&b, "path=/home/agent/work\n%s\n", runFilesSeparator)
	const rows = 400 // < runFilesMaxFiles (500), so the row cap cannot fire
	long := strings.Repeat("d/", 700)
	for i := 0; i < rows; i++ {
		fmt.Fprintf(&b, "1\t1\t%sfile%04d.go\n", long, i)
	}
	if b.Len() <= runFilesMaxOutput {
		panic("fixture no longer exceeds runFilesMaxOutput")
	}
	return b.String()
}

// TestRunFiles_ByteCapTruncatesWithoutStallingOnWait is B1-F4.
//
// The 512 KiB io.LimitReader left sess.Stdout undrained, so the in-sandbox git
// blocked on write, the demux goroutine blocked with it, and Wait could only
// time out: a 5 s stall, a 500, and a run.files FAILURE row on every poll tick
// — for a workspace that simply has a lot of changed files. And the byte-cap
// truncation itself was invisible: truncated stayed false, so a short list was
// presented as the whole truth.
func TestRunFiles_ByteCapTruncatesWithoutStallingOnWait(t *testing.T) {
	srv, _, audit, run := newRunFilesHarness(func(runner.ExecSpec) (*runner.ExecSession, error) {
		return stallingExecSession(overCapNumstat()), nil
	})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doRunFiles(srv, run.ID, nil) }()
	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleRunFiles never returned: it is parked on Wait behind an undrained stdout")
	}

	resp := decodeRunFiles(t, w)
	if !resp.Truncated {
		t.Error("truncated = false although the byte cap cut the stream short — a short list presented as complete")
	}
	if len(resp.Files) == 0 || len(resp.Files) >= runFilesMaxFiles {
		t.Fatalf("files = %d; the fixture must truncate on BYTES, below the %d row cap", len(resp.Files), runFilesMaxFiles)
	}
	if resp.VCS != runFilesVCSGit {
		t.Errorf("vcs = %q, want %q — git ran and produced output", resp.VCS, runFilesVCSGit)
	}
	for _, ev := range audit.events {
		if ev.Action == "run.files" {
			t.Fatalf("run.files audit row %+v — hitting the cap is an ordinary outcome, not a failure of the read", ev)
		}
	}
}

// nilSessionRunner is the runner contract's worst legal answer: no error and no
// session. run_files.go has guarded it since it was found there; its sibling
// did not, and a nil-deref panic on the HTTP chain (which has no recover
// middleware) takes the daemon down.
type nilSessionRunner struct{ runner.Runner }

func (nilSessionRunner) ExecStream(context.Context, string, runner.ExecSpec) (*runner.ExecSession, error) {
	return nil, nil
}

// TestRunExec_NilSessionIsA500OnBothWidgets is B1-F8.
func TestRunExec_NilSessionIsA500OnBothWidgets(t *testing.T) {
	run := types.AgentRun{
		ID:         uuid.New(),
		CreatedBy:  "sub-owner@corp.example",
		State:      types.RunRunning,
		SandboxRef: "sandbox-abc",
	}
	srv := New(Config{Store: runFilesStore{run: run}, Runner: nilSessionRunner{}, Audit: &recRecorder{}})

	for name, call := range map[string]func(http.ResponseWriter, *http.Request){
		"files":     srv.handleRunFiles,
		"resources": srv.handleRunResources,
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/"+name, nil)
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("id", run.ID.String())
			w := httptest.NewRecorder()
			call(w, r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx)))
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("code = %d, want 500 (body %s)", w.Code, w.Body.String())
			}
		})
	}
}

// TestRunResources_ByteCapSkipsWait is the sibling half of B1-F4: the resources
// script's own cap has the identical undrained-pipe shape.
func TestRunResources_ByteCapSkipsWait(t *testing.T) {
	var b strings.Builder
	b.WriteString("cpu_usage_usec=1\n")
	for b.Len() <= runResourcesMaxOutput {
		b.WriteString("noise_" + strings.Repeat("x", 200) + "=1\n")
	}
	run := types.AgentRun{
		ID: uuid.New(), CreatedBy: "sub-owner@corp.example",
		State: types.RunRunning, SandboxRef: "sandbox-abc",
	}
	fr := &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) {
		return stallingExecSession(b.String()), nil
	}}
	srv := New(Config{Store: runFilesStore{run: run}, Runner: fr, Audit: &recRecorder{}})

	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/resources", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", run.ID.String())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		srv.handleRunResources(w, r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx)))
		done <- w
	}()
	select {
	case w := <-done:
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (body %s)", w.Code, w.Body.String())
		}
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handleRunResources never returned: it is parked on Wait behind an undrained stdout")
	}
}
