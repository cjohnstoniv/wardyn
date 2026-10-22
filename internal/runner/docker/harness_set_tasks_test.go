// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// tasksLayout is setImageLayout with fake CLIs that take their time and exit
// with a code the test picks: claude after CLAUDE_SLEEP seconds with
// CLAUDE_EXIT, codex after CODEX_SLEEP with CODEX_EXIT. Each records when it ran,
// and its pid in $HOME/pid.<cli>.
func tasksLayout(t *testing.T) (set, home, binDir, tmuxLog string) {
	t.Helper()
	set, home, binDir, tmuxLog = setImageLayout(t)
	fakes := map[string]string{
		"claude": `#!/bin/sh
echo $$ > "$HOME/pid.claude"
sleep "${CLAUDE_SLEEP:-0}"
echo "claude says done"
printf 'claude %s\n' "$*" >> "$HOME/calls.log"
exit "${CLAUDE_EXIT:-0}"
`,
		"codex": `#!/bin/sh
if [ "$1" = login ]; then
  mkdir -p "${CODEX_HOME:-$HOME/.codex}"
  cat > "${CODEX_HOME:-$HOME/.codex}/auth.json"
  exit 0
fi
echo $$ > "$HOME/pid.codex"
sleep "${CODEX_SLEEP:-0}"
echo "codex says done"
printf 'codex %s\n' "$*" >> "$HOME/calls.log"
exit "${CODEX_EXIT:-0}"
`,
	}
	for name, script := range fakes {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o700); err != nil { //nolint:gosec // test fixture
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	return set, home, binDir, tmuxLog
}

// lastAgentsLine is the last "wardyn-agents:" status line the supervisor printed.
func lastAgentsLine(out string) string {
	last := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "wardyn-agents:") {
			last = l
		}
	}
	return last
}

var bothTasks = []string{"--tasks", "claude-code", "write the change", "codex-cli", "write the tests"}

// TestHarnessSetImage_TasksExitAll pins the exit rule of several agents in one run:
// the run ends when the LAST agent exits, fails when any agent failed and succeeds
// only when every agent succeeded.
func TestHarnessSetImage_TasksExitAll(t *testing.T) {
	for name, tc := range map[string]struct {
		env      []string
		wantCode int
		wantLast string
	}{
		"every agent succeeds": {nil, 0, "wardyn-agents: claude-code=exited=0 codex-cli=exited=0"},
		// codex fails at once while claude is still working: the run waits for claude.
		"one agent fails first": {[]string{"CLAUDE_SLEEP=1", "CODEX_EXIT=3"}, 1, "wardyn-agents: claude-code=exited=0 codex-cli=exited=3"},
		// the slow one fails after the fast one succeeded: still a failed run.
		"the last agent fails": {[]string{"CLAUDE_SLEEP=1", "CLAUDE_EXIT=4"}, 1, "wardyn-agents: claude-code=exited=4 codex-cli=exited=0"},
	} {
		t.Run(name, func(t *testing.T) {
			set, home, binDir, tmuxLog := tasksLayout(t)
			code, out := runSet(t, set, home, binDir, tmuxLog, "30s", bothTasks, append([]string{"WARDYN_START_HARNESS=claude-code"}, tc.env...)...)
			if code != tc.wantCode {
				t.Fatalf("agent-run --tasks exited %d, want %d\noutput:\n%s", code, tc.wantCode, out)
			}
			if got := lastAgentsLine(out); got != tc.wantLast {
				t.Errorf("last status line = %q, want %q\noutput:\n%s", got, tc.wantLast, out)
			}
			calls := ccRead(t, filepath.Join(home, "calls.log"))
			if !strings.Contains(calls, "write the change") || !strings.Contains(calls, "write the tests") {
				t.Errorf("every agent must run its own task, calls:\n%s", calls)
			}
			// Waiting for the last agent: claude, the slow one, finished before the supervisor did.
			if !strings.Contains(out, "[claude-code] claude says done") {
				t.Errorf("the run ended before its last agent finished\noutput:\n%s", out)
			}
		})
	}
}

// TestHarnessSetImage_TasksPrefixEveryAgentLine pins that each agent's output carries its
// id and that the supervisor's status lines are the only unprefixed ones it adds.
func TestHarnessSetImage_TasksPrefixEveryAgentLine(t *testing.T) {
	set, home, binDir, tmuxLog := tasksLayout(t)
	code, out := runSet(t, set, home, binDir, tmuxLog, "30s", bothTasks, "WARDYN_START_HARNESS=claude-code")
	if code != 0 {
		t.Fatalf("agent-run --tasks exited %d\noutput:\n%s", code, out)
	}
	for _, want := range []string{"[claude-code] claude says done", "[codex-cli] codex says done", "wardyn-agents: claude-code=running codex-cli=running"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q\noutput:\n%s", want, out)
		}
	}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.HasPrefix(l, "[claude-code] ") && !strings.HasPrefix(l, "[codex-cli] ") && !strings.HasPrefix(l, "wardyn-agents:") {
			t.Errorf("unprefixed line %q", l)
		}
	}
}

// TestHarnessSetImage_TasksRefuseAnUnknownAgent: an agent the run does not carry is refused.
func TestHarnessSetImage_TasksRefuseAnUnknownAgent(t *testing.T) {
	set, home, binDir, tmuxLog := tasksLayout(t)
	code, out := runSet(t, set, home, binDir, tmuxLog, "10s", []string{"--tasks", "gemini-cli", "x"}, "WARDYN_START_HARNESS=claude-code")
	if code != 1 || !strings.Contains(out, "agent 'gemini-cli' is not one of this run's harnesses") {
		t.Fatalf("exited %d, want 1 naming the agent\noutput:\n%s", code, out)
	}
}

// TestHarnessSetImage_TasksTermStopsEveryAgent: TERM to the supervisor reaches every
// agent's launcher and what it started, the supervisor names each agent's exit in a
// last status line, and it exits non-zero without tripping set -u.
func TestHarnessSetImage_TasksTermStopsEveryAgent(t *testing.T) {
	set, home, binDir, tmuxLog := tasksLayout(t)
	cmd := exec.Command("bash", append([]string{set}, bothTasks...)...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.Env = append(os.Environ(),
		"HOME="+home, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"), "TMUX_LOG="+tmuxLog,
		"WARDYN_HARNESSES=claude-code codex-cli", "WARDYN_START_HARNESS=claude-code", "WARDYN_REPOS=", "WARDYN_REPO_URL=",
		"WARDYN_MITM_CA_PEM=", "WARDYN_GITHUB_GRANT_ID=", "WARDYN_CLAUDE_MANAGED_B64=",
		"WARDYN_CODEX_BASE_URL=http://proxy.invalid/wardyn/llm/openai/v1", "CODEX_API_KEY=wardyn-proxy-injected",
		"CLAUDE_SLEEP=60", "CODEX_SLEEP=60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	pidOf := func(cli string) int {
		b, err := os.ReadFile(filepath.Join(home, "pid."+cli))
		if err != nil {
			return 0
		}
		n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		return n
	}
	for deadline := time.Now().Add(20 * time.Second); pidOf("claude") == 0 || pidOf("codex") == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("both agents never started\noutput:\n%s", out.String())
		}
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("the supervisor did not exit after TERM\noutput:\n%s", out.String())
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
		t.Errorf("supervisor exit = %v, want non-zero\noutput:\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "unbound variable") {
		t.Errorf("set -u tripped on teardown\noutput:\n%s", out.String())
	}
	if got := lastAgentsLine(out.String()); !strings.Contains(got, "claude-code=exited=") || !strings.Contains(got, "codex-cli=exited=") {
		t.Errorf("last status line = %q, want every agent exited\noutput:\n%s", got, out.String())
	}
	for _, cli := range []string{"claude", "codex"} {
		pid := pidOf(cli)
		gone := false
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if syscall.Kill(pid, 0) != nil {
				gone = true
				break
			}
		}
		if !gone {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Errorf("fake %s (pid %d) outlived the supervisor\noutput:\n%s", cli, pid, out.String())
		}
	}
}
