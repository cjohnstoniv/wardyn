// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An application run with no harness (#1994) starts from agent-base: interactive,
// a startup command, no harness and no custom image. Dispatch delivers the command
// as WARDYN_INTERACTIVE_SEED and sets no WARDYN_INTERACTIVE_START (that is only
// for interactive_start=agent), and both drivers run `agent-run --idle` as the
// main process. These run agent-base's REAL agent-run over a fake tmux that
// actually runs the pane command, the way the `wardyn` session would.

// baseBootEnv lays out $HOME and a PATH dir holding the runnable stub as
// `agent-run` (the pane command resolves it by name) and the fake tmux.
func baseBootEnv(t *testing.T) (home, binDir, tmuxLog string) {
	t.Helper()
	root := t.TempDir()
	home, binDir, tmuxLog = filepath.Join(root, "home"), filepath.Join(root, "bin"), filepath.Join(root, "tmux.log")
	for _, d := range []string{home, binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.Rename(baseRunnableAgentRun(t), filepath.Join(binDir, "agent-run")); err != nil {
		t.Fatalf("place runnable agent-run: %v", err)
	}
	// new-session -d -s wardyn <cmd>: run <cmd> detached, as the session's pane would.
	fake := `#!/bin/sh
printf '%s\n' "$*" >> "$TMUX_LOG"
if [ "$1" = new-session ]; then
  shift 4
  (sh -c "$1" </dev/null >>"$HOME/pane.log" 2>&1 &)
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(fake), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fake tmux: %v", err)
	}
	return home, binDir, tmuxLog
}

// baseRunIdle runs `agent-run --idle` for hold seconds under timeout: 124 means
// it held the container open, as it must.
func baseRunIdle(t *testing.T, home, binDir, tmuxLog string, hold string, extraEnv ...string) (code int, out string) {
	t.Helper()
	cmd := exec.Command("timeout", hold, "bash", filepath.Join(binDir, "agent-run"), "--idle")
	cmd.Env = append(os.Environ(), append([]string{
		"HOME=" + home,
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"TMUX_LOG=" + tmuxLog,
		"WARDYN_REPOS=", "WARDYN_REPO_URL=", "WARDYN_MITM_CA_PEM=", "WARDYN_GITHUB_GRANT_ID=",
		"WARDYN_INTERACTIVE_START=", "WARDYN_RECORDING=",
	}, extraEnv...)...)
	b, err := cmd.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), string(b)
	}
	if err != nil {
		t.Fatalf("running agent-run --idle: %v\noutput: %s", err, b)
	}
	return 0, string(b)
}

// The startup command runs once, in the prepared workspace, after prep-done.
func TestAgentBaseIdle_RunsTheStartupCommandOnceAfterPrep(t *testing.T) {
	home, binDir, tmuxLog := baseBootEnv(t)
	seed := `WARDYN_INTERACTIVE_SEED={ test -e "$HOME/.wardyn/prep-done" && echo prepped || echo unprepped; pwd; } >> "$HOME/seed.log"`
	code, out := baseRunIdle(t, home, binDir, tmuxLog, "4s", seed)
	if code != 124 {
		t.Fatalf("agent-run --idle exited %d, want 124 (held open)\noutput: %s", code, out)
	}
	time.Sleep(1500 * time.Millisecond) // a second start, if any, would land here
	got := ccRead(t, filepath.Join(home, "seed.log"))
	if got == "" {
		t.Fatalf("the application's startup command never ran: agent-base's agent-run --idle prepared and held, and nothing started the seed\nagent-run output:\n%s\ntmux calls:\n%s\npane:\n%s",
			out, ccRead(t, tmuxLog), ccRead(t, filepath.Join(home, "pane.log")))
	}
	if n := strings.Count(got, "prepped\n") + strings.Count(got, "unprepped\n"); n != 1 {
		t.Errorf("the startup command ran %d times, want exactly once\nseed log:\n%s", n, got)
	}
	if !strings.HasPrefix(got, "prepped\n") {
		t.Errorf("the startup command ran before prep-done\nseed log:\n%s", got)
	}
	if !strings.Contains(got, filepath.Join(home, "work")+"\n") {
		t.Errorf("the startup command did not run in the prepared workspace %s\nseed log:\n%s", filepath.Join(home, "work"), got)
	}
	if n := ccCount(ccRead(t, tmuxLog), "new-session -d -s wardyn agent-run --boot-seed"); n != 1 {
		t.Errorf("tmux got %d boot sessions, want 1\ntmux calls:\n%s", n, ccRead(t, tmuxLog))
	}
	if pane := ccRead(t, filepath.Join(home, "pane.log")); !strings.Contains(pane, "exited 0") {
		t.Errorf("the pane does not say how the startup command exited\npane:\n%s", pane)
	}
}

// Control: no seed is the plain environment run — no session, prepared, held.
func TestAgentBaseIdle_NoSeedStartsNothing(t *testing.T) {
	home, binDir, tmuxLog := baseBootEnv(t)
	code, out := baseRunIdle(t, home, binDir, tmuxLog, "2s")
	if code != 124 {
		t.Fatalf("agent-run --idle exited %d, want 124\noutput: %s", code, out)
	}
	if log := ccRead(t, tmuxLog); log != "" {
		t.Errorf("an unseeded run called tmux:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(home, ".wardyn", "prep-done")); err != nil {
		t.Errorf("the unseeded run never finished prep: %v", err)
	}
}

// Control: a starting prompt (interactive_start=agent) has no agent here to take
// it, so it is still only warned about, never run as a command.
func TestAgentBaseIdle_StartingPromptIsNotRunAsACommand(t *testing.T) {
	home, binDir, tmuxLog := baseBootEnv(t)
	code, out := baseRunIdle(t, home, binDir, tmuxLog, "2s",
		"WARDYN_INTERACTIVE_START=agent", `WARDYN_INTERACTIVE_SEED=touch "$HOME/ran"`)
	if code != 124 {
		t.Fatalf("agent-run --idle exited %d, want 124\noutput: %s", code, out)
	}
	if log := ccRead(t, tmuxLog); log != "" {
		t.Errorf("a starting prompt started a boot session:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(home, "ran")); err == nil {
		t.Error("a starting prompt ran as a shell command")
	}
	if !strings.Contains(out, "ships no coding agent") {
		t.Errorf("no warning that the prompt is not started\noutput: %s", out)
	}
}

// A revive (docker start over the kept writable layer) runs --idle again: the
// application's startup command starts again, once more, after this boot's prep.
func TestAgentBaseIdle_ReviveStartsTheStartupCommandAgain(t *testing.T) {
	home, binDir, tmuxLog := baseBootEnv(t)
	seed := `WARDYN_INTERACTIVE_SEED={ test -e "$HOME/.wardyn/prep-done" && echo prepped || echo unprepped; } >> "$HOME/seed.log"`
	for i := range 2 {
		if code, out := baseRunIdle(t, home, binDir, tmuxLog, "3s", seed); code != 124 {
			t.Fatalf("boot %d exited %d, want 124\noutput: %s", i+1, code, out)
		}
		time.Sleep(1200 * time.Millisecond) // the pane's start, if any, lands here
	}
	if got := ccRead(t, filepath.Join(home, "seed.log")); got != "prepped\nprepped\n" {
		t.Errorf("after a boot and a revive the startup command's log is %q, want it started once per boot, each after prep", got)
	}
	if n := ccCount(ccRead(t, tmuxLog), "new-session -d -s wardyn agent-run --boot-seed"); n != 2 {
		t.Errorf("tmux got %d boot sessions over two boots, want 2\ntmux calls:\n%s", n, ccRead(t, tmuxLog))
	}
}
