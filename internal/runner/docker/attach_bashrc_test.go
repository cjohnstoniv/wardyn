// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// attachBashrcPath resolves deploy/images/common/attach-bashrc.sh relative to
// this package (go test's working directory is always the package dir). It is
// baked in as /home/agent/.bashrc by every agent image, and attachShell's
// tmux→bash chain (session.go) is what sources it.
const attachBashrcPath = "../../../deploy/images/common/attach-bashrc.sh"

// runAttachShell sources the REAL attach-bashrc.sh in an interactive bash with
// HOME pointed at a scratch dir and a fake agent CLI first on PATH, then
// returns how many times that fake was invoked. Deliberately runs the actual
// script rather than pattern-matching its source: the branch under test is
// shell, so only a shell can prove it.
func runAttachShell(t *testing.T, home, binDir string, env ...string) int {
	t.Helper()
	calls, _ := runAttachShellOut(t, home, binDir, 0, env...)
	return len(calls)
}

// runAttachShellOut is runAttachShell returning what the fake agent recorded (its working
// directory per start) and the shell's own output. A non-zero timeout kills a shell that is
// still waiting, so a regression to a long wait fails in seconds instead of minutes.
func runAttachShellOut(t *testing.T, home, binDir string, timeout time.Duration, env ...string) (calls []string, out string) {
	t.Helper()
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "bash", "-i", "-c", "true")
	cmd.Env = append(os.Environ(),
		append([]string{
			"HOME=" + home,
			"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			// Keep the workspace-wait branch out of the picture: no WARDYN_REPOS
			// and no ~/.wardyn/workdir means it is skipped entirely.
			"WARDYN_REPOS=",
			"WARDYN_REPO_URL=",
		}, env...)...)
	b, err := cmd.CombinedOutput()
	out = string(b)
	if err != nil {
		// bash -i without a tty warns about job control on some hosts but still
		// sources .bashrc; only a hard failure with no output is fatal.
		if len(b) == 0 {
			t.Fatalf("interactive bash failed: %v", err)
		}
	}
	rec, readErr := os.ReadFile(filepath.Join(home, "calls.log"))
	if readErr != nil {
		return nil, out
	}
	return strings.Fields(strings.TrimSpace(string(rec))), out
}

// newAttachHome lays out a scratch $HOME carrying the real attach-bashrc.sh as
// ~/.bashrc, plus a fake `claude` on PATH that records each invocation.
func newAttachHome(t *testing.T) (home, binDir string) {
	t.Helper()
	src, err := filepath.Abs(attachBashrcPath)
	if err != nil {
		t.Fatalf("resolve attach-bashrc.sh path: %v", err)
	}
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("attach-bashrc.sh not found at %s: %v", src, err)
	}
	home = t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), body, 0o644); err != nil {
		t.Fatalf("write .bashrc: %v", err)
	}
	binDir = t.TempDir()
	fake := "#!/bin/sh\necho \"$PWD\" >> \"$HOME/calls.log\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(fake), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return home, binDir
}

// TestAttachBashrc_StartsAgentOnceWhenRequested pins the interactive_start
// contract end to end on the shell side: WARDYN_INTERACTIVE_START=agent (set by
// applyDispatchModeEnv, inherited by the attach exec from the container's env)
// launches the image's agent CLI, and the ~/.wardyn/agent-started marker keeps
// it to the FIRST shell — the tmux windows and panes a human opens afterwards
// get a plain prompt, not a second agent.
func TestAttachBashrc_StartsAgentOnceWhenRequested(t *testing.T) {
	home, binDir := newAttachHome(t)

	if got := runAttachShell(t, home, binDir, "WARDYN_INTERACTIVE_START=agent"); got != 1 {
		t.Fatalf("first attach shell: agent CLI ran %d times, want 1", got)
	}
	if got := runAttachShell(t, home, binDir, "WARDYN_INTERACTIVE_START=agent"); got != 1 {
		t.Fatalf("second shell in the same sandbox: agent CLI ran %d times total, want 1 (the marker must suppress it)", got)
	}
}

// TestAttachBashrc_NoAgentWithoutRequest is the other half: the default
// interactive run (interactive_start unset or "shell") never puts the env var
// on the sandbox, so the attach shell stays a bare shell. This is the
// long-standing behavior and must not change by accident.
func TestAttachBashrc_NoAgentWithoutRequest(t *testing.T) {
	home, binDir := newAttachHome(t)

	if got := runAttachShell(t, home, binDir); got != 0 {
		t.Fatalf("attach shell with no interactive_start: agent CLI ran %d times, want 0", got)
	}
	if got := runAttachShell(t, home, binDir, "WARDYN_INTERACTIVE_START=shell"); got != 0 {
		t.Fatalf("attach shell with interactive_start=shell: agent CLI ran %d times, want 0", got)
	}
}

// attachPrep lays out what agent-run --idle leaves for the attach shell: the resolved workspace
// directory, then (LAST) the prep-done marker.
func attachPrep(t *testing.T, home string) (workdir string, finish func()) {
	t.Helper()
	workdir = t.TempDir()
	return workdir, func() {
		_ = os.MkdirAll(filepath.Join(home, ".wardyn"), 0o755)
		_ = os.WriteFile(filepath.Join(home, ".wardyn", "workdir"), []byte(workdir), 0o644)
		_ = os.WriteFile(filepath.Join(home, ".wardyn", "prep-done"), nil, 0o644)
	}
}

// liveIdle starts a process standing in for agent-run --idle (the attach shell asks whether it is
// still running) and returns its pid.
func liveIdle(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start idle stand-in: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return strconv.Itoa(cmd.Process.Pid)
}

// TestAttachBashrc_StartFolderRunWaitsForPrep pins the second path of the wait: a run with a
// WARDYN_START_FOLDER and no repos has no WARDYN_REPOS to key on, so a first attach before prep wrote
// ~/.wardyn/workdir skipped the wait and started the agent outside the chosen folder.
func TestAttachBashrc_StartFolderRunWaitsForPrep(t *testing.T) {
	home, binDir := newAttachHome(t)
	workdir, finish := attachPrep(t, home)
	idle := liveIdle(t)
	time.AfterFunc(1500*time.Millisecond, finish)

	calls, out := runAttachShellOut(t, home, binDir, 30*time.Second,
		"WARDYN_INTERACTIVE_START=agent", "WARDYN_START_FOLDER=/work/sub", "WARDYN_IDLE_PID="+idle)
	if len(calls) != 1 || calls[0] != workdir {
		t.Fatalf("agent started in %v, want exactly once in the prepared workspace %s; output: %s", calls, workdir, out)
	}
}

// TestAttachBashrc_PrepGoneDoesNotStartAgent pins the bound of the wait: it is the idle process still
// running, not a clock. With that process gone and no prep-done (the clone never finished), the shell
// says so at once and does not start the agent over a half-cloned tree; a 120 s cap used to do exactly
// that on a slow clone.
func TestAttachBashrc_PrepGoneDoesNotStartAgent(t *testing.T) {
	home, binDir := newAttachHome(t)
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatalf("run idle stand-in: %v", err)
	}

	calls, out := runAttachShellOut(t, home, binDir, 30*time.Second,
		"WARDYN_INTERACTIVE_START=agent", "WARDYN_REPOS=https://example.invalid/r.git", "WARDYN_IDLE_PID="+strconv.Itoa(dead.Process.Pid))
	if len(calls) != 0 {
		t.Fatalf("agent started %v although preparation never finished", calls)
	}
	if !strings.Contains(out, "ended without finishing") {
		t.Fatalf("the shell did not say preparation ended without finishing; output: %s", out)
	}
}

// TestAttachBashrc_SlowPrepStillStartsAgentInWorkspace is the counterweight: preparation that takes a
// while but completes is waited for, and the agent starts in the workspace.
func TestAttachBashrc_SlowPrepStillStartsAgentInWorkspace(t *testing.T) {
	home, binDir := newAttachHome(t)
	workdir, finish := attachPrep(t, home)
	idle := liveIdle(t)
	time.AfterFunc(2*time.Second, finish)

	calls, out := runAttachShellOut(t, home, binDir, 30*time.Second,
		"WARDYN_INTERACTIVE_START=agent", "WARDYN_REPOS=https://example.invalid/r.git", "WARDYN_IDLE_PID="+idle)
	if len(calls) != 1 || calls[0] != workdir {
		t.Fatalf("agent started in %v, want exactly once in %s; output: %s", calls, workdir, out)
	}
}
