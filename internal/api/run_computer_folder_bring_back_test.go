// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sampledFolderSandbox is folderSandboxRunner with a CPU reading: nil is no reading.
type sampledFolderSandbox struct {
	*folderSandboxRunner
	mu  sync.Mutex
	cpu *float64
}

func (r *sampledFolderSandbox) BatchSample() bool { return true }

func (r *sampledFolderSandbox) SampleCPU(_ context.Context, refs []string) (map[string]float64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]float64{}
	if r.cpu != nil {
		for _, ref := range refs {
			out[ref] = *r.cpu
		}
	}
	return out, nil
}

const bringBackOwner = "sub-bring-back-owner"

// bringBackFixture is a live "both ways" run whose folder seeded, quiet for an hour, its agent
// idle, with the sandbox's README.md edited by the agent. It answers SSO sessions.
func bringBackFixture(t *testing.T) (*Server, *computerFolderStore, *recRecorder, types.AgentRun, *sampledFolderSandbox, string) {
	t.Helper()
	s, cs, audit, run, sb, app := computerFolderFixture(t, types.ComputerFolderBoth)
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal(cs.FailureHint())
	}
	cs.dispatchTestStore.mu.Lock()
	cs.run.CreatedBy, cs.run.SandboxRef, cs.run.CreatedAt = bringBackOwner, "sbx", time.Now().Add(-time.Hour)
	run = cs.run
	cs.dispatchTestStore.mu.Unlock()
	run.State = types.RunRunning
	sampled := &sampledFolderSandbox{folderSandboxRunner: sb, cpu: pct(1)}
	// SSO sessions are wired when the server is built, so build it again with them.
	cfg := s.cfg
	cfg.Runner, cfg.OIDC = sampled, &oidc.Authenticator{}
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
	return s, cs, audit, run, sampled, app
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
		set    func(*Server, *computerFolderStore, *sampledFolderSandbox)
		status int
		reason string
	}{
		"switch off": {func(s *Server, _ *computerFolderStore, _ *sampledFolderSandbox) { s.cfg.ComputerFolders = false }, http.StatusForbidden, reasonComputerFoldersOff},
		"killed":     {func(_ *Server, cs *computerFolderStore, _ *sampledFolderSandbox) { cs.state = types.RunKilled }, http.StatusConflict, reasonComputerFolderNotRunning},
		"copy in only": {func(_ *Server, cs *computerFolderStore, _ *sampledFolderSandbox) {
			cs.folder.Direction = types.ComputerFolderIn
		}, http.StatusConflict, reasonComputerFolderCopyInOnly},
		"no computer folder": {func(_ *Server, cs *computerFolderStore, _ *sampledFolderSandbox) { cs.folder.RunnerID = nil }, http.StatusConflict, reasonComputerFolderNone},
		"never synced":       {func(_ *Server, cs *computerFolderStore, _ *sampledFolderSandbox) { cs.seed.SeededAt = nil }, http.StatusConflict, reasonComputerFolderNotSynced},
		"runner offline": {func(s *Server, _ *computerFolderStore, _ *sampledFolderSandbox) {
			s.cfg.RunnerOnline = func(uuid.UUID) bool { return false }
		}, http.StatusConflict, reasonComputerRunnerOffline},
		"a person typed a minute ago": {func(_ *Server, cs *computerFolderStore, _ *sampledFolderSandbox) {
			at := time.Now().Add(-time.Minute)
			cs.run.ActiveAt = &at
		}, http.StatusConflict, reasonComputerFolderAgentActive},
		"the agent's CPU is busy": {func(_ *Server, _ *computerFolderStore, sb *sampledFolderSandbox) { sb.cpu = pct(80) }, http.StatusConflict, reasonComputerFolderAgentActive},
		"no CPU reading":          {func(_ *Server, _ *computerFolderStore, sb *sampledFolderSandbox) { sb.cpu = nil }, http.StatusConflict, reasonComputerFolderAgentActive},
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
			if cs.State() != before || stops != 0 || cs.status.Back != nil {
				t.Errorf("a refused bring-back changed the run: state %s, %d stops, back %+v", cs.State(), stops, cs.status.Back)
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

// A kill (or any other end) and a bring-back both move the run from RUNNING: exactly one wins.
func TestBringComputerFolderBack_HasOneWinnerAgainstAKill(t *testing.T) {
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
