// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"os"
	"path/filepath"
	"strings"
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

// TestValidateMountSource_WidenedRuntimeSocketsAndContainerdState pins the half
// of the 0.7.2 deny-list widening that nothing pinned (test-2/C3).
//
// 0.7.2 widened deniedSource from `case "docker.sock"` to the four
// container-runtime socket basenames, and added /var/lib/containerd to
// deniedSourcePrefixes. Reverting both stayed green: the one test naming the
// change asserted `strings.Contains(err, "denied user drive")` — a sentence the
// user-drive wrapper produces for EVERY refusal, including the "could not be
// resolved on this host" one a non-existent path earns anyway. So the test
// passed with the rule gone, for a reason unrelated to the rule.
//
// Each row therefore asserts a fragment naming ITS OWN path, and each path is
// chosen to be refused by the WIDENED rule alone: none of them is under a
// pre-0.7.2 denied prefix, so with the widening reverted every row here returns
// nil and reds.
func TestValidateMountSource_WidenedRuntimeSocketsAndContainerdState(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want string
	}{
		{"containerd socket outside /run", "/opt/sockets/containerd.sock",
			`mount source "/opt/sockets/containerd.sock" references a container-runtime socket`},
		{"podman socket outside /run", "/opt/sockets/podman.sock",
			`mount source "/opt/sockets/podman.sock" references a container-runtime socket`},
		{"cri-o socket outside /run", "/opt/sockets/crio.sock",
			`mount source "/opt/sockets/crio.sock" references a container-runtime socket`},
		{"containerd state root", "/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs",
			`is under denied host path "/var/lib/containerd"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMountSource(tc.src)
			if err == nil {
				t.Fatalf("ValidateMountSource(%q) = nil, want the widened refusal", tc.src)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ValidateMountSource(%q) = %q, want a message containing %q — a refusal that "+
					"cannot be told apart from the neighbouring rules' cannot pin this one", tc.src, err, tc.want)
			}
		})
	}

	// The control: docker.sock was the only basename before the widening, and it
	// still refuses with the same sentence — so the rows above fail because the
	// WIDENING is gone, never because the socket rule as a whole is.
	if err := ValidateMountSource("/opt/sockets/docker.sock"); err == nil ||
		!strings.Contains(err.Error(), "references a container-runtime socket") {
		t.Errorf("premise changed: docker.sock is no longer refused by the socket rule (%v)", err)
	}
}
