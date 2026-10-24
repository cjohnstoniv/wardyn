// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// groupStates is each live process in process group pgid by its state letter (R, S, T, ...).
func groupStates(t *testing.T, pgid int) map[int]string {
	t.Helper()
	ents, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]string{}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// pid (comm) state ppid pgrp ...: comm may hold spaces, so split after its last ')'.
		f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
		if len(f) > 2 && f[2] == strconv.Itoa(pgid) {
			out[pid] = f[0]
		}
	}
	return out
}

func waitGroup(t *testing.T, pgid, want int, stopped bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; {
		st := groupStates(t, pgid)
		n := 0
		for _, s := range st {
			if (s == "T") == stopped {
				n++
			}
		}
		if len(st) == want && n == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process group %d = %v, want %d processes, stopped=%v", pgid, st, want, stopped)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The pause behind "Pause the agent to sign in" (#2972) stops the agent's whole process group, not
// only its first process, and the resume continues every one of them.
func TestPauseStopsTheAgentsProcessGroup(t *testing.T) {
	r := newRig(t)
	c := r.client(t)
	id, err := c.Start(ctxT(t), ExecRequest{Argv: []string{"sh", "-c", "sleep 30 & sleep 30 & wait"}, Detach: true})
	if err != nil {
		t.Fatal(err)
	}
	pid := r.srv.lookup(id).pid
	t.Cleanup(func() { _ = syscallKillGroup(pid) })
	waitGroup(t, pid, 3, false)
	if _, err := c.PauseAgent(ctxT(t), 1); err != nil {
		t.Fatal(err)
	}
	waitGroup(t, pid, 3, true)
	if _, err := c.ResumeAgent(ctxT(t), 1); err != nil {
		t.Fatal(err)
	}
	waitGroup(t, pid, 3, false)
}

func TestPauseWithNoAgentIsNoExec(t *testing.T) {
	c := newRig(t).client(t)
	if _, err := c.PauseAgent(ctxT(t), 1); !errors.Is(err, ErrNoExec) {
		t.Fatalf("pause with no agent = %v, want ErrNoExec", err)
	}
}

// startGroup starts a detached agent of two processes in one group and returns the group.
func startGroup(t *testing.T, r *rig, c *Client) int {
	t.Helper()
	id, err := c.Start(ctxT(t), ExecRequest{Argv: []string{"sh", "-c", "sleep 30 & wait"}, Detach: true})
	if err != nil {
		t.Fatal(err)
	}
	pid := r.srv.lookup(id).pid
	t.Cleanup(func() { _ = syscallKillGroup(pid) })
	waitGroup(t, pid, 2, false)
	return pid
}

// signal sends one pause or resume of generation gen and answers whether the agent applied it: a
// dropped one is its 409, and an applied one says it was ordered.
func signal(t *testing.T, c *Client, pause bool, gen int64) bool {
	t.Helper()
	call := c.ResumeAgent
	if pause {
		call = c.PauseAgent
	}
	fenced, err := call(ctxT(t), gen)
	if err != nil {
		if !strings.Contains(err.Error(), "409") {
			t.Fatalf("pause=%v generation %d = %v, want applied or dropped with 409", pause, gen, err)
		}
		return false
	}
	if !fenced {
		t.Fatalf("pause=%v generation %d was applied but not answered fenced_pause", pause, gen)
	}
	return true
}

// A continue that reaches the guest after a later pause's stop, as a delayed one does once wardynd
// lost the run's lock, is dropped: the agent stays stopped under the later pause, whose own continue
// still runs it. A stop delivered after a later pause's continue is dropped the same way.
func TestAStaleContinueAfterALaterStopIsDropped(t *testing.T) {
	r := newRig(t)
	c := r.client(t)
	pid := startGroup(t, r, c)
	if !signal(t, c, true, 2) { // pause 2's stop
		t.Fatal("pause 2's stop was dropped")
	}
	waitGroup(t, pid, 2, true)
	if signal(t, c, false, 1) { // pause 1's continue, delivered late
		t.Fatal("pause 1's continue, older than pause 2's stop, was applied")
	}
	waitGroup(t, pid, 2, true)
	if !signal(t, c, false, 2) { // pause 2's resume
		t.Fatal("pause 2's continue was dropped")
	}
	waitGroup(t, pid, 2, false)

	// Reordered the other way: pause 1's stop, delivered after pause 3 has stopped and continued.
	for _, step := range []struct {
		pause   bool
		gen     int64
		applied bool
		stopped bool
	}{{true, 3, true, true}, {false, 3, true, false}, {true, 1, false, false}, {true, 4, true, true}, {true, 4, true, true}} {
		if got := signal(t, c, step.pause, step.gen); got != step.applied {
			t.Fatalf("pause=%v generation %d applied = %v, want %v", step.pause, step.gen, got, step.applied)
		}
		waitGroup(t, pid, 2, step.stopped)
	}
}

// A runner or wardynd that predates the generation sends none (an empty body, or 0): the agent
// signals it as it arrives, as it did before, and answers no fenced_pause.
func TestAPauseWithNoGenerationIsSignalledAsBefore(t *testing.T) {
	r := newRig(t)
	c := r.client(t)
	pid := startGroup(t, r, c)
	for _, step := range []struct {
		path    string
		in      any
		stopped bool
	}{{"/v1/agent/pause", nil, true}, {"/v1/agent/resume", nil, false}, {"/v1/agent/pause", AgentSignal{}, true}, {"/v1/agent/resume", AgentSignal{}, false}} {
		var res AgentSignalResult
		if err := c.do(ctxT(t), http.MethodPost, step.path, step.in, &res); err != nil || res.FencedPause {
			t.Fatalf("%s with %v = %+v %v, want applied with no fenced_pause", step.path, step.in, res, err)
		}
		waitGroup(t, pid, 2, step.stopped)
	}
}

// A guest with swap on is refused before its marker, so the sign-in store never runs where the
// kernel can page it out; a header-only list is no swap.
func TestInitRefusesAGuestWithSwap(t *testing.T) {
	boot := t.TempDir()
	swaps := filepath.Join(t.TempDir(), "swaps")
	const header = "Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n"
	if err := os.WriteFile(swaps, []byte(header+"/dev/zram0                              partition\t4005884\t\t0\t\t100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newRigAt(t, boot)
	r.srv.Swaps = swaps
	if _, err := r.client(t).Init(ctxT(t), Init{}); err == nil || !strings.Contains(err.Error(), "the guest has swap on (/dev/zram0)") {
		t.Fatalf("init with zram on = %v, want refused", err)
	}
	if err := os.WriteFile(swaps, []byte(header), 0o600); err != nil {
		t.Fatal(err)
	}
	r = newRigAt(t, boot)
	r.srv.Swaps = swaps
	if _, err := r.client(t).Init(ctxT(t), Init{}); err != nil {
		t.Fatalf("init with no swap after a refused one = %v", err)
	}
}

// Init mounts the sign-in store before it answers, read-only when sign-ins are off, and says which.
func TestInitReportsTheSignInStore(t *testing.T) {
	for _, off := range []bool{false, true} {
		r := newRig(t)
		var got []bool
		r.srv.SignInStore = func(o bool) error { got = append(got, o); return nil }
		res, err := r.client(t).Init(ctxT(t), Init{SignInsOff: off})
		want := map[bool]string{false: SignInsMemory, true: SignInsNone}[off]
		if err != nil || res.SignIns != want || len(got) != 1 || got[0] != off {
			t.Fatalf("init off=%v = %+v, %v, store calls %v; want %q", off, res, err, got, want)
		}
	}
	r := newRig(t)
	r.srv.SignInStore = func(bool) error { return errors.New("no tmpfs") }
	if _, err := r.client(t).Init(ctxT(t), Init{}); err == nil || !strings.Contains(err.Error(), "the sign-in store: no tmpfs") {
		t.Fatalf("init with a failed store = %v, want refused", err)
	}
}

// The real store (root only: it mounts): each path is a tmpfs, the agent's, mode 0700; read-only off.
func TestMountSignInStoreIsTmpfs(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("mounting a tmpfs needs root; the VM conformance suite checks it in a booted guest")
	}
	for _, off := range []bool{false, true} {
		home := t.TempDir()
		if err := MountSignInStore(home, off); err != nil {
			t.Fatal(err)
		}
		for _, rel := range SignInPaths {
			p := filepath.Join(home, rel)
			t.Cleanup(func() { _ = unmount(p) })
			if !isTmpfs(t, p) {
				t.Errorf("%s is not a tmpfs", p)
			}
			err := os.WriteFile(filepath.Join(p, "probe"), nil, 0o600)
			if (err == nil) == off {
				t.Errorf("off=%v: write in %s = %v", off, p, err)
			}
		}
	}
	home := t.TempDir()
	if err := os.Symlink("/etc", filepath.Join(home, ".mozilla")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".local/share/keyrings"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := MountSignInStore(home, false)
	t.Cleanup(func() {
		_ = unmount(filepath.Join(home, ".local/share/keyrings"))
		_ = unmount(filepath.Join(home, ".local/share/kwalletd"))
	})
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("store over a symbolic link = %v, want refused", err)
	}
}

// The reference guest image's build files configure no swap and no zram: nothing installs a swap
// generator or makes a swap file. The VM conformance suite checks /proc/swaps in a booted guest.
func TestReferenceImageConfiguresNoSwap(t *testing.T) {
	b, err := os.ReadFile("../../../deploy/images/vm-linux/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, bad := range []string{"zram", "swapon", "mkswap", "swapfile", "dphys-swapfile", "systemd-zram-generator"} {
			if strings.Contains(strings.ToLower(line), bad) {
				t.Errorf("the reference guest image's Dockerfile configures swap (%q): %s", bad, line)
			}
		}
	}
	if !strings.Contains(string(b), "wardyn-guest-agent") {
		t.Fatal("the reference guest image no longer installs the guest agent that refuses swap")
	}
}

// Within one generation the stop comes before the continue. A stop again before the continue (wardynd
// stopping a still marked pause again, or a duplicate) acts; a stop delivered after its own pause's
// continue is dropped, so it never stops an agent whose pause was resumed; a continue sent again acts.
// A stop delayed past its own failed pause's continue is dropped the same way, leaving the agent run.
func TestAStopAfterItsOwnContinueIsDropped(t *testing.T) {
	r := newRig(t)
	c := r.client(t)
	pid := startGroup(t, r, c)
	for i, step := range []struct {
		pause   bool
		gen     int64
		applied bool
		stopped bool
	}{
		{true, 1, true, true},   // pause 1
		{true, 1, true, true},   // its stop again, or a duplicate
		{false, 1, true, false}, // its resume
		{true, 1, false, false}, // a duplicate stop, delivered after the resume
		{false, 1, true, false}, // a duplicate continue
		{false, 2, true, false}, // pause 2 failed: its continue reached the agent first
		{true, 2, false, false}, // and its stop only after
		{true, 3, true, true},   // pause 3
		{false, 3, true, false}, // its resume
	} {
		if got := signal(t, c, step.pause, step.gen); got != step.applied {
			t.Fatalf("step %d: pause=%v generation %d applied = %v, want %v", i, step.pause, step.gen, got, step.applied)
		}
		waitGroup(t, pid, 2, step.stopped)
	}
}

// Once a request with a generation has acted, a pause without one (from a runner or wardynd that
// predates it) is still obeyed: a pause only ever protects a sign-in. It moves nothing, so the fenced
// resume that follows still runs the agent.
func TestAnUnfencedPauseAfterAFencedOneIsObeyed(t *testing.T) {
	r := newRig(t)
	c := r.client(t)
	pid := startGroup(t, r, c)
	signal(t, c, true, 1)
	signal(t, c, false, 1)
	waitGroup(t, pid, 2, false)
	if err := c.do(ctxT(t), http.MethodPost, "/v1/agent/pause", nil, nil); err != nil {
		t.Fatalf("pause with no generation after a fenced resume = %v, want it obeyed", err)
	}
	waitGroup(t, pid, 2, true)
	if !signal(t, c, false, 1) {
		t.Fatal("the fenced resume after an unfenced pause was dropped")
	}
	waitGroup(t, pid, 2, false)
}

// Once a request with a generation has acted, a resume without one is dropped with its reason: it
// cannot undo a fenced pause, and the agent stays stopped until a resume with a generation comes.
func TestAnUnfencedResumeAfterAFencedPauseIsDropped(t *testing.T) {
	r := newRig(t)
	c := r.client(t)
	pid := startGroup(t, r, c)
	signal(t, c, true, 1)
	waitGroup(t, pid, 2, true)
	const want = `409 Conflict: "stale_unfenced_resume: a resume with no generation came after one with a generation; the agent stays paused"`
	if _, err := c.ResumeAgent(ctxT(t), 0); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("resume with no generation after a fenced pause = %v, want the guest's %s", err, want)
	}
	waitGroup(t, pid, 2, true)
	signal(t, c, false, 1)
	waitGroup(t, pid, 2, false)
}

// A stop with no generation that acts after a restart of the agent's service leaves the kept order as
// it was, so the fenced continue that follows still runs the agent.
func TestAnUnfencedStopAfterARestartLeavesTheKeptOrder(t *testing.T) {
	boot := t.TempDir()
	r := newRigAt(t, boot)
	c := r.client(t)
	startGroup(t, r, c)
	signal(t, c, true, 1)
	signal(t, c, false, 1)
	kept, err := os.ReadFile(filepath.Join(boot, fenceFile))
	if err != nil {
		t.Fatal(err)
	}

	restarted := newRigAt(t, boot)
	rc := restarted.client(t)
	rpid := startGroup(t, restarted, rc)
	if err := rc.do(ctxT(t), http.MethodPost, "/v1/agent/pause", nil, nil); err != nil {
		t.Fatalf("pause with no generation after a restart = %v, want it obeyed", err)
	}
	waitGroup(t, rpid, 2, true)
	if now, err := os.ReadFile(filepath.Join(boot, fenceFile)); err != nil || string(now) != string(kept) {
		t.Fatalf("kept order after the unfenced stop = %q %v, want %q", now, err, kept)
	}
	if !signal(t, rc, false, 1) {
		t.Fatal("the fenced continue after an unfenced stop was dropped")
	}
	waitGroup(t, rpid, 2, false)
}

// The order outlives a restart of the guest agent's service, whose processes may outlive it too: the
// marker directory keeps it. A new boot, whose processes are new, starts it afresh.
func TestThePauseOrderOutlivesAServiceRestartNotABoot(t *testing.T) {
	boot := t.TempDir()
	r := newRigAt(t, boot)
	c := r.client(t)
	pid := startGroup(t, r, c)
	signal(t, c, true, 2)
	signal(t, c, false, 2)
	waitGroup(t, pid, 2, false)

	restarted := newRigAt(t, boot)
	rc := restarted.client(t)
	rpid := startGroup(t, restarted, rc)
	if signal(t, rc, true, 2) {
		t.Fatal("after a service restart, a stop delivered after its own pause's continue acted")
	}
	if signal(t, rc, false, 1) {
		t.Fatal("after a service restart, a continue older than the newest pause acted")
	}
	waitGroup(t, rpid, 2, false)

	booted := newRigAt(t, t.TempDir())
	bc := booted.client(t)
	bpid := startGroup(t, booted, bc)
	if !signal(t, bc, true, 1) {
		t.Fatal("a new boot's first pause was dropped")
	}
	waitGroup(t, bpid, 2, true)
}
