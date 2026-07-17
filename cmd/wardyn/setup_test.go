// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/setup"
)

func TestParseOSFamily(t *testing.T) {
	cases := []struct{ id, idLike, want string }{
		{"ubuntu", "debian", "debian"},
		{"debian", "", "debian"},
		{"fedora", "", "fedora"},
		{"rocky", "rhel centos fedora", "fedora"},
		{"arch", "", "arch"},
		{"opensuse-leap", "suse opensuse", "suse"},
		{"alpine", "", "other"},
		{"", "", "other"},
	}
	for _, c := range cases {
		if got := parseOSFamily(c.id, c.idLike); got != c.want {
			t.Errorf("parseOSFamily(%q,%q)=%q want %q", c.id, c.idLike, got, c.want)
		}
	}
}

func TestRestartDocker(t *testing.T) {
	if s := restartDocker(dockerEnv{initSys: "systemd"}); !strings.Contains(s, "systemctl") {
		t.Errorf("systemd restart = %q", s)
	}
	if s := restartDocker(dockerEnv{initSys: "openrc"}); !strings.Contains(s, "rc-service") {
		t.Errorf("openrc restart = %q", s)
	}
	if s := restartDocker(dockerEnv{initSys: "sysv"}); !strings.Contains(s, "service docker restart") {
		t.Errorf("sysv restart = %q", s)
	}
}

// linuxNative is a fully-detected native rootful Linux host.
func linuxNative() dockerEnv {
	return dockerEnv{goos: "linux", hasDocker: true, osType: "linux", initSys: "systemd", family: "debian"}
}

func TestPlanWall(t *testing.T) {
	cases := []struct {
		name       string
		env        dockerEnv
		wantAction action
		wantIn     string // substring expected somewhere in title+why+script
	}{
		{"debian-systemd-auto", linuxNative(), actAuto, "apt"},
		{"fedora-print", func() dockerEnv { e := linuxNative(); e.family = "fedora"; return e }(), actPrint, "storage.googleapis.com"},
		{"arch-print", func() dockerEnv { e := linuxNative(); e.family = "arch"; return e }(), actPrint, "runsc install"},
		{"non-systemd-print", func() dockerEnv { e := linuxNative(); e.family = "other"; e.initSys = "openrc"; return e }(), actPrint, "runsc"},
		{"docker-desktop", func() dockerEnv { e := linuxNative(); e.desktop = true; return e }(), actUnsupported, "native Docker"},
		{"docker-desktop-wsl", func() dockerEnv { e := linuxNative(); e.desktop = true; e.wsl = true; return e }(), actUnsupported, "WSL2"},
		{"rootless", func() dockerEnv { e := linuxNative(); e.rootless = true; return e }(), actUnsupported, "rootless"},
		{"windows-containers", func() dockerEnv { e := linuxNative(); e.osType = "windows"; return e }(), actUnsupported, "Windows containers"},
		{"no-docker", dockerEnv{goos: "linux"}, actUnsupported, "Docker isn't installed"},
		{"windows-host", dockerEnv{goos: "windows"}, actUnsupported, "WSL2"},
		{"macos", dockerEnv{goos: "darwin"}, actPrint, "Colima"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := planWall(c.env)
			if p.action != c.wantAction {
				t.Fatalf("action=%d want %d (%+v)", p.action, c.wantAction, p)
			}
			hay := p.title + " " + p.why + " " + p.script
			if !strings.Contains(hay, c.wantIn) {
				t.Errorf("expected %q in plan, got title=%q why=%q", c.wantIn, p.title, p.why)
			}
		})
	}
}

func TestPlanVault(t *testing.T) {
	kvm := func() dockerEnv { e := linuxNative(); e.kvm = true; return e }
	cases := []struct {
		name       string
		env        dockerEnv
		wantAction action
	}{
		{"kvm-native-print", kvm(), actPrint},
		{"no-kvm-unsupported", linuxNative(), actUnsupported},
		{"desktop-unsupported", func() dockerEnv { e := kvm(); e.desktop = true; return e }(), actUnsupported},
		{"rootless-unsupported", func() dockerEnv { e := kvm(); e.rootless = true; return e }(), actUnsupported},
		{"macos-unsupported", dockerEnv{goos: "darwin"}, actUnsupported},
		{"windows-unsupported", dockerEnv{goos: "windows"}, actUnsupported},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if p := planVault(c.env); p.action != c.wantAction {
				t.Fatalf("action=%d want %d (%+v)", p.action, c.wantAction, p)
			}
		})
	}
}

// gVisor's release bucket is keyed by `uname -m` (x86_64/aarch64), not
// amd64/arm64. gvisorBinaryScript gets this right; colimaWallScript used to
// remap uname -m to amd64/arm64, which 404s on both Intel and Apple-Silicon
// Colima VMs and silently leaves the host without Wall. Assert the two
// scripts use the identical arch expression and that the remap is gone.
func TestColimaWallScript_MatchesGvisorBinaryArchToken(t *testing.T) {
	colima := colimaWallScript()
	binary := gvisorBinaryScript(dockerEnv{})
	if strings.Contains(colima, "arm64") || strings.Contains(colima, "amd64") {
		t.Errorf("colimaWallScript must not remap uname -m to amd64/arm64 (gVisor's bucket uses x86_64/aarch64): %s", colima)
	}
	if !strings.Contains(colima, "A=$(uname -m)") {
		t.Errorf("colimaWallScript must use uname -m directly, like gvisorBinaryScript: %s", colima)
	}
	if !strings.Contains(binary, `ARCH="$(uname -m)"`) {
		t.Fatalf("sibling gvisorBinaryScript's arch expression changed — update this test to match: %s", binary)
	}
}

// kataScript must never blindly install "latest": the version floor
// (CVE-2026-44210/-47243) must be visible in both the plan text and the
// install script, and the plan for a genuinely installable host must offer to
// run it (actPrint, not actAuto — Kata is host-sensitive, never auto-installed).
func TestPlanVault_KataVersionFloor(t *testing.T) {
	env := func() dockerEnv { e := linuxNative(); e.kvm = true; return e }()
	p := planVault(env)
	if p.action != actPrint {
		t.Fatalf("action=%d, want actPrint", p.action)
	}
	if !strings.Contains(p.why, kataMinVersion) {
		t.Errorf("plan why-text must mention the floor %q: %q", kataMinVersion, p.why)
	}
	if !strings.Contains(p.script, kataMinVersion) {
		t.Errorf("kataScript must embed the floor %q: %s", kataMinVersion, p.script)
	}
	if !strings.Contains(p.script, "sort -V") {
		t.Errorf("kataScript must enforce the floor with a version compare, got: %s", p.script)
	}
}

// TestKataScript_FloorEnforcement extracts kataScript's version-floor snippet
// (the KATA_MIN_VERSION/ver_num compare, ahead of the network-touching
// download lines) and runs it under bash — with the candidate version supplied
// via the SAME WARDYN_KATA_VERSION env var an operator would set (no script
// text is built from test input) — for a version matrix proving the floor is
// inclusive (>=), rejects an older release, tolerates a "v"-prefixed tag, and
// accepts a newer one: the exact CVE-2026-44210/-47243 regression this floor
// exists to close.
func TestKataScript_FloorEnforcement(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}
	full := kataScript()
	snippet := strings.SplitN(full, "# Kata's release assets", 2)[0]
	if !strings.Contains(snippet, "KATA_MIN_VERSION") {
		t.Fatalf("could not extract the version-floor snippet from kataScript: %s", full)
	}

	tests := []struct {
		ver     string
		wantErr bool
	}{
		{"3.30.0", true},   // below floor: reject
		{"3.31.0", false},  // exactly the floor: accept (inclusive)
		{"v3.31.0", false}, // "v"-prefixed floor tag: accept
		{"3.31.1", false},  // above floor: accept
		{"4.0.0", false},   // above floor: accept
	}
	for _, tt := range tests {
		t.Run(tt.ver, func(t *testing.T) {
			cmd := exec.Command("bash", "-c", snippet)
			cmd.Env = append(os.Environ(), "WARDYN_KATA_VERSION="+tt.ver)
			err := cmd.Run()
			if tt.wantErr && err == nil {
				t.Errorf("version %q: want the floor check to fail (exit 1), got success", tt.ver)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("version %q: want the floor check to pass, got error: %v", tt.ver, err)
			}
		})
	}
}

// The bug this whole change fixes: a Docker Desktop host must never produce a
// plan that EXECUTES a daemon restart. actUnsupported plans are never run, so
// even though their guidance text mentions systemctl (for the *native* engine),
// executePlan won't touch the Desktop daemon.
func TestDockerDesktopNeverExecutes(t *testing.T) {
	for _, e := range []dockerEnv{
		{goos: "linux", hasDocker: true, osType: "linux", desktop: true, initSys: "systemd", family: "debian"},
		{goos: "linux", hasDocker: true, osType: "linux", desktop: true, wsl: true, initSys: "systemd", family: "debian"},
	} {
		if p := planWall(e); p.action != actUnsupported {
			t.Errorf("docker desktop wall action=%d, must be actUnsupported so it never restarts the engine", p.action)
		}
	}
}

// U111 pin: detectDocker must source its WSL/KVM facts from the shared
// internal/setup.DetectPlatform() leaf detector, not a private reimplementation.
// A consistency check is the honest counterfactual — if the CLI drifts back to
// its own os.Stat("/dev/kvm") / /proc/version scan, any divergence from the
// shared detector (which hardens the WSL check with a GOOS guard, etc.) fails
// here.
func TestDetectDockerUsesSharedPlatformDetector(t *testing.T) {
	e := detectDocker()
	p := setup.DetectPlatform()
	if e.wsl != p.WSL {
		t.Errorf("detectDocker().wsl=%v diverged from setup.DetectPlatform().WSL=%v — reuse the shared detector", e.wsl, p.WSL)
	}
	if e.kvm != p.KVM {
		t.Errorf("detectDocker().kvm=%v diverged from setup.DetectPlatform().KVM=%v — reuse the shared detector", e.kvm, p.KVM)
	}
}
