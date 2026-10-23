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

// TestCodexAgentRun_HarnessSandboxLaunchesTheMode: under WARDYN_HARNESS_SANDBOX, agent-run probes
// Codex's own sandbox in that mode and then runs codex exec in it, with approvals never and, for
// workspace-write, the sandbox's network kept (#2822). Unset keeps today's bypass launch.
func TestCodexAgentRun_HarnessSandboxLaunchesTheMode(t *testing.T) {
	const net = " -c sandbox_workspace_write.network_access=true"
	for mode, want := range map[string]string{
		"": "argv: exec --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox a task\n",
		"read-only": "argv: sandbox -c sandbox_mode=\"read-only\" -- true\n" +
			"argv: exec --skip-git-repo-check -s read-only -c approval_policy=\"never\" a task\n",
		"workspace-write": "argv: sandbox -c sandbox_mode=\"workspace-write\"" + net + " -- true\n" +
			"argv: exec --skip-git-repo-check -s workspace-write -c approval_policy=\"never\"" + net + " a task\n",
	} {
		t.Run(mode, func(t *testing.T) {
			home, binDir, codexLog := cxArgvCodex(t)
			cxRunTask(t, home, binDir, codexLog, "WARDYN_HARNESS_SANDBOX="+mode)
			if got := ccRead(t, codexLog); got != want {
				t.Errorf("codex argv = %q, want %q", got, want)
			}
		})
	}
}

// TestCodexAgentRun_HarnessSandboxFailsClosed: a sandbox that does not start, or a mode agent-run
// does not know, stops the run before codex exec: a task run without the wall its policy requires
// could report edits it never made, or make ones it should not.
func TestCodexAgentRun_HarnessSandboxFailsClosed(t *testing.T) {
	for name, tc := range map[string]struct{ mode, fake string }{
		"the probe fails": {"read-only", "#!/bin/sh\nprintf 'argv: %s\\n' \"$*\" >> \"$CODEX_LOG\"\n[ \"$1\" = sandbox ] && exit 1\nexit 0\n"},
		"an unknown mode": {"danger-full-access", "#!/bin/sh\nprintf 'argv: %s\\n' \"$*\" >> \"$CODEX_LOG\"\n"},
	} {
		t.Run(name, func(t *testing.T) {
			home, binDir, codexLog := cxArgvCodex(t)
			if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte(tc.fake), 0o700); err != nil { //nolint:gosec // test fixture
				t.Fatal(err)
			}
			cmd := exec.Command("timeout", "20s", "bash", cxRunnableAgentRun(t), "a task")
			cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"CODEX_LOG="+codexLog, "CODEX_HOME=", "WARDYN_CODEX_BASE_URL=", "CODEX_API_KEY=", "WARDYN_REPOS=", "WARDYN_REPO_URL=",
				"WARDYN_MITM_CA_PEM=", "WARDYN_GITHUB_GRANT_ID=", "WARDYN_HARNESS_SANDBOX="+tc.mode)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("agent-run exited 0, want a refusal\noutput: %s", out)
			}
			if log, _ := os.ReadFile(codexLog); strings.Contains(string(log), "argv: exec") {
				t.Errorf("codex exec ran: %q", log)
			}
		})
	}
}
