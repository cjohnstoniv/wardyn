// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// The stop scrub ends the agent account's processes, then scrubs, then looks again, and is done only when
// both passes leave none running. Catches a scrub that runs while a program of the run can still write
// back what it removes, and one reported done when such a program ran during it or could not be ended.
func TestStopScrubIsDoneOnlyWhenTheAgentAccountIsEndedAroundIt(t *testing.T) {
	boom := errors.New("boom")
	for _, c := range []struct {
		name    string
		nilEnd  bool
		running [2]int
		errs    [2]error
		outcome string // the scrub's own
		want    string
	}{
		{name: "ended before, none after", running: [2]int{3, 0}, outcome: ScrubDone, want: ScrubDone},
		{name: "one ran during the scrub", running: [2]int{3, 1}, outcome: ScrubDone, want: ScrubFailed},
		{name: "not ended before", running: [2]int{3, 0}, errs: [2]error{boom, nil}, outcome: ScrubDone, want: ScrubFailed},
		{name: "not ended after", errs: [2]error{nil, boom}, outcome: ScrubDone, want: ScrubFailed},
		{name: "nothing ends the account", nilEnd: true, outcome: ScrubDone, want: ScrubFailed},
		{name: "the scrub failed", outcome: ScrubFailed, want: ScrubFailed},
	} {
		var steps []string
		end := func(ctx context.Context) (int, error) {
			if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > endAgentWait {
				t.Errorf("%s: a pass has no bound of %v", c.name, endAgentWait)
			}
			i := len(steps) / 2 // 0 before the scrub, 1 after it
			steps = append(steps, "end")
			return c.running[i], c.errs[i]
		}
		if c.nilEnd {
			end = nil
		}
		scrub := func(_ context.Context, when string) ScrubReport {
			steps = append(steps, "scrub")
			return ScrubReport{When: when, Outcome: c.outcome, Removed: map[string]int{ScrubRunDirs: 1}}
		}
		r := stopScrub(context.Background(), end, scrub)
		wantSteps := []string{"end", "scrub", "end"}
		if c.nilEnd {
			wantSteps = []string{"scrub"}
		}
		if !slices.Equal(steps, wantSteps) {
			t.Errorf("%s: steps %v, want %v", c.name, steps, wantSteps)
		}
		if r.Outcome != c.want || r.When != ScrubAtStop || r.Removed[ScrubRunDirs] != 1 {
			t.Errorf("%s: report %+v, want outcome %s with the scrub's removals", c.name, r, c.want)
		}
	}
}

// endAccount signs the account out through launchd before it kills, then kills until nothing is left, and
// fails when a process outlives its time. Catches a stop that leaves a program of the run alive, or the
// desktop session's launchd domains loaded, so launchd starts its apps and agents again.
func TestEndAccountSignsOutThenKillsUntilNoneIsLeft(t *testing.T) {
	r := &fakeRunner{}
	lists := [][]int{{10, 11}, {10, 11}, {11}, {}}
	var killed []int
	list := func() ([]int, error) {
		if len(r.calls) < 2 && len(lists) < 4 {
			t.Error("listed for a kill before the sign-out")
		}
		l := lists[0]
		lists = lists[1:]
		return l, nil
	}
	kill := func(pid int) error {
		if len(r.calls) != 2 {
			t.Errorf("killed %d before the sign-out", pid)
		}
		killed = append(killed, pid)
		return nil
	}
	n, err := endAccount(context.Background(), r, 502, list, kill)
	if n != 2 || err != nil || !slices.Equal(killed, []int{10, 11, 11}) {
		t.Errorf("endAccount = %d, %v, killed %v; want 2 running, every one killed until none is left", n, err, killed)
	}
	want := [][]string{{"/bin/launchctl", "bootout", "gui/502"}, {"/bin/launchctl", "bootout", "user/502"}}
	if !slices.EqualFunc(r.calls, want, slices.Equal) {
		t.Errorf("sign-out %q, want %q", r.calls, want)
	}

	r = &fakeRunner{}
	if n, err := endAccount(context.Background(), r, 502, func() ([]int, error) { return nil, nil }, kill); n != 0 || err != nil || len(r.calls) != 0 {
		t.Errorf("with none running: %d, %v, %q; want no sign-out", n, err, r.calls)
	}
	if _, err := endAccount(context.Background(), r, 502, func() ([]int, error) { return nil, errors.New("no list") }, kill); err == nil {
		t.Error("a list that fails is not a failure")
	}
	listed := 0
	lost := func() ([]int, error) {
		if listed++; listed > 1 {
			return nil, errors.New("no list")
		}
		return []int{7}, nil
	}
	if _, err := endAccount(context.Background(), &fakeRunner{}, 502, lost, func(int) error { return nil }); err == nil {
		t.Error("a list that fails after the sign-out is not a failure")
	}
	// A sign-out that uses its whole time leaves the other domain its own.
	var ends []time.Time
	bounds := runnerFunc(func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		dl, ok := ctx.Deadline()
		if !ok || time.Until(dl) > bootoutWait {
			t.Errorf("a sign-out has no bound of %v", bootoutWait)
		}
		ends = append(ends, dl)
		time.Sleep(2 * time.Millisecond)
		return nil, nil
	})
	lists = [][]int{{10}, {}}
	if _, err := endAccount(context.Background(), bounds, 502, func() ([]int, error) { l := lists[0]; lists = lists[1:]; return l, nil }, func(int) error { return nil }); err != nil || len(ends) != 2 || !ends[1].After(ends[0]) {
		t.Errorf("sign-out bounds %v, %v; want one of its own for each domain", ends, err)
	}
	if 2*bootoutWait >= endAgentWait {
		t.Errorf("two sign-outs of %v leave a pass of %v no time to kill", bootoutWait, endAgentWait)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := endAccount(ctx, &fakeRunner{}, 502, func() ([]int, error) { return []int{7}, nil }, func(int) error { return nil }); err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("a process that never ends: %v after %v, want a failure at the bound", err, time.Since(start))
	}
}

// ofAccount counts a live process whose real, effective or saved uid is the account's, and no zombie.
// Catches a listing that misses a program the account started through a setuid program, which keeps
// only its real uid, so the stop scrub would run while it lives.
func TestOfAccountCountsRealEffectiveAndSavedUIDs(t *testing.T) {
	for _, c := range []struct {
		stat              int8
		ruid, euid, svuid uint32
		want              bool
	}{
		{2, 502, 502, 502, true},
		{2, 502, 0, 0, true}, // a setuid-root program the account started
		{2, 0, 502, 0, true},
		{2, 0, 0, 502, true},
		{2, 0, 501, 0, false},
		{procZombie, 502, 502, 502, false},
	} {
		if got := ofAccount(c.stat, c.ruid, c.euid, c.svuid, 502); got != c.want {
			t.Errorf("ofAccount(%d, %d, %d, %d, 502) = %v, want %v", c.stat, c.ruid, c.euid, c.svuid, got, c.want)
		}
	}
}

// keepEnded ends the account again and again until the guest goes down. Catches a stop that stops looking
// once the scrub has answered, so a program started after it runs until the power-off.
func TestKeepEndedEndsTheAccountUntilTheGuestGoesDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	passes, done := make(chan struct{}, 8), make(chan struct{})
	go func() {
		defer close(done)
		keepEnded(ctx, func(context.Context) (int, error) {
			select {
			case passes <- struct{}{}:
			default:
			}
			return 0, nil
		})
	}()
	for range 3 {
		select {
		case <-passes:
		case <-time.After(5 * time.Second):
			t.Fatal("keepEnded stopped ending the account while the guest was up")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("keepEnded outlived the guest's context")
	}
}
