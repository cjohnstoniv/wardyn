// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/runner/offline"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
)

// A state directory another user owns is refused before anything is written; a missing one says serve never ran.
func TestContain_RefusesForeignOwnedStateDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns every directory this test could pick")
	}
	err := checkStateOwner("/")
	if err == nil || !strings.Contains(err.Error(), "belongs to uid 0") {
		t.Fatalf("a root-owned state dir = %v, want refused", err)
	}
	if err := checkStateOwner(filepath.Join(t.TempDir(), "offline")); err == nil || !strings.Contains(err.Error(), "never run") {
		t.Fatalf("a missing state dir = %v, want refused", err)
	}
	if err := checkStateOwner(t.TempDir()); err != nil {
		t.Fatalf("this user's own state dir = %v, want accepted", err)
	}
}

// probeSub is the least substrate offline.New accepts.
type probeSub struct{ substrate.Substrate }

func (probeSub) Name() string                            { return "docker" }
func (probeSub) StopProxy(context.Context, string) error { return nil }

// contain's probe takes a shared lock on daemon.lock for two system calls; a serve starting at that moment retries its
// exclusive lock, so 200 probes racing 200 starts never fail a start.
func TestContain_ProbeNeverFailsAStartingServe(t *testing.T) {
	dir := t.TempDir()
	runnerID := uuid.New()
	start := func() error {
		m, err := offline.New(dir, runnerID, probeSub{}, uuid.UUID.String,
			func(context.Context, uuid.UUID, string) (int64, bool, error) { return 0, true, nil }, func(uuid.UUID) error { return nil })
		if err != nil {
			return err
		}
		return m.Close()
	}
	if err := start(); err != nil {
		t.Fatal(err)
	}
	offlineDir := filepath.Join(dir, "offline")
	var stop atomic.Bool
	var probes, serving atomic.Int32
	done := make(chan error, 1)
	go func() {
		for !stop.Load() || probes.Load() < 200 {
			up, err := probeServe(offlineDir)
			if err != nil {
				done <- err
				return
			}
			probes.Add(1)
			if up {
				serving.Add(1)
			}
		}
		done <- nil
	}()
	for i := range 200 {
		if err := start(); err != nil {
			stop.Store(true)
			<-done
			t.Fatalf("start %d failed beside the probes: %v", i, err)
		}
	}
	stop.Store(true)
	if err := <-done; err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probes.Load() < 200 {
		t.Fatalf("probes = %d, want at least 200", probes.Load())
	}
}

// runContainChild runs wardyn-runnerd contain in a child process and answers its real exit code and output.
func runContainChild(t *testing.T, test string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+test+"$")
	cmd.Env = append(os.Environ(), "WARDYN_CONTAIN_CHILD="+strings.Join(args, "\x1f"), "HOME="+t.TempDir(), "XDG_CONFIG_HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("child: %v", err)
	}
	return cmd.ProcessState.ExitCode(), string(out)
}

// containChild is the child side: it runs main with the arguments its parent passed, and exits.
func containChild() {
	if a := os.Getenv("WARDYN_CONTAIN_CHILD"); a != "" {
		os.Args = append([]string{"wardyn-runnerd", "contain"}, strings.Split(a, "\x1f")...)
		main()
	}
}

func privateStateDir(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	offlineDir := filepath.Join(dir, "offline")
	if err := os.Mkdir(offlineDir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir, offlineDir
}

func inboxRequests(t *testing.T, offlineDir string) int {
	t.Helper()
	names, _ := filepath.Glob(filepath.Join(offlineDir, "contain", "*.req"))
	return len(names)
}

// With no serve holding the lifecycle lock, contain leaves its request for the next start and exits 4.
func TestContain_NoServeExits4(t *testing.T) {
	containChild()
	dir, offlineDir := privateStateDir(t)
	code, out := runContainChild(t, "TestContain_NoServeExits4", "--all", "--state-dir", dir)
	if code != 4 || !strings.Contains(out, "serve is not running") {
		t.Fatalf("exit %d, output %q; want 4 and the no-serve text", code, out)
	}
	if n := inboxRequests(t, offlineDir); n != 1 {
		t.Fatalf("requests left for the next start = %d, want 1", n)
	}
}

// With serve holding the lock and no receipt within --wait, contain exits 5 and leaves its request.
func TestContain_NoReceiptExits5(t *testing.T) {
	containChild()
	dir, offlineDir := privateStateDir(t)
	lock, err := os.OpenFile(filepath.Join(offlineDir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	code, out := runContainChild(t, "TestContain_NoReceiptExits5", "--run", uuid.NewString(), "--state-dir", dir, "--wait", "300ms")
	if code != 5 || !strings.Contains(out, "no receipt within") {
		t.Fatalf("exit %d, output %q; want 5 and the no-receipt text", code, out)
	}
	if n := inboxRequests(t, offlineDir); n != 1 {
		t.Fatalf("requests left = %d, want 1", n)
	}
}
