// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// gen writes a base generation: disk, aux, machine id and a ready manifest
// whose AgentVersion names the generation.
func gen(t *testing.T, dir, g string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for n, c := range map[string]string{DiskFile: g + "-disk", AuxFile: g + "-aux", MachineIDFile: g + "-id"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeJSONAtomic(filepath.Join(dir, ManifestFile), Manifest{Format: BaseFormat, State: BaseReady, AgentVersion: g}, 0o600); err != nil {
		t.Fatal(err)
	}
}

// twoGens is a base at gen1 and an updated copy at gen2 beside it.
func twoGens(t *testing.T) (base, nb string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "bases")
	base, nb = filepath.Join(root, "0"), filepath.Join(root, "0.update")
	gen(t, base, "gen1")
	gen(t, nb, "gen2")
	return base, nb
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// blocked reports whether ch stays empty for d.
func blocked[T any](ch chan T, d time.Duration) bool {
	select {
	case <-ch:
		return false
	case <-time.After(d):
		return true
	}
}

// A clone that races the exchange waits for it and sees one generation: the
// disk, the aux storage and the machine identifier all from the new base.
func TestCloneRacingTheExchangeSeesOneGeneration(t *testing.T) {
	base, nb := twoGens(t)
	runDir := filepath.Join(t.TempDir(), "run")
	exchange, err := lockFile(context.Background(), baseLockPath(base), true)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := CloneRun(nil, base, runDir, IdentityTravel); done <- err }()
	if !blocked(done, 150*time.Millisecond) {
		t.Fatal("a clone read the base while the exchange held its lock")
	}
	if err := swapDirs(nb, base); err != nil {
		t.Fatal(err)
	}
	exchange()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for n, want := range map[string]string{DiskFile: "gen2-disk", AuxFile: "gen2-aux", MachineIDFile: "gen2-id"} {
		if got := readFile(t, filepath.Join(runDir, n)); got != want {
			t.Errorf("%s = %q, want %q", n, got, want)
		}
	}
}

// The exchange waits for a clone that is reading the base.
func TestExchangeWaitsForAClone(t *testing.T) {
	base, nb := twoGens(t)
	reading, err := lockFile(context.Background(), baseLockPath(base), false)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- swapBase(context.Background(), nb, base) }()
	if !blocked(done, 150*time.Millisecond) {
		t.Fatal("the base was swapped under a clone that was reading it")
	}
	if got := readFile(t, filepath.Join(base, DiskFile)); got != "gen1-disk" {
		t.Fatalf("base = %q while a clone held it", got)
	}
	reading()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(base, DiskFile)); got != "gen2-disk" {
		t.Fatalf("base after the exchange = %q", got)
	}
	if got := readFile(t, filepath.Join(nb, DiskFile)); got != "gen1-disk" {
		t.Fatalf("the old generation is at %q", got)
	}
}

// The manifest a run starts from and its clone come from the same generation,
// even when the exchange lands between the clone starting and getting the lock.
func TestManifestAndCloneShareAGeneration(t *testing.T) {
	base, nb := twoGens(t)
	runDir := filepath.Join(t.TempDir(), "run")
	exchange, err := lockFile(context.Background(), baseLockPath(base), true)
	if err != nil {
		t.Fatal(err)
	}
	type res struct {
		m   Manifest
		err error
	}
	done := make(chan res, 1)
	go func() {
		m, _, err := cloneReadyBase(nil, base, runDir, IdentityTravel)
		done <- res{m, err}
	}()
	if !blocked(done, 150*time.Millisecond) {
		t.Fatal("cloneReadyBase did not wait for the exchange")
	}
	if err := swapDirs(nb, base); err != nil {
		t.Fatal(err)
	}
	exchange()
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if got := readFile(t, filepath.Join(runDir, DiskFile)); r.m.AgentVersion != "gen2" || got != "gen2-disk" {
		t.Fatalf("manifest %q with a clone of %q", r.m.AgentVersion, got)
	}
}

// An exchange the volume cannot do is refused with the fix, and the base is
// left as it was: nothing falls back to two renames.
func TestUnsupportedExchangeIsRefused(t *testing.T) {
	base, nb := twoGens(t)
	for _, errno := range []error{unix.EINVAL, unix.ENOTSUP} {
		old := swapFn
		swapFn = func(string, string) error { return errno }
		err := swapBase(context.Background(), nb, base)
		swapFn = old
		if err == nil || !strings.Contains(err.Error(), "APFS") {
			t.Fatalf("%v: err = %v", errno, err)
		}
		if got := readFile(t, filepath.Join(base, DiskFile)); got != "gen1-disk" {
			t.Fatalf("%v: base = %q", errno, got)
		}
		if got := readFile(t, filepath.Join(nb, DiskFile)); got != "gen2-disk" {
			t.Fatalf("%v: staging = %q", errno, got)
		}
	}
}
