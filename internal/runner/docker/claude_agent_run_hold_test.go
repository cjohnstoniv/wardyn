// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ccRunTask runs the REAL claude-code agent-run in task mode with a fake
// `claude` on PATH that records its argv, one argument per line, and returns
// those lines plus the temp $HOME agent-run ran under (so callers can inspect
// files agent-run wrote there, e.g. ~/.wardyn/toolgate-mcp.json).
func ccRunTask(t *testing.T, extraEnv ...string) (argv []string, home string) {
	t.Helper()
	root := t.TempDir()
	home, binDir := filepath.Join(root, "home"), filepath.Join(root, "bin")
	for _, d := range []string{home, binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	argvLog := filepath.Join(root, "claude.argv")
	// CLAUDE_ENV_LOG, when a test sets it, also records the MCP_TOOL_TIMEOUT
	// claude was exec'd with ("unset" when agent-run exported none or an empty one).
	fake := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > \"$CLAUDE_ARGV_LOG\"\n" +
		"[ -z \"$CLAUDE_ENV_LOG\" ] || printf '%s' \"${MCP_TOOL_TIMEOUT:-unset}\" > \"$CLAUDE_ENV_LOG\"\n"
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
	return strings.Split(strings.TrimSpace(ccRead(t, argvLog)), "\n"), home
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
	argv, _ := ccRunTask(t, "WARDYN_TOOL_APPROVALS=hold")
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

// TestAgentRunLib_GoDurationToMs pins the hand-written Go-duration parser the
// hold lane sizes MCP_TOOL_TIMEOUT with (RL-1): the shapes
// time.Duration.String() emits convert to whole milliseconds, and anything it
// cannot read prints nothing and still exits 0 (agent-run runs under set -e).
func TestAgentRunLib_GoDurationToMs(t *testing.T) {
	lib := ccAbs(t, ccAgentRunLibPath)
	for _, tc := range []struct{ in, want string }{
		{"24h0m0s", "86400000"},
		{"1h30m0s", "5400000"},
		{"1m30.5s", "90500"},
		{"250ms", "250"},
		{"0s", ""},
		{"garbage", ""},
		{"24h0m0sx", ""},
		{"", ""},
	} {
		out, err := exec.Command("bash", "-c", `set -euo pipefail; source "$1"; go_duration_to_ms "$2"`,
			"go-duration-test", lib, tc.in).CombinedOutput()
		if err != nil {
			t.Errorf("go_duration_to_ms %q: exit %v, output %q", tc.in, err, out)
			continue
		}
		if got := strings.TrimSpace(string(out)); got != tc.want {
			t.Errorf("go_duration_to_ms %q = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestClaudeAgentRun_HoldLaneSizesMCPToolTimeout pins RL-1's claude-side cap:
// MCP_TOOL_TIMEOUT sits 15m PAST the approval ceiling so wardyn-toolgate's own
// deadline deny (and its final poll) wins, and is never lowered below claude's
// 1e8 ms default — a 24h ceiling leaves it unset. Above claude's own
// 2147483647ms (2^31-1, ~24.85d) clamp on the value, a bare ceiling+15m export
// would be silently lowered by claude itself and lose the deadline race to
// wardyn-toolgate's own (uncapped) -deadline; past that point agent-run caps
// MCP_TOOL_TIMEOUT at the clamp and passes toolgate an explicit -deadline 15m
// under it via toolgate-mcp.json's args, so the gate's deny keeps winning.
func TestClaudeAgentRun_HoldLaneSizesMCPToolTimeout(t *testing.T) {
	const mcpTimeoutClamp = 2147483647
	for _, tc := range []struct{ ceiling, want, wantDeadlineArg string }{
		{"72h0m0s", fmt.Sprint(72*3600000 + 900000), ""},
		{"24h0m0s", "unset", ""},
		{"", "unset", ""},
		// 720h exceeds the ~596h point where ceiling+15m > the clamp.
		{"720h0m0s", fmt.Sprint(mcpTimeoutClamp), fmt.Sprint(mcpTimeoutClamp-900000) + "ms"},
	} {
		envLog := filepath.Join(t.TempDir(), "mcp_tool_timeout")
		_, home := ccRunTask(t, "WARDYN_TOOL_APPROVALS=hold", "WARDYN_APPROVAL_EXPIRY_AFTER="+tc.ceiling,
			"CLAUDE_ENV_LOG="+envLog, "MCP_TOOL_TIMEOUT=")
		if got := ccRead(t, envLog); got != tc.want {
			t.Errorf("ceiling %q: claude saw MCP_TOOL_TIMEOUT=%q, want %q", tc.ceiling, got, tc.want)
		}
		cfg := ccRead(t, filepath.Join(home, ".wardyn", "toolgate-mcp.json"))
		var parsed struct {
			McpServers map[string]struct {
				Args []string `json:"args"`
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal([]byte(cfg), &parsed); err != nil {
			t.Fatalf("ceiling %q: toolgate-mcp.json is not valid JSON: %v\n%s", tc.ceiling, err, cfg)
		}
		gotDeadlineArg := ""
		args := parsed.McpServers["gate"].Args
		for i, a := range args {
			if a == "-deadline" && i+1 < len(args) {
				gotDeadlineArg = args[i+1]
			}
		}
		if gotDeadlineArg != tc.wantDeadlineArg {
			t.Errorf("ceiling %q: toolgate -deadline arg = %q, want %q (toolgate-mcp.json: %s)", tc.ceiling, gotDeadlineArg, tc.wantDeadlineArg, cfg)
		}
	}
}
