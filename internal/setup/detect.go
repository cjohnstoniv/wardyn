// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package setup provides host-environment detection for the first-run setup
// surface (GET /api/v1/setup/status): which resident coding-agent CLIs are
// present, and the OS/WSL posture the environment-step copy keys off. It is a
// leaf package (stdlib only) so the API handler and tests can depend on it
// without pulling in the rest of the control plane.
package setup

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// CLIProvider is a resident coding-agent CLI detected on the wardynd host.
// LoggedIn is a HEURISTIC (a home-dir credential-file check), not a live probe.
// BinPath is the resolved PATH location when Installed (empty otherwise) — the
// setup surface uses it to warn "logged in but the CLI is off PATH".
type CLIProvider struct {
	Tool      string
	Installed bool
	BinPath   string
	LoggedIn  bool
	LoginVia  string
}

// Platform is the wardynd host's OS, whether it is running under WSL, and
// whether it exposes KVM virtualization (/dev/kvm) — the hardware fact that
// separates "Vault is incompatible here" from "Vault just needs setup".
type Platform struct {
	OS  string
	WSL bool
	KVM bool
}

// DetectCLIProviders reports the resident coding-agent CLIs (claude, codex):
// whether each is on PATH (Installed) and an advisory login signal (LoggedIn +
// the LoginVia path that produced it).
//
// ponytail: LoggedIn is advisory — a stale/expired session whose credential
// file still exists reads as logged-in. The honest upgrade is shelling out to
// `claude whoami` (or the codex equivalent) and parsing it; the first-run check
// deliberately avoids the subprocess.
func DetectCLIProviders() []CLIProvider {
	home, _ := os.UserHomeDir()
	claude := detectProvider("claude", home, []string{filepath.Join(".claude", ".credentials.json")})
	// macOS: Claude Code stores the OAuth credential in the login Keychain (service
	// "Claude Code-credentials"), so ~/.claude/.credentials.json usually does NOT
	// exist — the file check above false-negatives a logged-in Mac and the UI wrongly
	// reads "not logged in". Fall back to a Keychain presence probe. Note: host-mode
	// subscription staging still needs the on-disk file, so this only fixes the login
	// signal, not the composed-run mount (see stage-claude-creds.sh).
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

// detectMacKeychainClaude reports the Keychain-backed Claude login on macOS as a
// LoginVia string, or "" if absent/not-macOS. It queries only the item's presence
// (`find-generic-password` WITHOUT -w), so the secret is never read into this
// process and no "allow access" ACL prompt is triggered — this is a metadata probe,
// not a credential read.
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

// DetectPlatform reports the host OS (runtime.GOOS), whether it is WSL, and
// whether /dev/kvm is exposed.
func DetectPlatform() Platform {
	return Platform{OS: runtime.GOOS, WSL: detectWSL(), KVM: detectKVM()}
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

// detectKVM reports whether the host exposes /dev/kvm. Accurate in host mode;
// a CONTAINERIZED wardynd without /dev/kvm mounted reads false even on KVM
// hardware — a false negative that only softens "incompatible" copy for a
// tier the runner already reports honestly when it is actually live.
func detectKVM() bool {
	_, err := os.Stat("/dev/kvm")
	return err == nil
}

// isWSLProcVersion is the pure /proc/version -> WSL predicate (kept separate so
// it is testable without a real /proc/version).
func isWSLProcVersion(procVersion string) bool {
	return strings.Contains(strings.ToLower(procVersion), "microsoft")
}
