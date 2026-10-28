// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

func TestMacShareTarget(t *testing.T) {
	for target, want := range map[string]string{
		"/home/agent/work":       "/Users/agent/work",
		"/home/agent/a/b":        "/Users/agent/a/b",
		"/home/agent/.wardynx/a": "/Users/agent/.wardynx/a",
	} {
		if got, err := macShareTarget(target); err != nil || got != want {
			t.Errorf("%s = %q, %v; want %q", target, got, err, want)
		}
	}
	for _, target := range []string{"/home/agent", "/home/agent/", "/home/agent/.wardyn", "/home/agent/.wardyn/x",
		"/home/agent/.WARDYN/x", "/work", "/workspace/x", "/home/agentx/a", "/home/agent/a/../b", "relative"} {
		if _, err := macShareTarget(target); err == nil || err.Error() != runner.MacVMShareTarget {
			t.Errorf("%s: %v, want the target sentence", target, err)
		}
	}
}

// The pool: a stuck worker refuses its create at the deadline; stuck workers never exceed the pool,
// so the next create waits for a slot and refuses at its own deadline; a cancelled create returns at
// once; a late finish frees its slot, and its result is dropped.
func TestMacShareProbeBounds(t *testing.T) {
	p := &macShareProbe{slots: make(chan struct{}, 2), wait: 50 * time.Millisecond}
	release := make(chan struct{})
	var live, most atomic.Int32
	stuck := func() (string, error) {
		n := live.Add(1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		<-release
		live.Add(-1)
		return "/late", nil
	}
	for i := 0; i < 4; i++ {
		if _, err := p.run(context.Background(), stuck); !errors.Is(err, errShareUnreadable) {
			t.Fatalf("stuck create %d: %v, want errShareUnreadable", i, err)
		}
	}
	if most.Load() > 2 {
		t.Fatalf("%d workers ran at once; the pool is 2", most.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, err := p.run(ctx, stuck); !errors.Is(err, context.Canceled) || time.Since(start) > 40*time.Millisecond {
		t.Fatalf("cancelled create: %v after %v", err, time.Since(start))
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for len(p.slots) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d slots still held after the workers returned", len(p.slots))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got, err := p.run(context.Background(), func() (string, error) { return "/ok", nil }); err != nil || got != "/ok" {
		t.Fatalf("a quick check after the stuck ones: %q, %v", got, err)
	}
}

// A folder the runner may not read refuses with the folder sentence, which names only its last segment.
func TestMacSharesUnreadableFolder(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 folder")
	}
	r := newMacRig(t)
	dir := filepath.Join(t.TempDir(), "secret-project")
	if err := os.Mkdir(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	spec := macSpec()
	spec.Mounts = []runner.Mount{{Source: dir, Target: "/home/agent/work", ReadOnly: true}}
	if _, err := r.d.CreateSandbox(context.Background(), spec); err == nil || err.Error() != runner.MacVMFolderUnreadable("secret-project") {
		t.Fatalf("CreateSandbox = %v", err)
	}
}

// A run's folders reach the helper's plan at their canonical paths and the guest's Init at their
// /Users/agent targets, read-only as asked.
func TestMacVMCreateSharesFolders(t *testing.T) {
	r := newMacRig(t)
	base := t.TempDir()
	proj, out := filepath.Join(base, "proj"), filepath.Join(base, "out")
	for _, d := range []string{proj, out} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(proj, link); err != nil {
		t.Fatal(err)
	}
	realProj, _ := filepath.EvalSymlinks(proj)
	realOut, _ := filepath.EvalSymlinks(out)
	spec := macSpec()
	spec.Mounts = []runner.Mount{{Source: link, Target: "/home/agent/proj", ReadOnly: true}, {Source: out, Target: "/home/agent/out"}}
	sb, err := r.d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.d.StopSandbox(context.Background(), sb.Ref) }()
	want := []macvm.Share{{Tag: "wardyn0", HostPath: realProj, Target: "/Users/agent/proj", ReadOnly: true},
		{Tag: "wardyn1", HostPath: realOut, Target: "/Users/agent/out"}}
	ms := r.eng.Machines()
	if len(ms) != 1 || len(ms[0].Plan.Shares) != 2 || ms[0].Plan.Shares[0] != want[0] || ms[0].Plan.Shares[1] != want[1] {
		t.Fatalf("plan shares %+v, want %+v", ms[0].Plan.Shares, want)
	}
	r.mu.Lock()
	in := r.init
	r.mu.Unlock()
	wantMounts := []guestagent.Mount{{Tag: "wardyn0", Target: "/Users/agent/proj", ReadOnly: true}, {Tag: "wardyn1", Target: "/Users/agent/out"}}
	if in == nil || len(in.Mounts) != 2 || in.Mounts[0] != wantMounts[0] || in.Mounts[1] != wantMounts[1] {
		t.Fatalf("Init mounts %+v, want %+v", in, wantMounts)
	}
}

// Every share refusal comes before the run directory, the network or the ledger entry exists.
func TestMacVMShareRefusalsComeFirst(t *testing.T) {
	r := newMacRig(t)
	roots := t.TempDir()
	outside := t.TempDir()
	for name, mnt := range map[string]runner.Mount{
		"member outside its roots":     {Source: outside, Target: "/home/agent/w", MemberAuthored: true, ReadOnly: true},
		"writable outside the ceiling": {Source: roots, Target: "/home/agent/w", MemberAuthored: true},
		"the agent's home":             {Source: roots, Target: "/home/agent", ReadOnly: true},
		"the Wardyn folder":            {Source: roots, Target: "/home/agent/.wardyn/x", ReadOnly: true},
		"outside /home/agent":          {Source: roots, Target: "/workspace", ReadOnly: true},
		"the VM root":                  {Source: r.c.Layout.Root, Target: "/home/agent/w", ReadOnly: true},
		"holding the VM root":          {Source: filepath.Dir(r.c.Layout.Root), Target: "/home/agent/w", ReadOnly: true},
		"a missing folder":             {Source: filepath.Join(roots, "nope"), Target: "/home/agent/w", ReadOnly: true},
	} {
		spec := macSpec()
		spec.Mounts = []runner.Mount{mnt}
		spec.UserMountRoots = []string{roots}
		spec.UserWritable = &runner.UserWritableCeiling{}
		if _, err := r.d.CreateSandbox(context.Background(), spec); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if counted, err := r.c.Ledger.Counted(); err != nil || len(counted) != 0 {
			t.Errorf("%s: ledger %+v %v", name, counted, err)
		}
		if runs, _ := os.ReadDir(r.c.Layout.RunsDir()); len(runs) != 0 {
			t.Errorf("%s: run directories %v", name, runs)
		}
		if ms := r.eng.Machines(); len(ms) != 0 {
			t.Errorf("%s: a machine was built", name)
		}
	}
}

// The container's denied mounts are a macOS VM's too. A /work target, which a macOS guest cannot
// take, moves under /home/agent so the source's own refusal is what is checked.
func TestMacVMSharesRefuseTheContainersDeniedMounts(t *testing.T) {
	r := newMacRig(t)
	for _, tc := range deniedMountCases {
		m := tc.m
		if strings.HasPrefix(m.Target, "/work") {
			m.Target = "/home/agent/x"
		}
		spec := macSpec()
		spec.Mounts = []runner.Mount{m}
		_, _, err := r.d.macShares(context.Background(), spec)
		if err == nil {
			t.Errorf("%s: shared %+v", tc.name, m)
			continue
		}
		if _, terr := macShareTarget(m.Target); terr == nil && !strings.Contains(err.Error(), "denied workspace mount") {
			t.Errorf("%s: %v, want the container's denied-mount refusal", tc.name, err)
		}
	}
}
