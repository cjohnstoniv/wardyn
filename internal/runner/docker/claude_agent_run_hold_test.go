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

// ccRunTask runs the REAL claude-code agent-run in task mode with a fake
// `claude` on PATH that records its argv, one argument per line, and returns
// those lines.
func ccRunTask(t *testing.T, extraEnv ...string) []string {
	t.Helper()
	root := t.TempDir()
	home, binDir := filepath.Join(root, "home"), filepath.Join(root, "bin")
	for _, d := range []string{home, binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	argvLog := filepath.Join(root, "claude.argv")
	fake := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > \"$CLAUDE_ARGV_LOG\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(fake), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fake claude: %v", err)
	}
	cmd := exec.Command("timeout", "20s", "bash", ccRunnableAgentRun(t), "do the task")
	cmd.Env = append(os.Environ(), append([]string{
		"HOME=" + home,
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"CLAUDE_ARGV_LOG=" + argvLog,
		"CLAUDE_CONFIG_DIR=",
		"CLAUDE_CODE_USE_BEDROCK=1",
		"WARDYN_REPOS=",
		"WARDYN_REPO_URL=",
		"WARDYN_MITM_CA_PEM=",
		"WARDYN_GITHUB_GRANT_ID=",
		"WARDYN_CLAUDE_MANAGED_B64=",
		"WARDYN_SSH_GRANTS=",
		"WARDYN_SCAN_ONLY=",
		"WARDYN_TASK_MODE=",
		"WARDYN_TOOL_APPROVALS=",
	}, extraEnv...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("agent-run task mode: %v\noutput: %s", err, out)
	}
	return strings.Split(strings.TrimSpace(ccRead(t, argvLog)), "\n")
}

func ccHasPair(argv []string, flag, val string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == val {
			return true
		}
	}
	return false
}

// TestClaudeAgentRun_HoldLaneLoadsNoRepoSettings pins #358's interim fix. On the
// hold lane Claude Code resolves permissions.allow rules BEFORE it asks the
// --permission-prompt-tool, so a rule in the workspace's .claude/settings.json
// (which the agent can write) ran tools without Wardyn's gate being asked.
// `--setting-sources user` keeps project and local settings out; the toolgate
// flags must survive beside it.
func TestClaudeAgentRun_HoldLaneLoadsNoRepoSettings(t *testing.T) {
	argv := ccRunTask(t, "WARDYN_TOOL_APPROVALS=hold")
	if !ccHasPair(argv, "--setting-sources", "user") {
		t.Errorf("hold lane does not pass `--setting-sources user` — a repository permissions.allow rule approves tool calls before the gate is asked (#358)\nargv: %q", argv)
	}
	if !ccHasPair(argv, "--permission-prompt-tool", "mcp__gate__approve") || !ccHasPair(argv, "--permission-mode", "manual") {
		t.Errorf("hold lane lost its gate flags\nargv: %q", argv)
	}
	strict := false
	for _, a := range argv {
		if a == "--strict-mcp-config" {
			strict = true
		}
		if a == "--dangerously-skip-permissions" {
			t.Errorf("hold lane passes --dangerously-skip-permissions, which resolves calls before the gate\nargv: %q", argv)
		}
	}
	if !strict {
		t.Errorf("hold lane lost --strict-mcp-config\nargv: %q", argv)
	}
}
