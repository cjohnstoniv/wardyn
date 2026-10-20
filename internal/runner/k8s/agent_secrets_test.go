// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// The watcher deletes a named file once it is read and leaves an unread one
// and an unnamed one, running the command it wraps. Driven on the host's own
// file system, which must update access times on read (relatime) as the pod's
// tmpfs does.
func TestWipeAfterReadScript(t *testing.T) {
	d := t.TempDir()
	for _, f := range []string{"probe", "read", "unread", "kept"} {
		if err := os.WriteFile(filepath.Join(d, f), []byte("v"), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(20 * time.Millisecond)
	before, _ := os.Stat(filepath.Join(d, "probe"))
	_, _ = os.ReadFile(filepath.Join(d, "probe"))
	if out, err := exec.Command("stat", "-c", "%x|%y", filepath.Join(d, "probe")).Output(); err != nil || string(out[:len(out)/2]) == string(out[len(out)/2+1:]) {
		t.Skipf("this file system does not move access times on read (%s, %v); mtime %v", out, err, before.ModTime())
	}
	cmd := exec.Command("sh", "-c", wipeAfterReadScript, "wardyn-agent", d, "2", "read", "unread", "sh", "-c", `echo started > "$0/started"; sleep 5`, d) //nolint:gosec // the script under test
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(4 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(d, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the wrapped command never ran")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.ReadFile(filepath.Join(d, "read")); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(d, "read")); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the read file was not deleted")
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, f := range []string{"unread", "kept"} {
		if _, err := os.Stat(filepath.Join(d, f)); err != nil {
			t.Errorf("%s: %v, want it kept", f, err)
		}
	}
}
