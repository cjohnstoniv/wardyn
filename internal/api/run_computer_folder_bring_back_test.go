// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// agentExec answers the two execs a bring-back runs before its pass: the read of PID 1's command
// line, and the stop. It is false for any other exec.
func agentExec(spec runner.ExecSpec, pid1 string, stop func() (string, int)) (*runner.ExecSession, bool) {
	var out string
	code := 0
	switch {
	case len(spec.Argv) == 2 && spec.Argv[0] == "cat" && spec.Argv[1] == "/proc/1/cmdline":
		out = pid1
	case len(spec.Argv) == 3 && spec.Argv[0] == "sh" && spec.Argv[2] == agentStopScript:
		out, code = stop()
	default:
		return nil, false
	}
	return &runner.ExecSession{
		Stdin: nopWriteCloser{io.Discard}, Stdout: strings.NewReader(out), Stderr: strings.NewReader(""),
		Wait: func() (int, error) { return code, nil }, Close: func() error { return nil },
	}, true
}

// taskIdlePID1 is /proc/1/cmdline in a Docker task run's sandbox.
var taskIdlePID1 = "sh\x00-c\x00" + runner.AgentIdleScript + "\x00"

// agentSandbox is folderSandboxRunner with an agent in it. The agent writes late.txt when the
// pass starts unless it was stopped first, as a writer asleep at the request would. stopFails
// leaves a process the stop cannot end; stopErr fails the sandbox's teardown.
type agentSandbox struct {
	*folderSandboxRunner
	t         *testing.T
	mu        sync.Mutex
	pid1      string
	stopFails bool
	stopErr   error
	stops     int
	agentDown bool
	wrote     bool
	// onPID1 runs at the read of PID 1, the bring-back's last check before its compare-and-set.
	onPID1 func()
}

func (r *agentSandbox) ExecStream(ctx context.Context, ref string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.onPID1 != nil && len(spec.Argv) == 2 && spec.Argv[1] == "/proc/1/cmdline" {
		r.onPID1()
	}
	if sess, ok := agentExec(spec, r.pid1, func() (string, int) {
		r.stops++
		if r.stopFails {
			return "left: 42\n", 3
		}
		r.agentDown = true
		return agentStoppedMark + "\n", 0
	}); ok {
		return sess, nil
	}
	if !r.agentDown && !r.wrote {
		r.wrote = true
		f, err := r.client(r.t).Create("/home/agent/work/late.txt")
		if err != nil {
			r.t.Error(err)
		} else {
			_, _ = f.Write([]byte("written after the request"))
			_ = f.Close()
		}
	}
	return r.folderSandboxRunner.ExecStream(ctx, ref, spec)
}

func (r *agentSandbox) StopSandbox(ctx context.Context, ref string) error {
	if r.stopErr != nil {
		return r.stopErr
	}
	return r.folderSandboxRunner.StopSandbox(ctx, ref)
}

// refStore is computerFolderStore keeping the sandbox ref the tail clears, so a test can tell a
// confirmed teardown.
type refStore struct{ *computerFolderStore }

func (s refStore) SetSandboxRef(_ context.Context, _ uuid.UUID, ref string) error {
	s.dispatchTestStore.mu.Lock()
	defer s.dispatchTestStore.mu.Unlock()
	s.run.SandboxRef = ref
	return nil
}

const bringBackOwner = "sub-bring-back-owner"

// bringBackFixture is a live "both ways" run whose folder seeded, in a Docker task sandbox, with
// the sandbox's README.md edited by the agent. It answers SSO sessions.
func bringBackFixture(t *testing.T) (*Server, *computerFolderStore, *recRecorder, types.AgentRun, *agentSandbox, string) {
	t.Helper()
	s, cs, audit, run, sb, app := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	cs.dispatchTestStore.mu.Lock()
	cs.run.CreatedBy, cs.run.SandboxRef = bringBackOwner, "sbx"
	run = cs.run
	cs.dispatchTestStore.mu.Unlock()
	cs.runner.Owner = bringBackOwner
	run.State = types.RunRunning
	agent := &agentSandbox{folderSandboxRunner: sb, t: t, pid1: taskIdlePID1}
	// SSO sessions are wired when the server is built, so build it again with them.
	cfg := s.cfg
	cfg.Runner, cfg.Store, cfg.OIDC = agent, refStore{cs}, &oidc.Authenticator{}
	subs := s.runnerStreams.subs
	s = New(cfg)
	s.runnerStreams.subs = subs
	f, err := sb.client(t).OpenFile("/home/agent/work/README.md", os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("edited by the agent"))
	_ = f.Close()
	_ = sb.client(t).Chtimes("/home/agent/work/README.md", time.Now(), time.Now().Add(time.Hour))
	return s, cs, audit, run, agent, app
}

func bringBack(t *testing.T, s *Server, cookie *http.Cookie, run types.AgentRun) *bringBackResult {
	t.Helper()
	w := doSSO(t, s, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/computer-folder/bring-back", cookie, "")
	res := &bringBackResult{code: w.Code, body: w.Body.String()}
	_ = json.Unmarshal(w.Body.Bytes(), &res.json)
	return res
}

type bringBackResult struct {
	code int
	body string
	json map[string]any
}

func (h *bringBackResult) reason() string { r, _ := h.json["reason"].(string); return r }

func bringBackOwnerSession(t *testing.T) *http.Cookie {
	return ssoSession(t, bringBackOwner, "owner@corp.example", oidc.RoleUser)
}

func TestBringComputerFolderBack_EndsTheRunAfterItsChangesComeBack(t *testing.T) {
	s, cs, audit, run, sb, app := bringBackFixture(t)
	var backBeforeStop bool
	sb.stopped = func() {
		b, _ := os.ReadFile(filepath.Join(app, "README.md"))
		backBeforeStop = string(b) == "edited by the agent"
	}
	res := bringBack(t, s, bringBackOwnerSession(t), run)
	if res.code != http.StatusOK || res.json["state"] != string(types.RunStopped) {
		t.Fatalf("bring-back = %d %s", res.code, res.body)
	}
	if !backBeforeStop {
		t.Fatal("the sandbox was stopped before its changes came back")
	}
	if sb.stops != 1 || res.json["sandbox_removed"] != true {
		t.Errorf("%d agent stops, sandbox_removed %v: want the agent stopped once and the sandbox gone", sb.stops, res.json["sandbox_removed"])
	}
	if cs.State() != types.RunStopped {
		t.Errorf("state = %s, want STOPPED", cs.State())
	}
	if back, _ := res.json["back"].(map[string]any); back == nil || back["files"] != float64(1) {
		t.Errorf("the answer's pass = %v, want 1 file", res.json["back"])
	}
	ev := findAuditData(audit.snapshot(), run.ID, "run.folder.transfer", `"direction":"bring_back"`)
	if ev == nil || ev.Outcome != "success" || ev.ActorType != types.ActorHuman || ev.Actor != bringBackOwner || !strings.Contains(string(ev.Data), `"files":1`) {
		t.Fatalf("the pass is not audited as the person's bring_back with its count: %+v", ev)
	}
	if findAuditData(audit.snapshot(), run.ID, "run.folder.transfer", `"direction":"back"`) != nil {
		t.Error("the pass is also audited as an ordinary end's")
	}
	if ev := findAudit(audit.snapshot(), run.ID, "run.folder.finalize", "success"); ev == nil || ev.Actor != bringBackOwner {
		t.Errorf("the run's end is not audited as the person's bring-back: %+v", ev)
	}
}

func TestBringComputerFolderBack_OnlyTheRunsOwnPerson(t *testing.T) {
	for name, cookie := range map[string]func(*testing.T) *http.Cookie{
		"admin": func(t *testing.T) *http.Cookie {
			return ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
		},
		"another person": func(t *testing.T) *http.Cookie {
			return ssoSession(t, "sub-other", "other@corp.example", oidc.RoleUser)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, cs, audit, run, _, app := bringBackFixture(t)
			if res := bringBack(t, s, cookie(t), run); res.code != http.StatusNotFound {
				t.Fatalf("bring-back = %d %s, want 404", res.code, res.body)
			}
			if cs.State() != types.RunRunning || findAudit(audit.snapshot(), run.ID, "run.folder.finalize", "success") != nil {
				t.Errorf("state %s: the run was ended for someone else", cs.State())
			}
			if b, _ := os.ReadFile(filepath.Join(app, "README.md")); string(b) != "hello" {
				t.Errorf("someone else wrote onto the person's computer: %q", b)
			}
		})
	}
}

func TestBringComputerFolderBack_Refusals(t *testing.T) {
	for name, tc := range map[string]struct {
		set    func(*Server, *computerFolderStore, *agentSandbox)
		status int
		reason string
	}{
		"switch off": {func(s *Server, _ *computerFolderStore, _ *agentSandbox) { s.cfg.ComputerFolders = false }, http.StatusForbidden, reasonComputerFoldersOff},
		"killed":     {func(_ *Server, cs *computerFolderStore, _ *agentSandbox) { cs.state = types.RunKilled }, http.StatusConflict, reasonComputerFolderNotRunning},
		"copy in only": {func(_ *Server, cs *computerFolderStore, _ *agentSandbox) {
			cs.folder.Direction = types.ComputerFolderIn
		}, http.StatusConflict, reasonComputerFolderCopyInOnly},
		"no computer folder": {func(_ *Server, cs *computerFolderStore, _ *agentSandbox) { cs.folder.RunnerID = nil }, http.StatusConflict, reasonComputerFolderNone},
		"never synced":       {func(_ *Server, cs *computerFolderStore, _ *agentSandbox) { cs.seed.SeededAt = nil }, http.StatusConflict, reasonComputerFolderNotSynced},
		"runner offline": {func(s *Server, _ *computerFolderStore, _ *agentSandbox) {
			s.cfg.RunnerOnline = func(uuid.UUID) bool { return false }
		}, http.StatusConflict, reasonComputerRunnerOffline},
		"PID 1 is the agent": {func(_ *Server, _ *computerFolderStore, sb *agentSandbox) {
			sb.pid1 = "claude\x00-p\x00fix the build\x00"
		}, http.StatusConflict, reasonComputerFolderAgentUnstoppable},
	} {
		t.Run(name, func(t *testing.T) {
			s, cs, audit, run, sb, app := bringBackFixture(t)
			stops := 0
			sb.stopped = func() { stops++ }
			tc.set(s, cs, sb)
			before := cs.State()
			res := bringBack(t, s, bringBackOwnerSession(t), run)
			if res.code != tc.status || res.reason() != tc.reason {
				t.Fatalf("bring-back = %d %s, want %d %s", res.code, res.body, tc.status, tc.reason)
			}
			if want := map[string]string{
				reasonComputerFolderNotRunning: bringBackNotRunningMsg,
				reasonComputerRunnerOffline:    "laptop is offline, so the folder can't come back now. Try again when it reconnects.",
			}[tc.reason]; want != "" && res.json["error"] != want {
				t.Errorf("refusal = %q, want %q", res.json["error"], want)
			}
			if cs.State() != before || stops != 0 || sb.stops != 0 || cs.status.Back != nil {
				t.Errorf("a refused bring-back changed the run: state %s, %d sandbox stops, %d agent stops, back %+v", cs.State(), stops, sb.stops, cs.status.Back)
			}
			if b, _ := os.ReadFile(filepath.Join(app, "README.md")); string(b) != "hello" {
				t.Errorf("a refused bring-back wrote onto the computer: %q", b)
			}
			if findAudit(audit.snapshot(), run.ID, "run.folder.finalize", "success") != nil {
				t.Error("a refused bring-back is audited as done")
			}
		})
	}
}

// A writer asleep when the person asks (sleep 300 && echo x > f) wakes during the pass unless the
// agent is stopped first: what it writes after the request never comes back.
func TestBringComputerFolderBack_AWriteAfterTheRequestDoesNotComeBack(t *testing.T) {
	s, _, _, run, sb, app := bringBackFixture(t)
	if res := bringBack(t, s, bringBackOwnerSession(t), run); res.code != http.StatusOK {
		t.Fatalf("bring-back = %d %s", res.code, res.body)
	}
	if _, err := os.Stat(filepath.Join(app, "late.txt")); err == nil || sb.wrote {
		t.Fatal("a write the agent made after the request came back")
	}
	if b, _ := os.ReadFile(filepath.Join(app, "README.md")); string(b) != "edited by the agent" {
		t.Errorf("README.md = %q: the changes made before the request did not come back", b)
	}
}

// A paused run is refused, not resumed: resuming it would wake its agent before it could be stopped.
func TestBringComputerFolderBack_APausedRunIsNotResumed(t *testing.T) {
	s, cs, _, run, sb, app := bringBackFixture(t)
	at := time.Now().Add(-time.Hour)
	cs.dispatchTestStore.mu.Lock()
	cs.run.PausedAt = &at
	cs.dispatchTestStore.mu.Unlock()
	res := bringBack(t, s, bringBackOwnerSession(t), run)
	if res.code != http.StatusConflict || res.reason() != reasonComputerFolderPaused || res.json["error"] != bringBackPausedMsg {
		t.Fatalf("bring-back = %d %s, want 409 %s", res.code, res.body, reasonComputerFolderPaused)
	}
	got, _ := cs.GetRun(context.Background(), run.ID)
	if got.State != types.RunRunning || got.PausedAt == nil || sb.stops != 0 || sb.wrote {
		t.Errorf("state %s, paused %v, %d agent stops, agent wrote %v: want the run left paused and untouched", got.State, got.PausedAt != nil, sb.stops, sb.wrote)
	}
	if _, err := os.Stat(filepath.Join(app, "late.txt")); err == nil {
		t.Error("a paused run's write came back")
	}
}

// When the agent cannot be confirmed stopped, nothing is copied. The run has already ended, since
// one compare-and-set from RUNNING decided it: the pass is skipped and audited as failed, and the
// shared teardown then stops the sandbox, discarding its changes.
func TestBringComputerFolderBack_AStopThatFailsCopiesNothing(t *testing.T) {
	s, cs, audit, run, sb, app := bringBackFixture(t)
	sb.stopFails = true
	var teardowns []int // agent stops seen at each sandbox stop
	sb.stopped = func() { teardowns = append(teardowns, sb.stops) }
	res := bringBack(t, s, bringBackOwnerSession(t), run)
	if res.code != http.StatusOK {
		t.Fatalf("bring-back = %d %s", res.code, res.body)
	}
	if back, _ := res.json["back"].(map[string]any); back == nil || back["error"] != bringBackAgentRunningMsg {
		t.Errorf("the answer's pass = %v, want the stop's error", res.json["back"])
	}
	if b, _ := os.ReadFile(filepath.Join(app, "README.md")); string(b) != "hello" {
		t.Errorf("README.md = %q: a pass ran with the agent still running", b)
	}
	if sb.stops != 1 || cs.State() != types.RunStopped {
		t.Errorf("%d agent stops, state %s: want one stop and the run STOPPED", sb.stops, cs.State())
	}
	if sb.wrote {
		t.Error("a pass ran after the stop failed")
	}
	if len(teardowns) != 1 || teardowns[0] != 1 {
		t.Errorf("sandbox stops %v (agent stops seen at each): want the sandbox stopped once, after the failed stop", teardowns)
	}
	ev := findAuditData(audit.snapshot(), run.ID, "run.folder.transfer", `"direction":"bring_back"`)
	if ev == nil || ev.Outcome != "failure" {
		t.Fatalf("the failed pass is not audited as one: %+v", ev)
	}
	var data map[string]any
	if _ = json.Unmarshal(ev.Data, &data); data["error"] != localFolderSeedError(bringBackAgentRunningMsg) {
		t.Errorf("the failed pass's audit error = %v", data["error"])
	}
	if cs.status.Back == nil || cs.status.Back.Error != bringBackAgentRunningMsg {
		t.Errorf("the folder's record = %+v, want the stop's error", cs.status.Back)
	}
}

// The folder can come back while the sandbox's teardown after it fails: the answer says which.
func TestBringComputerFolderBack_SaysWhetherTheSandboxWasRemoved(t *testing.T) {
	for _, stopErr := range []error{nil, errors.New("daemon unreachable")} {
		s, _, _, run, sb, _ := bringBackFixture(t)
		sb.stopErr = stopErr
		res := bringBack(t, s, bringBackOwnerSession(t), run)
		back, _ := res.json["back"].(map[string]any)
		if res.code != http.StatusOK || back == nil || back["error"] != nil || back["files"] != float64(1) {
			t.Fatalf("teardown error %v: bring-back = %d %s, want the folder back", stopErr, res.code, res.body)
		}
		if got := res.json["sandbox_removed"]; got != (stopErr == nil) {
			t.Errorf("teardown error %v: sandbox_removed = %v", stopErr, got)
		}
	}
}

// A kill (or any other end) and a bring-back both move the run from RUNNING: exactly one wins.
func TestBringComputerFolderBack_HasOneWinnerAgainstAKill(t *testing.T) {
	// The kill lands after every check the bring-back makes, just before its compare-and-set.
	s, cs, _, run, sb, app := bringBackFixture(t)
	sb.onPID1 = func() {
		if ok, err := s.casRunState(context.Background(), run.ID, types.RunRunning, types.RunKilled); !ok || err != nil {
			t.Errorf("the kill did not land: %v %v", ok, err)
		}
	}
	if res := bringBack(t, s, bringBackOwnerSession(t), run); res.code != http.StatusConflict || res.reason() != reasonComputerFolderNotRunning {
		t.Fatalf("bring-back after a kill = %d %s, want 409 %s", res.code, res.body, reasonComputerFolderNotRunning)
	}
	if b, _ := os.ReadFile(filepath.Join(app, "README.md")); cs.State() != types.RunKilled || sb.stops != 0 || string(b) != "hello" {
		t.Fatalf("state %s, %d agent stops, README.md %q: the losing bring-back still acted", cs.State(), sb.stops, b)
	}
	for i := range 10 {
		s, cs, _, run, _, _ := bringBackFixture(t)
		start := make(chan struct{})
		var killed bool
		var res *bringBackResult
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			killed, _ = s.casRunState(context.Background(), run.ID, types.RunRunning, types.RunKilled)
		})
		wg.Go(func() {
			<-start
			res = bringBack(t, s, bringBackOwnerSession(t), run)
		})
		close(start)
		wg.Wait()
		broughtBack := res.code == http.StatusOK
		if killed == broughtBack {
			t.Fatalf("round %d: kill won %v, bring-back %d %s: want exactly one winner", i, killed, res.code, res.body)
		}
		if want := map[bool]types.RunState{true: types.RunKilled, false: types.RunStopped}[killed]; cs.State() != want {
			t.Fatalf("round %d: state %s, want %s", i, cs.State(), want)
		}
		if killed && res.reason() != reasonComputerFolderNotRunning {
			t.Fatalf("round %d: the losing bring-back = %d %s", i, res.code, res.body)
		}
	}
}

// A later end of the same run (its runner's exit, say) finds the folder brought back and leaves it.
func TestBringComputerFolderBack_ALaterEndKeepsItsRecord(t *testing.T) {
	s, cs, audit, run, _, _ := bringBackFixture(t)
	if res := bringBack(t, s, bringBackOwnerSession(t), run); res.code != http.StatusOK {
		t.Fatalf("bring-back = %d %s", res.code, res.body)
	}
	want := *cs.status.Back // a second pass would find nothing new and record 0 files
	s.bringComputerFolderBack(context.Background(), run.ID, "sbx")
	if got := cs.status.Back; got == nil || got.Error != "" || got.Files != want.Files {
		t.Errorf("a later end replaced the pass's record: %+v", got)
	}
	n := 0
	for _, ev := range audit.snapshot() {
		if ev.Action == "run.folder.transfer" && strings.Contains(string(ev.Data), `"direction":"b`) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d passes back audited, want 1", n)
	}
}
