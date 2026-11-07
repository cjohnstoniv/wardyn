// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// guiEnvRunner plays the guest's status file over ExecStream: out answers each read, and exit is the cat's.
type guiEnvRunner struct {
	runner.Runner
	mu    sync.Mutex
	reads int
	argvs [][]string
	out   func(n int) (stdout string, exit int)
}

func (g *guiEnvRunner) ExecStream(_ context.Context, _ string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	g.mu.Lock()
	g.reads++
	n := g.reads
	g.argvs = append(g.argvs, spec.Argv)
	g.mu.Unlock()
	stdout, exit := g.out(n)
	return &runner.ExecSession{
		Stdin:  nopWriteCloser{io.Discard},
		Stdout: strings.NewReader(stdout),
		Stderr: strings.NewReader(""),
		Wait:   func() (int, error) { return exit, nil },
		Close:  func() error { return nil },
	}, nil
}

func (g *guiEnvRunner) readCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reads
}

// guiEnvRig is a window rig whose guest answers the status file from out, with the waits made short.
func guiEnvRig(t *testing.T, out func(n int) (string, int)) (*macWindowRig, *guiEnvRunner) {
	t.Helper()
	r := newMacWindowRig(t)
	g := &guiEnvRunner{Runner: r.h.srv.cfg.Runner, out: out}
	r.h.srv.cfg.Runner = g
	oldEvery, oldWait, oldTTL := macGUIEnvPollEvery, macGUIEnvWait, macGUIEnvCacheTTL
	macGUIEnvPollEvery, macGUIEnvWait, macGUIEnvCacheTTL = time.Millisecond, 300*time.Millisecond, 0
	t.Cleanup(func() { macGUIEnvPollEvery, macGUIEnvWait, macGUIEnvCacheTTL = oldEvery, oldWait, oldTTL })
	return r, g
}

func always(line string, exit int) func(int) (string, int) {
	return func(int) (string, int) { return line, exit }
}

func TestMacWindowIsRefusedWhileTheDesktopSettingsArePending(t *testing.T) {
	r, g := guiEnvRig(t, always("session_env=pending\n", 0))
	if _, rf := r.open(macWindowTriggerCLI); rf == nil || macWindowRefusalReason(rf) != reasonMacWindowPreparing || rf.body.Error != macWindowSettingsPendingMsg {
		t.Fatalf("open while pending = %+v, want the preparing refusal", rf)
	}
	if r.sub.openCount() != 0 {
		t.Fatalf("the runner was asked for a window %d times ahead of the settings", r.sub.openCount())
	}
	if len(g.argvs) == 0 || strings.Join(g.argvs[0], " ") != strings.Join(macGUIEnvArgv, " ") {
		t.Errorf("the guest was asked %v, want %v", g.argvs, macGUIEnvArgv)
	}
	v := r.h.srv.macWindowViewFor(context.Background(), r.h.run, r.auth())
	if v == nil || v.State != macWindowStatePreparing || v.Message != macWindowSettingsPendingMsg {
		t.Fatalf("status while pending = %+v, want preparing with the settings sentence", v)
	}
	g.out = always("session_env=set vars=4 via=asuser\n", 0)
	if _, rf := r.open(macWindowTriggerCLI); rf != nil {
		t.Fatalf("open once the settings are set: %+v", rf)
	}
}

func TestMacWindowAtStartWaitsForTheDesktopSettings(t *testing.T) {
	release := make(chan struct{})
	r, g := guiEnvRig(t, func(int) (string, int) {
		select {
		case <-release:
			return "session_env=set vars=4 via=asuser\n", 0
		default:
			return "session_env=pending\n", 0
		}
	})
	if err := r.h.srv.macWindowStorePending(context.Background(), r.h.run.ID, r.auth()); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); r.h.srv.macWindowAtStart(context.Background(), r.h.run.ID) }()
	macWindowEventually(t, "the start to poll the settings", func() bool { return g.readCount() >= 3 })
	if r.sub.openCount() != 0 {
		t.Fatalf("the window opened ahead of the settings")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the start never finished")
	}
	if r.sub.openCount() != 1 {
		t.Fatalf("opens = %d after the settings were set, want 1", r.sub.openCount())
	}
}

// guiEnvAtStart runs the at-start window to its end and returns the settings sentence the run's window state
// would carry with no window open (macGUIEnvNote on an open view).
func guiEnvAtStart(t *testing.T, r *macWindowRig) string {
	t.Helper()
	if err := r.h.srv.macWindowStorePending(context.Background(), r.h.run.ID, r.auth()); err != nil {
		t.Fatal(err)
	}
	r.h.srv.macWindowAtStart(context.Background(), r.h.run.ID)
	v := macWindowView{State: macWindowStateOpen}
	r.h.srv.macGUIEnvNote(context.Background(), r.h.run, &v)
	return v.Message
}

func TestMacWindowAtStartOpensAfterTheWaitWhenSettingsStayPendingAndSaysSo(t *testing.T) {
	r, _ := guiEnvRig(t, always("session_env=pending\n", 0))
	macGUIEnvWait = 30 * time.Millisecond
	msg := guiEnvAtStart(t, r)
	// The wait is bounded: the window is asked for once it ends, so a guest that never says is not a window that never opens.
	if r.sub.openCount() != 1 {
		t.Fatalf("opens = %d after the wait, want 1", r.sub.openCount())
	}
	// It opened without the settings confirmed, so it says so, as for failed: the retry, and no hold on a later open.
	if !strings.Contains(msg, "agent-run --session-env-retry") {
		t.Errorf("settings sentence after the wait ran out while pending = %q, want the retry", msg)
	}
	if rf := r.h.srv.macGUIEnvRefusal(context.Background(), r.h.run, false); rf != nil {
		t.Errorf("an open after the wait ran out was refused: %+v", rf)
	}
}

func TestMacWindowAtStartSaysSoWhenTheStatusIsLostAfterPending(t *testing.T) {
	r, _ := guiEnvRig(t, func(n int) (string, int) {
		if n == 1 {
			return "session_env=pending\n", 0
		}
		return "", 1 // the read fails from here on
	})
	msg := guiEnvAtStart(t, r)
	if r.sub.openCount() != 1 {
		t.Fatalf("opens = %d after the read failed, want 1", r.sub.openCount())
	}
	if !strings.Contains(msg, "agent-run --session-env-retry") {
		t.Errorf("settings sentence after pending then a failed read = %q, want the retry", msg)
	}
	// A guest that never said anything (an older one, with no status file) opens as before, with no sentence.
	r2, _ := guiEnvRig(t, always("", 1))
	if msg := guiEnvAtStart(t, r2); msg != "" || r2.sub.openCount() != 1 {
		t.Errorf("older guest at start: opens = %d, sentence %q; want 1 and none", r2.sub.openCount(), msg)
	}
}

func TestMacWindowOpensWithFailedSettingsAndSaysSoWithARetry(t *testing.T) {
	r, _ := guiEnvRig(t, always("session_env=failed reason=setenv_failed\n", 0))
	w, rf := r.open(macWindowTriggerCLI)
	if rf != nil || w == nil {
		t.Fatalf("a failed setter held the window: %+v", rf)
	}
	r2, _ := guiEnvRig(t, always("session_env=failed reason=not_signed_in\n", 0))
	v2 := r2.h.srv.macWindowViewFor(context.Background(), r2.h.run, r2.auth())
	if v2 == nil || v2.State != macWindowStateOpen || !strings.Contains(v2.Message, "agent-run --session-env-retry") || !strings.Contains(v2.Message, "not signed in") {
		t.Fatalf("status with failed settings = %+v, want open with the reason and the retry command", v2)
	}
}

func TestMacWindowIsNotHeldByAGuestThatCannotSayAndNeverHoldsRunStartup(t *testing.T) {
	for name, out := range map[string]func(int) (string, int){
		"no status file (an older guest)": always("", 1),
		"garbage":                         always("hello\n", 0),
	} {
		t.Run(name, func(t *testing.T) {
			r, _ := guiEnvRig(t, out)
			if _, rf := r.open(macWindowTriggerCLI); rf != nil {
				t.Fatalf("a guest that cannot say held the window: %+v", rf)
			}
		})
	}
	// The wait is the at-start window's alone: the check that reads (a GET of the run) never blocks.
	r, _ := guiEnvRig(t, always("session_env=pending\n", 0))
	start := time.Now()
	r.h.srv.macWindowViewFor(context.Background(), r.h.run, r.auth())
	if time.Since(start) > macGUIEnvWait/2 {
		t.Errorf("a read of the run's window state waited %v", time.Since(start))
	}
}

// The per-run answers are dropped when the run ends, so they do not pile up one per run.
func TestMacGUIEnvEntriesAreDroppedWhenTheRunEnds(t *testing.T) {
	r, _ := guiEnvRig(t, always("session_env=pending\n", 0))
	r.h.srv.macGUIEnvFresh(context.Background(), r.h.run)
	macGUIEnvGaveUp.Store(r.h.run.ID, true)
	_ = r.h.srv.revokeRunCascade(context.Background(), r.h.run.ID)
	if _, ok := macGUIEnvSeen.Load(r.h.run.ID); ok {
		t.Error("the run's last settings answer is still kept after it ended")
	}
	if _, ok := macGUIEnvGaveUp.Load(r.h.run.ID); ok {
		t.Error("the run's gave-up mark is still kept after it ended")
	}
}
