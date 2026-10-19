// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package api

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The volume probe must refuse a volume that takes one byte but not a spool-sized rewrite, or a
// failed rewrite is retried every probe pass and the lines behind the suspect land each time.
// The child process caps its own file size (RLIMIT_FSIZE) below the spool's: the probe is the
// only write after the cap, and the cap stays out of every other test in this package.
func TestAuditSpoolVolumeProbe_RefusesAVolumeTooSmallForTheRewrite(t *testing.T) {
	if os.Getenv("WARDYN_SPOOL_PROBE_CHILD") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestAuditSpoolVolumeProbe_RefusesAVolumeTooSmallForTheRewrite$", "-test.count=1")
		cmd.Env = append(os.Environ(), "WARDYN_SPOOL_PROBE_CHILD="+t.TempDir())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child: %v\n%s", err, out)
		}
		return
	}
	a, err := NewAuditSpool(filepath.Join(os.Getenv("WARDYN_SPOOL_PROBE_CHILD"), "spool.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.f.Close()
	if _, err := a.f.WriteString(strings.Repeat("x", 8191) + "\n"); err != nil {
		t.Fatal(err)
	}
	signal.Ignore(syscall.SIGXFSZ) // an over-cap write then returns EFBIG instead of killing the child
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &lim); err != nil {
		t.Fatal(err)
	}
	capped := lim
	capped.Cur = 1024
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &capped); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	ok := a.volumeWritable()
	a.mu.Unlock()
	// Lift the cap before the child exits: a -cover build writes its coverage files then.
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lim); err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("volumeWritable = true on a volume that cannot take the spool's rewrite, want false")
	}
}
