// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateMount_Allowed(t *testing.T) {
	allowed := []Mount{
		{Source: "/home/maintainer/repo", Target: "/home/agent/work"},
		{Source: "/srv/data", Target: "/work"},
		{Source: "/srv/data", Target: "/work/data"},
		{Source: "/opt/x", Target: "/workspace/x", ReadOnly: true},
		{Source: "/home/u/docker-stuff", Target: "/work/x"}, // not docker.sock
	}
	for _, m := range allowed {
		if err := ValidateMount(m); err != nil {
			t.Errorf("ValidateMount(%+v) = %v, want nil (allowed)", m, err)
		}
	}
}

func TestValidateMount_Denied(t *testing.T) {
	denied := []Mount{
		{Source: "/", Target: "/home/agent/x"},
		{Source: "/proc", Target: "/home/agent/x"},
		{Source: "/proc/sys/kernel", Target: "/work/x"},
		{Source: "/sys", Target: "/work/x"},
		{Source: "/dev", Target: "/work/x"},
		{Source: "/dev/sda", Target: "/work/x"},
		{Source: "/run", Target: "/work/x"},
		{Source: "/var/run", Target: "/work/x"},
		{Source: "/var/run/docker.sock", Target: "/work/x"},
		{Source: "/var/lib/docker", Target: "/work/x"},
		{Source: "/var/lib/docker/volumes", Target: "/work/x"},
		{Source: "/etc", Target: "/work/x"},
		{Source: "/etc/passwd", Target: "/work/x"},
		{Source: "/boot", Target: "/work/x"},
		{Source: "/root", Target: "/work/x"},
		{Source: "/root/.ssh/id_rsa", Target: "/work/x"},
		{Source: "/run/docker.sock", Target: "/work/x"},
		{Source: "/anywhere/docker.sock", Target: "/work/x"},
		// non-absolute / non-cleaned sources
		{Source: "relative", Target: "/work/x"},
		{Source: "/home/../etc", Target: "/work/x"},
		{Source: "/home/u/repo/", Target: "/work/x"},
		{Source: "//double", Target: "/work/x"},
		{Source: "", Target: "/work/x"},
		// bad targets
		{Source: "/home/u/repo", Target: "/etc"},
		{Source: "/home/u/repo", Target: "/usr"},
		{Source: "/home/u/repo", Target: "/home/agentX"}, // not /home/agent boundary
		{Source: "/home/u/repo", Target: "/workspaceX"},  // not /workspace boundary
		{Source: "/home/u/repo", Target: "relative"},
		{Source: "/home/u/repo", Target: ""},
	}
	for _, m := range denied {
		if err := ValidateMount(m); err == nil {
			t.Errorf("ValidateMount(%+v) = nil, want error (denied)", m)
		}
	}
}

// ValidateMount must not be fooled by a lexically-clean source that is (or
// traverses) a symlink into a denied path — the daemon resolves symlinks
// source-side, so we resolve too and re-run the deny-list on the real path.
func TestValidateMount_Symlink(t *testing.T) {
	dir := t.TempDir()

	// A symlink whose target is a denied path (/etc) is rejected, even though
	// the link path itself is lexically clean and under no denied prefix.
	link := filepath.Join(dir, "sneaky")
	if err := os.Symlink("/etc", link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := ValidateMount(Mount{Source: link, Target: "/work/x"}); err == nil {
		t.Errorf("ValidateMount(symlink -> /etc) = nil, want error (resolves into denied path)")
	}

	// A normal directory under an allowed prefix still passes (resolves to
	// itself, no denied prefix).
	real := filepath.Join(dir, "repo")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := ValidateMount(Mount{Source: real, Target: "/work/repo"}); err != nil {
		t.Errorf("ValidateMount(%q) = %v, want nil (real dir, allowed)", real, err)
	}

	// A non-existent source still passes lexical-only: wardynd may talk to a
	// remote/VM dockerd where the path exists only daemon-side, so EvalSymlinks'
	// IsNotExist must NOT fail closed.
	missing := filepath.Join(dir, "does-not-exist")
	if err := ValidateMount(Mount{Source: missing, Target: "/work/x"}); err != nil {
		t.Errorf("ValidateMount(%q) = %v, want nil (non-existent source, lexical-only)", missing, err)
	}
}
