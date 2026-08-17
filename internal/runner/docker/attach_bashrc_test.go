// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	cmd := exec.Command("bash", "-i", "-c", "true")
	cmd.Env = append(os.Environ(),
		append([]string{
			"HOME=" + home,
			"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			// Keep the workspace-wait branch out of the picture: no WARDYN_REPOS
			// and no ~/.wardyn/workdir means it is skipped entirely.
			"WARDYN_REPOS=",
			"WARDYN_REPO_URL=",
		}, env...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// bash -i without a tty warns about job control on some hosts but still
		// sources .bashrc; only a hard failure with no output is fatal.
		if len(out) == 0 {
			t.Fatalf("interactive bash failed: %v", err)
		}
	}
	b, readErr := os.ReadFile(filepath.Join(home, "calls.log"))
	if readErr != nil {
		return 0
	}
	return len(strings.Fields(strings.TrimSpace(string(b))))
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
	fake := "#!/bin/sh\necho ran >> \"$HOME/calls.log\"\n"
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
