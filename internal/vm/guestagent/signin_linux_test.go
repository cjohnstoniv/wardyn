// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"errors"
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
	if err := c.PauseAgent(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	waitGroup(t, pid, 3, true)
	if err := c.ResumeAgent(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	waitGroup(t, pid, 3, false)
}

func TestPauseWithNoAgentIsNoExec(t *testing.T) {
	c := newRig(t).client(t)
	if err := c.PauseAgent(ctxT(t)); !errors.Is(err, ErrNoExec) {
		t.Fatalf("pause with no agent = %v, want ErrNoExec", err)
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
