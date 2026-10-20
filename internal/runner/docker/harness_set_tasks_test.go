// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tasksLayout is setImageLayout with fake CLIs that take their time and exit
// with a code the test picks: claude after CLAUDE_SLEEP seconds with
// CLAUDE_EXIT, codex at once with CODEX_EXIT. Each records when it ran.
func tasksLayout(t *testing.T) (set, home, binDir, tmuxLog string) {
	t.Helper()
	set, home, binDir, tmuxLog = setImageLayout(t)
	fakes := map[string]string{
		"claude": `#!/bin/sh
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
