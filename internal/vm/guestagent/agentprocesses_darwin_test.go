// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// POST /v1/scrub ends the agent account before the scrub and looks again after it, and with no way to
// end the account it reports failed. Catches a handler that scrubs while a program of the run can
// still write back what the scrub removes, and one that reports done with no way to end those programs.
func TestDarwinStopScrubEndsTheAgentAccountAroundTheScrub(t *testing.T) {
	for _, withEnd := range []bool{true, false} {
		s, _, h := newDarwinServer(t)
		var steps []string
		s.Scrub = func(_ context.Context, when string) ScrubReport {
			steps = append(steps, "scrub")
			return ScrubReport{When: when, Outcome: ScrubDone}
		}
		want, wantSteps := ScrubFailed, []string{"scrub"}
		if withEnd {
			s.EndAgent = func(context.Context) (int, error) { steps = append(steps, "end"); return 0, nil }
			want, wantSteps = ScrubDone, []string{"end", "scrub", "end"}
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/scrub", nil))
		var r ScrubReport
		if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil || r.Outcome != want || !slices.Equal(steps, wantSteps) {
			t.Errorf("EndAgent set %v: %d %s (%v), steps %v; want %s after %v", withEnd, w.Code, w.Body, err, steps, want, wantSteps)
		}
	}
}

// After it answers, the stop scrub keeps ending the agent account until the guest goes down. Catches a
// handler that stops looking once it has answered, so a program of the account that starts after the
// second look runs until the power-off on a disk recorded as scrubbed.
func TestDarwinStopScrubKeepsTheAgentAccountEndedAfterItAnswers(t *testing.T) {
	s, _, h := newDarwinServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ends atomic.Int32
	s.Guest = ctx
	s.EndAgent = func(context.Context) (int, error) { ends.Add(1); return 0, nil }
	s.Scrub = func(_ context.Context, when string) ScrubReport { return ScrubReport{When: when, Outcome: ScrubDone} }
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/scrub", nil))
	for end := time.Now().Add(5 * time.Second); ends.Load() < 5; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("the agent account was ended %d times; want it kept ended after the answer", ends.Load())
		}
	}
}

// accountProcesses lists a live process of the uid, and not one that has ended (a zombie) or is another
// uid's. Catches a listing that misses a program of the agent account, so the stop scrub runs while it
// lives, or one that never empties, so every stop is recorded as not scrubbed.
func TestDarwinAccountProcessesListsTheLiveProcessesOfTheUID(t *testing.T) {
	uid := uint32(os.Getuid())
	cmd := exec.Command("/bin/sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	pids, err := accountProcesses(uid)
	if err != nil || !slices.Contains(pids, pid) || !slices.Contains(pids, os.Getpid()) {
		t.Errorf("accountProcesses(%d) = %v, %v; want this test %d and its child %d", uid, pids, err, os.Getpid(), pid)
	}
	if uid != 0 && slices.Contains(pids, 1) {
		t.Errorf("accountProcesses(%d) lists launchd, which is root's", uid)
	}
	// Killed and not yet waited for, the child is a zombie until Wait.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if pids, err = accountProcesses(uid); err == nil && !slices.Contains(pids, pid) {
			break
		}
		if time.Now().After(end) {
			t.Errorf("accountProcesses(%d) still lists the ended child %d: %v, %v", uid, pid, pids, err)
			break
		}
	}
	_ = cmd.Wait()
}
