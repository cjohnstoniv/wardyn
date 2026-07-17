// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"runtime"
	"strings"
	"testing"
)

// DetectPlatform must report the compiled-in GOOS.
func TestDetectPlatform_OS(t *testing.T) {
	if got := DetectPlatform().OS; got != runtime.GOOS {
		t.Fatalf("DetectPlatform().OS = %q, want %q", got, runtime.GOOS)
	}
}

// isWSLProcVersion is a pure substring (case-insensitive) predicate: a stock
// Linux /proc/version is not WSL; anything naming Microsoft is.
func TestIsWSLProcVersion(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"stock_linux", "Linux version 6.1.0-generic (gcc 12) #1 SMP Debian", false},
		{"wsl_lower", "Linux version 5.15.0-microsoft-standard-WSL2 (oe-user@oe-host)", true},
		{"wsl_mixed_case", "Linux version 5.15.0-Microsoft-standard", true},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWSLProcVersion(tc.in); got != tc.want {
				t.Fatalf("isWSLProcVersion(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// DetectCLIProviders must return exactly the claude + codex entries. Installed
// tracks whatever LookPath reports on the test host — we do not assert a
// specific install state, only that the two tools are enumerated in order.
func TestDetectCLIProviders_EnumeratesClaudeAndCodex(t *testing.T) {
	provs := DetectCLIProviders()
	if len(provs) != 2 {
		t.Fatalf("DetectCLIProviders() returned %d entries, want 2", len(provs))
	}
	if provs[0].Tool != "claude" {
		t.Errorf("provs[0].Tool = %q, want claude", provs[0].Tool)
	}
	if provs[1].Tool != "codex" {
		t.Errorf("provs[1].Tool = %q, want codex", provs[1].Tool)
	}
	// LoggedIn implies a LoginVia path was recorded (advisory heuristic). BinPath is
	// set iff Installed — host-independent (we never assert whether a CLI is on
	// PATH here, only that the two fields stay consistent with each other).
	for _, p := range provs {
		if p.LoggedIn && p.LoginVia == "" {
			t.Errorf("%s LoggedIn=true but LoginVia empty", p.Tool)
		}
		if p.Installed && p.BinPath == "" {
			t.Errorf("%s Installed=true but BinPath empty", p.Tool)
		}
		if !p.Installed && p.BinPath != "" {
			t.Errorf("%s Installed=false but BinPath=%q (must be empty)", p.Tool, p.BinPath)
		}
	}
}

// vaultKVMDetail must give materially different, honest advice per posture. The
// regression this pins (U085): when KVM is absent but wardynd is containerized,
// the copy must NOT assert a bare "hardware limit no install can fix" and must
// point at bind-mounting /dev/kvm — the compose topology's real remedy.
func TestVaultKVMDetail(t *testing.T) {
	registered := vaultKVMDetail(true, false)
	if !strings.Contains(registered, "wardyn setup vault") {
		t.Errorf("kvm=true copy should point at `wardyn setup vault`, got %q", registered)
	}

	containerized := vaultKVMDetail(false, true)
	if !strings.Contains(containerized, "bind-mount /dev/kvm") {
		t.Errorf("containerized missing-KVM copy must name the bind-mount fix, got %q", containerized)
	}
	if strings.Contains(containerized, "no install can fix") {
		t.Errorf("containerized missing-KVM copy must not assert a hard hardware limit, got %q", containerized)
	}

	bareMetal := vaultKVMDetail(false, false)
	if !strings.Contains(bareMetal, "/dev/kvm") {
		t.Errorf("bare-metal missing-KVM copy should still name /dev/kvm, got %q", bareMetal)
	}
	if containerized == bareMetal || registered == containerized {
		t.Errorf("each posture must yield distinct copy")
	}
}

// containerizedCgroup is the pure /proc/1/cgroup predicate.
func TestContainerizedCgroup(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"docker_v1", "12:pids:/docker/abc123", true},
		{"containerd", "0::/system.slice/containerd.service/kubepods", true},
		{"libpod", "0::/machine.slice/libpod-xyz.scope", true},
		{"bare_v2", "0::/", false},
		{"host_systemd", "12:pids:/init.scope", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := containerizedCgroup(tc.in); got != tc.want {
				t.Fatalf("containerizedCgroup(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
