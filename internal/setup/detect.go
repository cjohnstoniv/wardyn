// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package setup provides host-environment detection for the first-run setup
// surface: resident coding-agent CLIs, and the OS/WSL posture the
// environment-step copy keys off. Leaf package (stdlib only).
package setup

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// CLIProvider is a coding-agent CLI on the wardynd host's PATH. Whether it is
// signed in is not read: a host login credentials no run (0.8.2 retired the
// host ~/.claude lane; a run's model access is its model provider's).
type CLIProvider struct {
	Tool      string
	Installed bool
	BinPath   string
}

// Platform is the wardynd host's OS, WSL/KVM/containerized posture. KVM
// separates "Vault incompatible" from "just needs setup"; Containerized
// changes what a missing /dev/kvm means (mount the device vs no hardware).
type Platform struct {
	OS            string
	WSL           bool
	KVM           bool
	Containerized bool
}

// DetectCLIProviders reports the coding-agent CLIs (claude, codex) on PATH.
func DetectCLIProviders() []CLIProvider {
	return []CLIProvider{detectProvider("claude"), detectProvider("codex")}
}

// detectProvider resolves one CLI's install state.
func detectProvider(tool string) CLIProvider {
	p := CLIProvider{Tool: tool}
	if path, err := exec.LookPath(tool); err == nil {
		p.Installed = true
		p.BinPath = path
	}
	return p
}

// DetectPlatform reports the host OS, WSL, /dev/kvm, and containerized status.
func DetectPlatform() Platform {
	return Platform{OS: runtime.GOOS, WSL: detectWSL(), KVM: detectKVM(), Containerized: detectContainerized()}
}

// VaultKVMDetail is the operator-facing Vault (Kata) availability explanation
// — a containerized host missing /dev/kvm gets "mount the device", not "no hardware".
func VaultKVMDetail() string {
	p := DetectPlatform()
	return vaultKVMDetail(p.KVM, p.Containerized)
}

// vaultKVMDetail is the pure (KVM, containerized) -> copy mapping, testable
// without real hardware.
func vaultKVMDetail(kvm, containerized bool) string {
	if kvm {
		return "no Vault (Kata microVM) runtime registered on this host yet — fixable, run `wardyn setup vault`. See Getting Started."
	}
	if containerized {
		return "Vault needs KVM virtualization and this containerized wardynd can't see /dev/kvm — bind-mount /dev/kvm into the wardynd service (compose), then Re-check. If the host itself has no KVM this stays unavailable. See Getting Started."
	}
	return "Vault needs KVM virtualization and this host doesn't expose /dev/kvm — on bare metal/host mode this is usually a hardware/hypervisor limit; if wardynd is containerized, bind-mount /dev/kvm into it. See Getting Started."
}

// detectWSL reports whether this host is WSL: linux AND /proc/version names
// "microsoft". Any read error (non-linux, or /proc absent) is not-WSL.
func detectWSL() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	b, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	return isWSLProcVersion(string(b))
}

// detectKVM reports whether the host exposes /dev/kvm. A CONTAINERIZED
// wardynd without /dev/kvm mounted reads false even on KVM hardware.
func detectKVM() bool {
	_, err := os.Stat("/dev/kvm")
	return err == nil
}

// detectContainerized reports whether wardynd runs in a container: marker
// files (Docker's /.dockerenv, Podman's /run/.containerenv), else the
// cgroup-1 hint in /proc/1/cgroup (false on cgroup v2's bare "0::/").
func detectContainerized() bool {
	for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	if b, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		return containerizedCgroup(string(b))
	}
	return false
}

// containerizedCgroup is the pure /proc/1/cgroup -> containerized predicate.
func containerizedCgroup(cgroup string) bool {
	for _, token := range []string{"docker", "containerd", "kubepods", "libpod"} {
		if strings.Contains(cgroup, token) {
			return true
		}
	}
	return false
}

// isWSLProcVersion is the pure /proc/version -> WSL predicate.
func isWSLProcVersion(procVersion string) bool {
	return strings.Contains(strings.ToLower(procVersion), "microsoft")
}

// SCMPosture is a presence-only snapshot of the host's git-credential habits,
// used to recommend a safer credential-ladder rung — never to import
// anything. Best-effort: a CONTAINERIZED wardynd can't see $HOME.
type SCMPosture struct {
	// GhCLI: a gh CLI login, i.e. a broad whole-account oauth session (ladder rung 4).
	GhCLI bool `json:"gh_cli"`
	// CredentialHelper is the git credential.helper name ("" if unset); "store"/"cache" mean plaintext-ish creds on disk.
	CredentialHelper string `json:"credential_helper"`
	// GitCredentialsFile: ~/.git-credentials exists (plaintext credentials).
	GitCredentialsFile bool `json:"git_credentials_file"`
	// Netrc: ~/.netrc (or .netrc.gpg) exists — legacy plaintext credentials.
	Netrc bool `json:"netrc"`
}

// DetectSCMPosture reports the host git-credential posture (presence only).
func DetectSCMPosture() SCMPosture {
	home, _ := os.UserHomeDir()
	var p SCMPosture
	if home == "" {
		return p
	}
	exists := func(rel ...string) bool {
		_, err := os.Stat(filepath.Join(append([]string{home}, rel...)...))
		return err == nil
	}
	p.GhCLI = exists(".config", "gh", "hosts.yml")
	p.GitCredentialsFile = exists(".git-credentials")
	p.Netrc = exists(".netrc") || exists(".netrc.gpg")
	if out, err := exec.Command("git", "config", "--global", "credential.helper").Output(); err == nil {
		p.CredentialHelper = strings.TrimSpace(string(out))
	}
	return p
}
