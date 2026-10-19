// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The SSH lane over the REAL claude-code agent-run with fakes: it wires git
// onto the proxy's ssh agent (no key and no known_hosts file is written, no
// mint is asked for), and closes the agent once the start-up clones are done,
// before the agent starts, with the ssh config and GIT_SSH_COMMAND gone.
func TestClaudeAgentRun_SSHLaneUsesTheProxyAgent(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH; agent-run reads WARDYN_SSH_GRANTS with it")
	}
	const grant = "6f0c1d2e-0000-4000-8000-00000000a93d"
	root := t.TempDir()
	home, binDir := filepath.Join(root, "home"), filepath.Join(root, "bin")
	for _, d := range []string{home, binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	curlLog, claudeLog := filepath.Join(root, "curl.log"), filepath.Join(root, "claude.log")
	helper := filepath.Join(binDir, "wardyn-git-helper")
	fakes := map[string]string{
		"claude": "#!/bin/sh\nprintf 'GIT_SSH_COMMAND=%s\\n' \"${GIT_SSH_COMMAND:-unset}\" > \"$CLAUDE_LOG\"\n" +
			"[ -e \"$HOME/.ssh/wardyn_ssh_config\" ] && echo config-left >> \"$CLAUDE_LOG\"\nexit 0\n",
		// Each curl call logs its argv, the GIT_SSH_COMMAND it sees and the ssh
		// config as it stands at that moment.
		"curl":              "#!/bin/sh\n{ echo \"ARGV $*\"; echo \"GIT_SSH_COMMAND=$GIT_SSH_COMMAND\"; cat \"$HOME/.ssh/wardyn_ssh_config\" 2>/dev/null; } >> \"$CURL_LOG\"\n",
		"ssh":               "#!/bin/sh\nexit 0\n",
		"corkscrew":         "#!/bin/sh\nexit 0\n",
		"wardyn-git-helper": "#!/bin/sh\nexit 0\n",
	}
	for name, body := range fakes {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o700); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
	}
	script := ccRunnableAgentRun(t)
	// The helper's image path, pointed at the fake (the GIT_SSH_COMMAND assert
	// below fails if agent-run stops naming it).
	body := strings.ReplaceAll(ccRead(t, script), "/usr/local/bin/wardyn-git-helper", helper)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	cmd := exec.Command("timeout", "20s", "bash", script, "do the task")
	cmd.Env = append(os.Environ(),
		"HOME="+home, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"CURL_LOG="+curlLog, "CLAUDE_LOG="+claudeLog, "WARDYN_PROXY_URL=http://wardyn-proxy:3128",
		`WARDYN_SSH_GRANTS={"github.com":"`+grant+`"}`,
		"CLAUDE_CONFIG_DIR=", "CLAUDE_CODE_USE_BEDROCK=1", "WARDYN_REPOS=", "WARDYN_REPO_URL=", "WARDYN_MITM_CA_PEM=",
		"WARDYN_GITHUB_GRANT_ID=", "WARDYN_CLAUDE_MANAGED_B64=", "WARDYN_SCAN_ONLY=", "WARDYN_TASK_MODE=", "WARDYN_TOOL_APPROVALS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("agent-run: %v\n%s", err, out)
	}

	calls := ccRead(t, curlLog)
	if n := ccCount(calls, "ARGV "); n != 1 || !strings.Contains(calls, "-X POST http://wardyn-proxy:3128/wardyn/v1/ssh-agent/close") {
		t.Fatalf("curl calls (%d), want exactly the agent close:\n%s", n, calls)
	}
	for _, want := range []string{
		"GIT_SSH_COMMAND=" + helper + " ssh-exec --agent github_com=" + grant + " -- ssh -F " + filepath.Join(home, ".ssh", "wardyn_ssh_config"),
		"    IdentityAgent ${WARDYN_SSH_AGENT_DIR}/github_com.sock",
		"    KnownHostsCommand " + helper + " ssh-known-hosts " + grant,
		"    UserKnownHostsFile /dev/null",
		"    HostName ssh.github.com",
		"    ProxyCommand corkscrew wardyn-proxy 3128 %h %p",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("at the close, want %q in:\n%s", want, calls)
		}
	}
	if strings.Contains(calls, "IdentityFile") || strings.Contains(calls, "credentials/mint") {
		t.Errorf("the lane still names a key file or a mint:\n%s", calls)
	}
	entries, err := os.ReadDir(filepath.Join(home, ".ssh"))
	if err != nil || len(entries) != 0 {
		t.Errorf("~/.ssh after the clone = %v (%v), want empty", entries, err)
	}
	if got := ccRead(t, claudeLog); got != "GIT_SSH_COMMAND=unset\n" {
		t.Errorf("the agent started with %q, want no GIT_SSH_COMMAND and no ssh config", got)
	}
}
