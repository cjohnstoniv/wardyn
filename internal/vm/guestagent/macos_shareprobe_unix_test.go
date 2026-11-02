// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package guestagent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestShareProbe(t *testing.T) {
	dir := t.TempDir()
	if err := (ShareProbe{}).Read(context.Background(), dir); err != nil {
		t.Fatalf("a readable folder: %v", err)
	}
	failing := ShareProbe{argv: []string{"/bin/sh", "-c", "echo denied >&2; exit 1", "sh"}}
	if err := failing.Read(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "denied") || !strings.Contains(err.Error(), "Full Disk Access") {
		t.Fatalf("a failed listing: err = %v", err)
	}
	// A read with no answer: the shell writes its pid, which Setsid made its group's id, and leaves a child
	// behind, so only a kill of the whole group ends both.
	hang := ShareProbe{argv: []string{"/bin/sh", "-c", `echo $$ >"$1/pid"; sleep 60 & wait`, "sh"}, bound: 500 * time.Millisecond}
	start := time.Now()
	err := hang.Read(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "no answer") || !strings.Contains(err.Error(), "Full Disk Access") {
		t.Fatalf("a read with no answer: err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the probe took %s on a 500ms bound", d)
	}
	b, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	pgid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); unix.Kill(-pgid, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the probe's process group outlived its bound")
		}
	}
}
