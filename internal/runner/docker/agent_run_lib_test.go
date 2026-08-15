// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// agentRunLibPath resolves deploy/images/common/agent-run-lib.sh relative to
// this package (go test's working directory is always the package dir).
const agentRunLibPath = "../../../deploy/images/common/agent-run-lib.sh"

// TestAgentRunLib_MakeToolchainDirsCreatesGOTMPDIR is the shell-level sibling
// of TestAttach_OpensInteractiveShellNotTrackedAsAgentExec's GOTMPDIR pin: the
// attach shell's inline guard (session.go's attachShell) and agent-run-lib.sh's
// make_toolchain_dirs are the SAME contract on two runtime paths — this run
// actually sources the real script and calls the real function (no daemon
// needed, so it stays cheap), proving it creates GOTMPDIR rather than merely
// pattern-matching the guard's source text.
func TestAgentRunLib_MakeToolchainDirsCreatesGOTMPDIR(t *testing.T) {
	libPath, err := filepath.Abs(agentRunLibPath)
	if err != nil {
		t.Fatalf("resolve agent-run-lib.sh path: %v", err)
	}
	if _, err := os.Stat(libPath); err != nil {
		t.Fatalf("agent-run-lib.sh not found at %s: %v", libPath, err)
	}
	target := filepath.Join(t.TempDir(), "gotmp")

	// "$0" absorbs bash -c's own script-name slot so $1/$2 are libPath/target,
	// never the -c string itself.
	out, err := exec.Command("bash", "-c",
		`set -euo pipefail; source "$1"; GOTMPDIR="$2" make_toolchain_dirs`,
		"agent-run-lib-test", libPath, target,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("make_toolchain_dirs failed: %v\noutput: %s", err, out)
	}
	if fi, statErr := os.Stat(target); statErr != nil || !fi.IsDir() {
		t.Fatalf("GOTMPDIR %q was not created by make_toolchain_dirs (stat err: %v)", target, statErr)
	}
}

// TestAgentRunLib_MakeToolchainDirsNoopWhenUnset asserts the guard's other
// half: an env without GOTMPDIR set (a run whose scans found no Go) does
// nothing — no error, no directory conjured from nothing.
func TestAgentRunLib_MakeToolchainDirsNoopWhenUnset(t *testing.T) {
	libPath, err := filepath.Abs(agentRunLibPath)
	if err != nil {
		t.Fatalf("resolve agent-run-lib.sh path: %v", err)
	}
	out, err := exec.Command("bash", "-c",
		`set -euo pipefail; unset GOTMPDIR; source "$1"; make_toolchain_dirs; echo ok`,
		"agent-run-lib-test", libPath,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("make_toolchain_dirs (unset GOTMPDIR) failed: %v\noutput: %s", err, out)
	}
	if got := string(out); got != "ok\n" {
		t.Errorf("output = %q, want just \"ok\\n\" (no side effects when GOTMPDIR is unset)", got)
	}
}

// TestAgentRunLib_MaybeExecTaskModePreservesPATH is the shell-level regression
// for W15-W15f-exec-lane-runtime-1: maybe_exec_task_mode used to `exec /bin/sh
// -lc "$1"` — the `-l` makes it a LOGIN shell, which sources /etc/profile and
// reassembles PATH from scratch, destroying every BYOI/devcontainer image's
// own Dockerfile ENV PATH toolchain before the task command ever runs. Plain
// `-c` (no login) must leave an inherited PATH byte-for-byte untouched.
func TestAgentRunLib_MaybeExecTaskModePreservesPATH(t *testing.T) {
	libPath, err := filepath.Abs(agentRunLibPath)
	if err != nil {
		t.Fatalf("resolve agent-run-lib.sh path: %v", err)
	}
	const wantPATH = "/wardyn-test-marker/bin:/wardyn-test-marker/sbin"

	cmd := exec.Command("bash", "-c",
		`set -euo pipefail; source "$1"; export PATH="$2" WARDYN_TASK_MODE=exec; maybe_exec_task_mode 'printf "%s" "$PATH"'`,
		"agent-run-lib-test", libPath, wantPATH,
	)
	out, err := cmd.Output() // stdout only; maybe_exec_task_mode's own banner goes to stderr
	if err != nil {
		t.Fatalf("maybe_exec_task_mode failed: %v", err)
	}
	if got := string(out); got != wantPATH {
		t.Errorf("PATH inside exec task mode = %q, want unchanged %q (a login shell (-l) is reassembling it via /etc/profile)", got, wantPATH)
	}
}

// TestAgentRunLib_SelftestReportRepoAndGitFailsClosedWithoutHelper is the
// regression for W15-W15f-exec-lane-runtime-2: selftest_report_repo_and_git
// used to be pure report-only, so a BYOI-wrapped image (which COPYs the
// wardyn-git-helper binary onto PATH but never wires `git config --system
// credential.helper` — only the prebuilt claude-code/codex-cli images bake
// that RUN line in) passed --selftest cleanly even though a granted run's
// git brokering would silently never fire. It must now fail closed (nonzero
// return) exactly when a git grant is present AND no helper is wired.
func TestAgentRunLib_SelftestReportRepoAndGitFailsClosedWithoutHelper(t *testing.T) {
	libPath, err := filepath.Abs(agentRunLibPath)
	if err != nil {
		t.Fatalf("resolve agent-run-lib.sh path: %v", err)
	}
	home := t.TempDir()

	run := func(t *testing.T, extraEnv []string) (out string, rc int) {
		t.Helper()
		cmd := exec.Command("bash", "-c",
			`set -uo pipefail; source "$1"; selftest_report_repo_and_git; echo "RC=$?"`,
			"agent-run-lib-test", libPath,
		)
		cmd.Env = append(os.Environ(), "HOME="+home)
		cmd.Env = append(cmd.Env, extraEnv...)
		b, cmdErr := cmd.CombinedOutput()
		if cmdErr != nil {
			t.Fatalf("selftest_report_repo_and_git harness failed: %v\noutput: %s", cmdErr, b)
		}
		out = string(b)
		if _, scanErr := fmt.Sscanf(out[strings.LastIndex(out, "RC=")+len("RC="):], "%d", &rc); scanErr != nil {
			t.Fatalf("could not parse RC= from output: %q", out)
		}
		return out, rc
	}

	t.Run("no grant, no helper wired -> report-only, rc=0", func(t *testing.T) {
		_, rc := run(t, []string{"GIT_CONFIG_NOSYSTEM=1", "WARDYN_GITHUB_GRANT_ID=", "WARDYN_GIT_PAT_GRANTS="})
		if rc != 0 {
			t.Errorf("rc = %d, want 0 (no grant means an unwired helper is not a failure)", rc)
		}
	})

	t.Run("grant present, no helper wired -> fails closed, rc=1", func(t *testing.T) {
		out, rc := run(t, []string{"GIT_CONFIG_NOSYSTEM=1", "WARDYN_GITHUB_GRANT_ID=gh-grant-1", "WARDYN_GIT_PAT_GRANTS="})
		if rc != 1 {
			t.Errorf("rc = %d, want 1 (grant present + no credential helper must fail closed)\noutput: %s", rc, out)
		}
	})

	t.Run("grant present, helper wired -> rc=0", func(t *testing.T) {
		sysConfig := filepath.Join(t.TempDir(), "gitconfig")
		if err := os.WriteFile(sysConfig, []byte("[credential]\n\thelper = /bin/true\n"), 0o644); err != nil {
			t.Fatalf("write fake system gitconfig: %v", err)
		}
		out, rc := run(t, []string{"GIT_CONFIG_SYSTEM=" + sysConfig, "WARDYN_GITHUB_GRANT_ID=gh-grant-1", "WARDYN_GIT_PAT_GRANTS="})
		if rc != 0 {
			t.Errorf("rc = %d, want 0 (helper wired must pass)\noutput: %s", rc, out)
		}
	})
}
