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

// TestAgentRunLib_MaybeExecTaskModePreservesPATH: maybe_exec_task_mode must
// not `exec /bin/sh -lc "$1"` — the `-l` makes it a login shell, which sources
// /etc/profile and reassembles PATH from scratch, destroying every
// BYOI/devcontainer image's own Dockerfile ENV PATH toolchain before the task
// command ever runs. Plain `-c` (no login) must leave an inherited PATH
// byte-for-byte untouched.
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

// TestAgentRunLib_SelftestReportRepoAndGitFailsClosedWithoutHelper:
// selftest_report_repo_and_git must not be report-only — a BYOI-wrapped image
// that COPYs the wardyn-git-helper binary onto PATH without wiring `git
// config --system credential.helper` (only the prebuilt claude-code/codex-cli
// images bake that RUN line in) would pass --selftest cleanly even though a
// granted run's git brokering would silently never fire. It fails closed
// (nonzero return) exactly when a git grant is present and no helper is
// wired.
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

// cloneOne runs agent-run-lib.sh's clone_one under the lib's set -euo pipefail and reports
// whether the shell got past it: a clone failure is a governance signal, never fatal.
func cloneOne(t *testing.T, url, dest, ref string) string {
	t.Helper()
	libPath, err := filepath.Abs(agentRunLibPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", `set -euo pipefail; source "$1"; clone_one "$2" "$3" "" "$4"; echo CONTINUED`,
		"agent-run-lib-test", libPath, url, dest, ref)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "WARDYN_RUN_ID=") // no run id: name_run_branch leaves git config alone
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "CONTINUED") {
		t.Fatalf("clone_one did not continue: %v\n%s", err, out)
	}
	return string(out)
}

// A clone target that already holds a folder of the person's own files (a writable local_dir mount
// over the repo's default target) is never deleted or cloned over, whether or not a ref is pinned.
func TestAgentRunLib_CloneNeverDeletesAPreexistingTarget(t *testing.T) {
	for _, ref := range []string{"", "main"} {
		dest := filepath.Join(t.TempDir(), "widget")
		if err := os.MkdirAll(dest, 0o755); err != nil {
			t.Fatal(err)
		}
		mine := filepath.Join(dest, "notes.txt")
		if err := os.WriteFile(mine, []byte("mine"), 0o600); err != nil {
			t.Fatal(err)
		}
		out := cloneOne(t, "file:///nonexistent-repo", dest, ref)
		if b, err := os.ReadFile(mine); err != nil || string(b) != "mine" {
			t.Errorf("ref %q: the person's file was lost (%v):\n%s", ref, err, out)
		}
		if !strings.Contains(out, "clone SKIPPED for "+dest) {
			t.Errorf("ref %q: no skip line:\n%s", ref, out)
		}
	}
}

// A target that cannot be created (under a read-only mount) skips the clone instead of killing the run.
func TestAgentRunLib_CloneIntoAnUncreatableTargetContinues(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only directory mode this test relies on")
	}
	ro := t.TempDir()
	if err := os.Chmod(ro, 0o555); err != nil { //nolint:gosec // test fixture: a read-only parent
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) }) //nolint:gosec // restore so TempDir cleanup can remove it
	out := cloneOne(t, "file:///nonexistent-repo", filepath.Join(ro, "widget"), "main")
	if !strings.Contains(out, "cannot be created") {
		t.Errorf("no skip line for the uncreatable target:\n%s", out)
	}
}

// A sha ref still reaches the fetch fallback after the direct --branch attempt fails, and checks the
// commit out into a target that did not exist.
func TestAgentRunLib_CloneByShaUsesTheFetchFallback(t *testing.T) {
	src := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", src, "-c", "user.name=t", "-c", "user.email=t@example.test"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("config", "uploadpack.allowAnySHA1InWant", "true")
	if err := os.WriteFile(filepath.Join(src, "README"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "README")
	git("commit", "-q", "-m", "one")
	sha := git("rev-parse", "HEAD")
	dest := filepath.Join(t.TempDir(), "widget")
	out := cloneOne(t, "file://"+src, dest, sha)
	if !strings.Contains(out, "via fetch") {
		t.Errorf("the sha clone did not take the fetch fallback:\n%s", out)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "README")); err != nil || string(b) != "hi" {
		t.Errorf("the commit was not checked out (%v):\n%s", err, out)
	}
}

// A pinned ref whose fetch fails leaves the target as the attempt found it, not holding a bare .git
// that the next start would take for a populated checkout and never retry; the retry then clones.
func TestAgentRunLib_FailedFetchLeavesTheTargetRetryable(t *testing.T) {
	src := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", src, "-c", "user.name=t", "-c", "user.email=t@example.test"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("config", "uploadpack.allowAnySHA1InWant", "true")
	if err := os.WriteFile(filepath.Join(src, "README"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "README")
	git("commit", "-q", "-m", "one")
	dest := filepath.Join(t.TempDir(), "widget")
	out := cloneOne(t, "file://"+src, dest, strings.Repeat("0", 40))
	if !strings.Contains(out, "clone FAILED for "+dest) {
		t.Fatalf("no failure line for a sha the remote does not have:\n%s", out)
	}
	if left, _ := os.ReadDir(dest); len(left) != 0 {
		t.Fatalf("the failed fetch left %d entr(ies) in the target (a bare .git reads as a checkout):\n%s", len(left), out)
	}
	out = cloneOne(t, "file://"+src, dest, git("rev-parse", "HEAD"))
	if b, err := os.ReadFile(filepath.Join(dest, "README")); err != nil || string(b) != "hi" {
		t.Errorf("the retry did not clone into the target (%v):\n%s", err, out)
	}
}
