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

// CLIProvider is a resident coding-agent CLI. LoggedIn is a HEURISTIC
// (credential-file check), not a live probe; BinPath (set when Installed)
// lets the setup surface warn "logged in but the CLI is off PATH".
type CLIProvider struct {
	Tool      string
	Installed bool
	BinPath   string
	LoggedIn  bool
	LoginVia  string
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

// DetectCLIProviders reports the resident coding-agent CLIs (claude, codex):
// Installed (on PATH) and an advisory LoggedIn+LoginVia signal — heuristic,
// since a stale credential file still reads as logged-in (no shelling out).
func DetectCLIProviders() []CLIProvider {
	home, _ := os.UserHomeDir()
	claude := detectProvider("claude", home, []string{filepath.Join(".claude", ".credentials.json")})
	// macOS stores the OAuth credential in Keychain, not on disk — fall back
	// to a Keychain probe (fixes only the login signal, not host-mode staging).
	if !claude.LoggedIn {
		if via := detectMacKeychainClaude(); via != "" {
			claude.LoggedIn = true
			claude.LoginVia = via
		}
	}
	return []CLIProvider{
		claude,
		detectProvider("codex", home, []string{filepath.Join(".codex", "auth.json")}),
	}
}

// detectMacKeychainClaude reports the Keychain-backed Claude login on macOS,
// or "" if absent/not-macOS — presence-only probe (no -w), no ACL prompt.
func detectMacKeychainClaude() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	// -s <service>: match the service attribute; no -w so only metadata is touched.
	if err := exec.Command("security", "find-generic-password", "-s", "Claude Code-credentials").Run(); err == nil {
		return "macOS Keychain (Claude Code-credentials)"
	}
	return ""
}

// detectProvider resolves one CLI's install + login heuristic. loginPaths are
// checked relative to home; the first that exists wins and is recorded verbatim.
func detectProvider(tool, home string, loginPaths []string) CLIProvider {
	p := CLIProvider{Tool: tool}
	if path, err := exec.LookPath(tool); err == nil {
		p.Installed = true
		p.BinPath = path
	}
	if home != "" {
		for _, rel := range loginPaths {
			candidate := filepath.Join(home, rel)
			if _, err := os.Stat(candidate); err == nil {
				p.LoggedIn = true
				p.LoginVia = candidate
				break
			}
		}
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
