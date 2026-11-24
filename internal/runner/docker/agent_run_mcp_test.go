// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The run's MCP servers reach each harness through the configuration agent-run
// writes or passes from what dispatch delivered: Claude Code's file as a second
// --mcp-config under --strict-mcp-config on both autonomous lanes, Codex CLI's
// broker URLs as [mcp_servers.<key>] tables. PURE-SHELL tests over the REAL
// agent-run scripts with a fake CLI, like the tests beside them.

// ccRunTaskWithMCP is ccRunTask with Claude Code's MCP file at a test path
// (present when mcpFile is non-empty) instead of /etc/claude-code.
func ccRunTaskWithMCP(t *testing.T, mcpFile string, extraEnv ...string) []string {
	t.Helper()
	root := t.TempDir()
	home, binDir := filepath.Join(root, "home"), filepath.Join(root, "bin")
	for _, d := range []string{home, binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	argvLog := filepath.Join(root, "claude.argv")
	fake := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > \"$CLAUDE_ARGV_LOG\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(fake), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	path := filepath.Join(root, "wardyn-mcp.json")
	if mcpFile != "" {
		if err := os.WriteFile(path, []byte(mcpFile), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := ccRunnableAgentRun(t)
	body := ccRead(t, script)
	if !strings.Contains(body, "/etc/claude-code/wardyn-mcp.json") {
		t.Fatal("claude-code agent-run no longer reads /etc/claude-code/wardyn-mcp.json")
	}
	if err := os.WriteFile(script, []byte(strings.ReplaceAll(body, "/etc/claude-code/wardyn-mcp.json", path)), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	cmd := exec.Command("timeout", "20s", "bash", script, "do the task")
	cmd.Env = append(os.Environ(), append([]string{
		"HOME=" + home, "PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"), "CLAUDE_ARGV_LOG=" + argvLog,
		"CLAUDE_CONFIG_DIR=", "CLAUDE_CODE_USE_BEDROCK=1", "WARDYN_REPOS=", "WARDYN_REPO_URL=", "WARDYN_MITM_CA_PEM=",
		"WARDYN_GITHUB_GRANT_ID=", "WARDYN_CLAUDE_MANAGED_B64=", "WARDYN_SSH_GRANTS=", "WARDYN_SCAN_ONLY=", "WARDYN_TASK_MODE=",
		"WARDYN_TOOL_APPROVALS=",
	}, extraEnv...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("agent-run: %v\n%s", err, out)
	}
	argv := strings.Split(strings.TrimSpace(ccRead(t, argvLog)), "\n")
	for i, a := range argv {
		if a == path {
			argv[i] = "<mcp>"
		}
	}
	return argv
}

func TestClaudeAgentRun_MCPFileOnBothAutonomousLanes(t *testing.T) {
	const file = `{"mcpServers":{"linear":{"type":"http","url":"http://wardyn-proxy:3128/wardyn/mcp/t/linear"}}}`
	hold := ccRunTaskWithMCP(t, file, "WARDYN_TOOL_APPROVALS=hold")
	if !ccHasPair(hold, "--mcp-config", "<mcp>") || !slices.Contains(hold, "--strict-mcp-config") ||
		!ccHasPair(hold, "--permission-prompt-tool", "mcp__gate__approve") || slices.Index(hold, "<mcp>") < slices.Index(hold, "--mcp-config") {
		t.Errorf("hold lane: want the gate's file and the MCP file under --strict-mcp-config\nargv: %q", hold)
	}
	auto := ccRunTaskWithMCP(t, file)
	if !ccHasPair(auto, "--mcp-config", "<mcp>") || !slices.Contains(auto, "--strict-mcp-config") || !slices.Contains(auto, "--dangerously-skip-permissions") {
		t.Errorf("autonomous lane: want the MCP file under --strict-mcp-config\nargv: %q", auto)
	}
	// No servers: the autonomous lane is unchanged.
	if none := ccRunTaskWithMCP(t, ""); slices.Contains(none, "--mcp-config") || slices.Contains(none, "--strict-mcp-config") {
		t.Errorf("no MCP file: argv %q gained MCP flags", none)
	}
}

func TestCodexAgentRun_MCPServersRenderedOnceAfterTables(t *testing.T) {
	home, binDir, codexLog := cxFakeCodex(t)
	cfgPath := filepath.Join(home, ".codex", "config.toml")
	cxWrite(t, cfgPath, "model = \"x\"\n\n[projects.\"/home/agent/work\"]\ntrust_level = \"trusted\"\n")
	env := `WARDYN_MCP_SERVERS_JSON={"github":"http://wardyn-proxy:3128/wardyn/mcp/Ab-_9/github","linear":"http://wardyn-proxy:3128/wardyn/mcp/Ab-_9/linear"}`
	cxRunTask(t, home, binDir, codexLog, env)
	first := ccRead(t, cfgPath)
	cxRunTask(t, home, binDir, codexLog, env)
	if got := ccRead(t, cfgPath); got != first {
		t.Fatalf("a second start changed config.toml:\n%s\n---\n%s", first, got)
	}
	want := "[mcp_servers.github]\nurl = \"http://wardyn-proxy:3128/wardyn/mcp/Ab-_9/github\"\n" +
		"[mcp_servers.linear]\nurl = \"http://wardyn-proxy:3128/wardyn/mcp/Ab-_9/linear\"\n"
	if !strings.HasSuffix(first, want) || !strings.HasPrefix(first, "model = \"x\"\n") || strings.Count(first, "[mcp_servers.") != 2 {
		t.Fatalf("config.toml:\n%s", first)
	}
}
