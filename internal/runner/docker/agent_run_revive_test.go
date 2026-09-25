// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Revive after a reboot, image side (long-holds design rev 4, §4 row 3). The
// control plane `docker start`s a kept container, which re-runs `agent-run
// --idle` over the writable layer the run left. Pure-shell tests over the REAL
// scripts, like claude_agent_run_boot_test.go: the second --idle on the same
// $HOME is the revive.

// TestClaudeAgentRun_ReviveContinuesNeverReseeds — the second boot of a seeded
// agent run continues the conversation and never starts the seed again: the
// operator's first prompt, possibly with auto-tools, must not replay against
// a workspace it already changed.
func TestClaudeAgentRun_ReviveContinuesNeverReseeds(t *testing.T) {
	home, binDir, tmuxLog := ccIdleEnv(t)
	env := []string{ccSeedEnv, "WARDYN_INTERACTIVE_START=agent"}
	if code, out := ccRunIdle(t, home, binDir, tmuxLog, env...); code != 124 {
		t.Fatalf("first boot exited %d (output: %s), want 124", code, out)
	}
	if err := os.Truncate(tmuxLog, 0); err != nil {
		t.Fatal(err)
	}
	code, out := ccRunIdle(t, home, binDir, tmuxLog, env...)
	if code != 124 {
		t.Fatalf("revive exited %d (output: %s), want 124 — it must hold the container open for attach", code, out)
	}
	log := ccRead(t, tmuxLog)
	if strings.Contains(log, "--boot-seed") {
		t.Errorf("the revive started the seed again\ntmux log:\n%s", log)
	}
	if n := ccCount(log, "argv: new-session -d -s wardyn agent-run --continue"); n != 1 {
		t.Fatalf("tmux got %d `new-session -d -s wardyn agent-run --continue` calls, want exactly 1\ntmux log:\n%s\noutput:\n%s", n, log, out)
	}
	// The pane waits for prep-done, so the last boot's must be gone before it
	// exists, and the marker must be back so a first attach starts no second
	// claude.
	if !strings.Contains(log, "prep-done: absent") || !strings.Contains(log, "marker: present") {
		t.Errorf("at the revive session: want the last boot's prep-done cleared and agent-started written\ntmux log:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(home, ".wardyn", "prep-done")); err != nil {
		t.Errorf("the revive never finished this boot's prep: %v", err)
	}
}

// TestClaudeAgentRun_ReviveRedoesTheGitHelperSecret — the last boot's
// caller-auth secret is a 0400 file even its owner cannot reopen for writing.
// Rewritten in place, prep dies under `set -e` and the revived container exits.
func TestClaudeAgentRun_ReviveRedoesTheGitHelperSecret(t *testing.T) {
	home, binDir, tmuxLog := ccIdleEnv(t)
	grant := "WARDYN_GITHUB_GRANT_ID=11111111-1111-1111-1111-111111111111"
	if code, out := ccRunIdle(t, home, binDir, tmuxLog, grant); code != 124 {
		t.Fatalf("first boot exited %d (output: %s), want 124", code, out)
	}
	secret := filepath.Join(home, ".wardyn", "git-helper.secret")
	first := ccRead(t, secret)
	if code, out := ccRunIdle(t, home, binDir, tmuxLog, grant); code != 124 {
		t.Fatalf("revive exited %d (output: %s), want 124 — a kept 0400 secret must not kill the revived container", code, out)
	}
	if got := ccRead(t, secret); got == "" || got == first {
		t.Errorf("git-helper secret after the revive = %q (first boot %q); want a fresh one", got, first)
	}
}

// TestClaudeAgentRun_ReviveOfAShellRun — a run that did not land the human in
// the agent gets its files and a shell back: no session, and no agent-started
// marker left to hide the agent from a later attach.
func TestClaudeAgentRun_ReviveOfAShellRun(t *testing.T) {
	home, binDir, tmuxLog := ccIdleEnv(t)
	env := []string{ccSeedEnv, "WARDYN_INTERACTIVE_START=shell"}
	ccRunIdle(t, home, binDir, tmuxLog, env...)
	if err := os.Truncate(tmuxLog, 0); err != nil {
		t.Fatal(err)
	}
	if code, out := ccRunIdle(t, home, binDir, tmuxLog, env...); code != 124 {
		t.Fatalf("revive exited %d (output: %s), want 124", code, out)
	}
	if log := ccRead(t, tmuxLog); log != "" {
		t.Errorf("the revive of a shell run called tmux; its startup command must not run again\ntmux log:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(home, ".wardyn", "agent-started")); err == nil {
		t.Error("the revive left agent-started behind")
	}
}

// TestClaudeAgentRun_ContinuePane — the revive session's pane waits for this
// boot's prep, then continues the conversation in the workspace: `claude
// --continue`, never the seed.
func TestClaudeAgentRun_ContinuePane(t *testing.T) {
	root := t.TempDir()
	home, binDir := filepath.Join(root, "home"), filepath.Join(root, "bin")
	work := filepath.Join(home, "work", "repo")
	for _, d := range []string{filepath.Join(home, ".wardyn"), binDir, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	fake := "#!/bin/sh\npwd >> \"$HOME/claude.log\"\nprintf '%s\\n' \"$*\" >> \"$HOME/claude.log\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(fake), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fake claude: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, ".wardyn", "workdir"), []byte(work+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".wardyn", "prep-done"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("timeout", "10s", "bash", ccRunnableAgentRun(t), "--continue")
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"WARDYN_INTERACTIVE_START=agent",
		ccSeedEnv,
		"CLAUDE_CODE_USE_BEDROCK=1",
		"WARDYN_SEED_AUTO_TOOLS=",
		"WARDYN_IDLE_PID="+strconv.Itoa(os.Getpid()),
	)
	cmd.Stdin = strings.NewReader("")
	_ = cmd.Run()
	got := ccRead(t, filepath.Join(home, "claude.log"))
	if !strings.Contains(got, work+"\n--continue\n") {
		t.Errorf("want `claude --continue` in the prepared workspace %q\nclaude log:\n%s", work, got)
	}
	if strings.Contains(got, "say hello") || strings.Contains(got, "--dangerously-skip-permissions") {
		t.Errorf("the continue pane replayed the seed or widened the run's posture\nclaude log:\n%s", got)
	}
}

// TestCodexAgentRun_ReviveStartsFresh — codex does not continue: the revive
// never starts the seed again, and clears agent-started so the first attach
// starts a fresh codex in the workspace.
func TestCodexAgentRun_ReviveStartsFresh(t *testing.T) {
	home, binDir, tmuxLog := ccIdleEnv(t)
	env := []string{ccSeedEnv, "WARDYN_INTERACTIVE_START=agent"}
	cxRunIdle(t, home, binDir, tmuxLog, env...)
	if err := os.Truncate(tmuxLog, 0); err != nil {
		t.Fatal(err)
	}
	if code, out := cxRunIdle(t, home, binDir, tmuxLog, env...); code != 124 {
		t.Fatalf("revive exited %d (output: %s), want 124", code, out)
	}
	if log := ccRead(t, tmuxLog); strings.Contains(log, "--boot-seed") {
		t.Errorf("the codex revive started the seed again\ntmux log:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(home, ".wardyn", "agent-started")); err == nil {
		t.Error("the codex revive left agent-started behind, so no attach would start codex")
	}
	if _, err := os.Stat(filepath.Join(home, ".wardyn", "prep-done")); err != nil {
		t.Errorf("the codex revive never finished this boot's prep: %v", err)
	}
}
