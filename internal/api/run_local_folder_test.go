// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// localFolderStore is dispatchTestStore plus the seed surface.
type localFolderStore struct {
	*dispatchTestStore
	mu     sync.Mutex
	folder *types.RunLocalFolder
	seed   store.LocalFolderSeed
	sets   int
}

func (s *localFolderStore) RunLocalFolderSeed(context.Context, uuid.UUID) (*types.RunLocalFolder, store.LocalFolderSeed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.folder, s.seed, nil
}

func (s *localFolderStore) SetRunLocalFolderSeed(_ context.Context, _ uuid.UUID, errText string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.folder == nil || s.seed.SeededAt != nil || s.seed.Error != "" {
		return false, nil
	}
	s.sets++
	if errText == "" {
		now := time.Now()
		s.seed.SeededAt = &now
	} else {
		s.seed.Error = errText
	}
	return true, nil
}

// execCountRunner counts agent Execs.
type execCountRunner struct {
	*killRaceRunner
	execs atomic.Int32
}

func (r *execCountRunner) Exec(context.Context, string, []string) (string, error) {
	r.execs.Add(1)
	return "exec-1", nil
}

func localFolderFixture(t *testing.T) (*Server, *localFolderStore, *recRecorder, types.AgentRun, *execCountRunner) {
	t.Helper()
	old := localFolderSeedPoll
	localFolderSeedPoll = 5 * time.Millisecond
	t.Cleanup(func() { localFolderSeedPoll = old })
	rn := &execCountRunner{killRaceRunner: &killRaceRunner{fakeRunner: &fakeRunner{}}}
	s, st, audit, run := dispatchTeardownFixture(t, rn, types.RunRunning)
	ls := &localFolderStore{dispatchTestStore: st, folder: &types.RunLocalFolder{Name: "my-app", RemoteDir: "/home/agent/work"}}
	s.cfg.Store = ls
	return s, ls, audit, run, rn
}

func TestHoldForLocalFolder_WaitsForTheFirstSync(t *testing.T) {
	s, ls, audit, run, _ := localFolderFixture(t)
	done := make(chan bool, 1)
	go func() { done <- s.holdForLocalFolder(context.Background(), run, "sbx") }()
	select {
	case <-done:
		t.Fatal("the hold let the agent start before the folder's first sync landed")
	case <-time.After(50 * time.Millisecond):
	}
	s.localFolderPassed(context.Background(), run.ID, "alice@example.com", "/home/agent/work", true, "")
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("the hold refused the start after the sync landed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the hold never released")
	}
	if findAudit(audit.snapshot(), run.ID, "run.seed.hold", "success") == nil || findAudit(audit.snapshot(), run.ID, "run.seed", "success") == nil {
		t.Errorf("the wait and its result are not both audited: %v", audit.snapshot())
	}
	if ls.State() != types.RunRunning {
		t.Errorf("state = %s", ls.State())
	}
}

func TestHoldForLocalFolder_AFailedSeedFailsTheRunNamingTheFolder(t *testing.T) {
	s, ls, _, run, rn := localFolderFixture(t)
	s.localFolderPassed(context.Background(), run.ID, "alice@example.com", "/home/agent/work", false, "list /home/agent/work: connection lost\x1b[2J")
	if s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal("the agent was let start after its seed failed")
	}
	hint := ls.FailureHint()
	if ls.State() != types.RunFailed || !strings.Contains(hint, `"my-app"`) || !strings.Contains(hint, "connection lost") || strings.Contains(hint, "\x1b") {
		t.Errorf("state %s, hint %q: want FAILED naming the folder, with the reported error and no control characters", ls.State(), hint)
	}
	if rn.stopCount() != 1 {
		t.Errorf("the sandbox was stopped %d times, want 1", rn.stopCount())
	}
}

func TestHoldForLocalFolder_ALateSeedFailsTheRun(t *testing.T) {
	s, ls, audit, run, _ := localFolderFixture(t)
	s.cfg.LocalFolderSeedWait = 30 * time.Millisecond
	if s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal("the hold let the agent start without a sync")
	}
	if hint := ls.FailureHint(); ls.State() != types.RunFailed || !strings.Contains(hint, `"my-app" did not finish its first sync`) {
		t.Errorf("state %s, hint %q", ls.State(), hint)
	}
	if findAudit(audit.snapshot(), run.ID, "run.seed", "failure") == nil {
		t.Error("the timeout is not audited")
	}
	// A report arriving after the timeout changes nothing.
	s.localFolderPassed(context.Background(), run.ID, "alice@example.com", "/home/agent/work", true, "")
	if ls.seed.SeededAt != nil || ls.sets != 1 {
		t.Errorf("a late report was recorded: %+v (%d writes)", ls.seed, ls.sets)
	}
}

func TestStartAgentOrIdle_AFailedSeedNeverExecsTheAgent(t *testing.T) {
	s, ls, _, run, rn := localFolderFixture(t)
	s.localFolderPassed(context.Background(), run.ID, "alice@example.com", "/home/agent/work", false, "the pass failed")
	s.startAgentOrIdle(context.Background(), run, "sbx", "wardyn-agent:latest", false, runnerExecution{}, nil)
	if n := rn.execs.Load(); n != 0 || ls.State() != types.RunFailed {
		t.Errorf("agent execs %d, state %s: dispatch must hold the agent for its local folder and fail the run", n, ls.State())
	}
}

func TestHoldForLocalFolder_ARunWithoutOneIsNotHeld(t *testing.T) {
	s, ls, audit, run, _ := localFolderFixture(t)
	ls.folder = nil
	if !s.holdForLocalFolder(context.Background(), run, "sbx") {
		t.Fatal("a run with no local folder was held")
	}
	if findAudit(audit.snapshot(), run.ID, "run.seed.hold", "success") != nil {
		t.Error("a run with no local folder audited a wait")
	}
}

func TestLocalFolderPassed_OnlyTheFoldersOwnDirectory(t *testing.T) {
	s, ls, _, run, _ := localFolderFixture(t)
	s.localFolderPassed(context.Background(), run.ID, "alice@example.com", "/home/agent/other", true, "")
	if ls.sets != 0 {
		t.Error("a sync into another directory seeded the folder")
	}
}

func TestLocalFolderRefusal(t *testing.T) {
	s, _, _, _, _ := localFolderFixture(t)
	s.cfg.SSHListenAddr = ":2222"
	ok := createRunRequest{Task: "fix it", LocalFolder: &types.RunLocalFolder{Name: " my-app "}}
	if r := s.localFolderRefusal(&ok); r != nil {
		t.Fatalf("a task run with a local folder was refused: %+v", r)
	}
	if *ok.LocalFolder != (types.RunLocalFolder{Name: "my-app", RemoteDir: "/home/agent/work"}) {
		t.Errorf("normalised = %+v", *ok.LocalFolder)
	}
	cases := map[string]createRunRequest{
		"interactive":     {Task: "x", Interactive: true, LocalFolder: &types.RunLocalFolder{Name: "a"}},
		"background-only": {Task: "x", Experience: types.ExperienceBackground, LocalFolder: &types.RunLocalFolder{Name: "a"}},
		"no task":         {LocalFolder: &types.RunLocalFolder{Name: "a"}},
		"no name":         {Task: "x", LocalFolder: &types.RunLocalFolder{}},
		"bad remote dir":  {Task: "x", LocalFolder: &types.RunLocalFolder{Name: "a", RemoteDir: "/etc"}},
		"control char":    {Task: "x", LocalFolder: &types.RunLocalFolder{Name: "a\nb"}},
	}
	for name, req := range cases {
		if r := s.localFolderRefusal(&req); r == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	s.cfg.SSHListenAddr = ""
	if r := s.localFolderRefusal(&createRunRequest{Task: "x", LocalFolder: &types.RunLocalFolder{Name: "a"}}); r == nil {
		t.Error("a deployment with no SSH gateway accepted a local folder")
	}
}

func TestSSHSyncPassReported(t *testing.T) {
	s, ls, _, run, _ := localFolderFixture(t)
	report := func(v any) []byte {
		b, _ := json.Marshal(v)
		return ssh.Marshal(struct{ Report string }{string(b)})
	}
	pass := &sshSyncPass{}
	if s.sshSyncPassReported(context.Background(), run.ID, "alice@example.com", pass, report(map[string]any{"complete": true})) {
		t.Error("a report before the sync started was accepted")
	}
	pass.start("/home/agent/work")
	if !s.sshSyncPassReported(context.Background(), run.ID, "alice@example.com", pass, report(map[string]any{"complete": true, "deleted_on_laptop": 4})) {
		t.Fatal("a report on a started sync was refused")
	}
	if pass.laptopDeletes() != 4 || ls.seed.SeededAt == nil {
		t.Errorf("deletes %d, seed %+v", pass.laptopDeletes(), ls.seed)
	}
	for _, bad := range [][]byte{[]byte("junk"), report(map[string]any{"deleted_on_laptop": -1}), report(map[string]any{"error": strings.Repeat("x", maxSyncPassReport)})} {
		if s.sshSyncPassReported(context.Background(), run.ID, "alice@example.com", pass, bad) {
			t.Errorf("accepted %q", bad[:min(len(bad), 40)])
		}
	}
}

func TestSFTPOpCounter_ReadsTheFraming(t *testing.T) {
	packet := func(typ byte, body string) []byte {
		b := make([]byte, 4, 64)
		b = append(b, typ, 0, 0, 0, 7) // type, request id
		b = append(b, body...)
		binary.BigEndian.PutUint32(b, uint32(len(b)-4))
		return b
	}
	ext := func(name, rest string) string {
		n := make([]byte, 4)
		binary.BigEndian.PutUint32(n, uint32(len(name)))
		return string(n) + name + rest
	}
	var stream []byte
	stream = append(stream, packet(sftpRemove, "\x00\x00\x00\x01a")...)
	stream = append(stream, packet(3, strings.Repeat("w", 300))...) // an open, never counted
	stream = append(stream, packet(sftpRmdir, "\x00\x00\x00\x01d")...)
	stream = append(stream, packet(sftpRename, "\x00\x00\x00\x01a\x00\x00\x00\x01b")...)
	stream = append(stream, packet(sftpExtended, ext(sftpPosixMove, "\x00\x00\x00\x01a\x00\x00\x00\x01b"))...)
	stream = append(stream, packet(sftpExtended, ext("statvfs@openssh.com", "\x00\x00\x00\x01/"))...)
	for _, step := range []int{1, 3, 7, len(stream)} {
		c := &sftpOpCounter{}
		for i := 0; i < len(stream); i += step {
			c.feed(stream[i:min(i+step, len(stream))])
		}
		if rm, mv := c.counts(); rm != 2 || mv != 2 {
			t.Errorf("chunks of %d: removes %d renames %d, want 2 and 2", step, rm, mv)
		}
	}
}

func TestQueueLaunch_RefusesALocalFolderLikeCreate(t *testing.T) {
	s, _, _, run, _ := localFolderFixture(t)
	s.cfg.SSHListenAddr = "" // the deployment lost its SSH gateway while the run waited
	st := &queuedStart{run: run, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil), cw: newQueueRefusalCapture()}
	end := s.queueLaunch(st, createRunRequest{Task: "fix it", LocalFolder: &types.RunLocalFolder{Name: "my-app"}}, governanceCeiling{})
	if end == nil || !strings.Contains(end.hint, "no SSH gateway") {
		t.Fatalf("end = %+v: a queued start must refuse a local folder the deployment cannot hold, as create does", end)
	}
}
