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

// The harnesses image (deploy/images/harnesses): one sandbox carrying several
// harness CLIs, one of them started. PURE-SHELL tests over the REAL scripts, the
// idiom of the sibling attach-bashrc and agent-run tests: nothing here needs a
// docker daemon.
const agentRunSetPath = "../../../deploy/images/common/agent-run-set"

// TestAttachBashrc_StartsTheStartHarness — on the set image the first-attach
// auto-start launches the CLI of WARDYN_START_HARNESS, not the first one found
// (which is claude). An unset or unknown value keeps the fixed list; a set but empty one (Your own
// tools) starts no CLI.
func TestAttachBashrc_StartsTheStartHarness(t *testing.T) {
	for _, tc := range []struct {
		name, start, want string
		unset             bool
	}{
		{"start=codex-cli", "codex-cli", "codex", false},
		{"start=claude-code", "claude-code", "claude", false},
		{"unset", "", "claude", true},
		{"empty", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, binDir := newAttachHome(t)
			for _, cli := range []string{"claude", "codex"} {
				fake := "#!/bin/sh\necho " + cli + " >> \"$HOME/calls.log\"\n"
				if err := os.WriteFile(filepath.Join(binDir, cli), []byte(fake), 0o755); err != nil { //nolint:gosec // test fixture
					t.Fatalf("write fake %s: %v", cli, err)
				}
			}
			env := []string{"WARDYN_INTERACTIVE_START=agent"}
			if !tc.unset {
				env = append(env, "WARDYN_START_HARNESS="+tc.start)
			}
			runAttachShell(t, home, binDir, env...)
			var started []string
			if b, err := os.ReadFile(filepath.Join(home, "calls.log")); err == nil {
				started = strings.Fields(string(b))
			}
			want := []string{}
			if tc.want != "" {
				want = []string{tc.want}
			}
			if len(started) != len(want) || (len(want) == 1 && started[0] != want[0]) {
				t.Fatalf("WARDYN_START_HARNESS=%q (unset %v) started %v, want exactly %v", tc.start, tc.unset, started, want)
			}
		})
	}
}

// setImageLayout lays out what the Dockerfile installs: the set launcher plus the
// REAL claude-code and codex-cli launchers under a harness dir, each sourcing the
// real library, with fake `claude`, `codex` and `tmux` first on PATH. It returns
// the set launcher, the scratch HOME and the PATH dir.
func setImageLayout(t *testing.T) (set, home, binDir, tmuxLog string) {
	t.Helper()
	home, binDir, tmuxLog = ccIdleEnv(t)
	harnessDir := t.TempDir()
	for id, runnable := range map[string]string{"claude-code": ccRunnableAgentRun(t), "codex-cli": cxRunnableAgentRun(t)} {
		dst := filepath.Join(harnessDir, id, "agent-run")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dst, err)
		}
		if err := os.WriteFile(dst, []byte(ccRead(t, runnable)), 0o700); err != nil { //nolint:gosec // test fixture
			t.Fatalf("write %s: %v", dst, err)
		}
	}
	body := ccRead(t, ccAbs(t, agentRunSetPath))
	const from = "harness_dir=/usr/local/lib/wardyn/harness"
	if !strings.Contains(body, from) {
		t.Fatalf("agent-run-set no longer sets %s", from)
	}
	set = filepath.Join(t.TempDir(), "agent-run")
	if err := os.WriteFile(set, []byte(strings.Replace(body, from, "harness_dir="+harnessDir, 1)), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write agent-run-set: %v", err)
	}
	// codex login writes auth.json the way the real one does; any other call is
	// recorded with the seed's argv. claude only records.
	fakes := map[string]string{
		"codex": `#!/bin/sh
if [ "$1" = login ]; then
  mkdir -p "${CODEX_HOME:-$HOME/.codex}"
  cat > "${CODEX_HOME:-$HOME/.codex}/auth.json"
  exit 0
fi
printf 'codex %s\n' "$*" >> "$HOME/calls.log"
`,
		"claude": "#!/bin/sh\nprintf 'claude %s\\n' \"$*\" >> \"$HOME/calls.log\"\n",
	}
	for name, script := range fakes {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o700); err != nil { //nolint:gosec // test fixture
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	return set, home, binDir, tmuxLog
}

func runSet(t *testing.T, set, home, binDir, tmuxLog string, timeout string, args []string, env ...string) (int, string) {
	t.Helper()
	cmd := exec.Command("timeout", append([]string{timeout, "bash", set}, args...)...)
	cmd.Env = append(os.Environ(), append([]string{
		"HOME=" + home,
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"TMUX_LOG=" + tmuxLog,
		"WARDYN_HARNESSES=claude-code codex-cli",
		"WARDYN_REPOS=",
		"WARDYN_REPO_URL=",
		"WARDYN_MITM_CA_PEM=",
		"WARDYN_GITHUB_GRANT_ID=",
		"WARDYN_CLAUDE_MANAGED_B64=",
		"WARDYN_CODEX_BASE_URL=http://proxy.invalid/wardyn/llm/openai/v1",
		"CODEX_API_KEY=wardyn-proxy-injected",
	}, env...)...)
	b, err := cmd.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), string(b)
	}
	if err != nil {
		t.Fatalf("running agent-run-set: %v\noutput: %s", err, b)
	}
	return 0, string(b)
}

// TestHarnessSetImage_IdlePreparesEveryHarness — `agent-run --idle` on the set
// image configures the harness it does NOT start too: a Codex started by hand in
// a Claude-start run needs its config.toml and auth.json, because 0.149.1 reads
// neither OPENAI_* variable. And the boot seed runs in the START harness's CLI.
func TestHarnessSetImage_IdlePreparesEveryHarness(t *testing.T) {
	set, home, binDir, tmuxLog := setImageLayout(t)
	code, out := runSet(t, set, home, binDir, tmuxLog, "3s", []string{"--idle"},
		"WARDYN_START_HARNESS=claude-code", ccSeedEnv)
	if code != 124 {
		t.Fatalf("agent-run --idle exited %d (output: %s), want 124 — it must hold the container open", code, out)
	}
	cfg := filepath.Join(home, ".codex")
	if got := ccRead(t, filepath.Join(cfg, "config.toml")); !strings.Contains(got, `openai_base_url = "http://proxy.invalid/wardyn/llm/openai/v1"`) {
		t.Errorf("codex config.toml lacks the route; --prepare-only never ran for the non-start harness\nconfig:\n%s\noutput:\n%s", got, out)
	}
	if got := ccRead(t, filepath.Join(cfg, "auth.json")); got != "wardyn-proxy-injected" {
		t.Errorf("codex auth.json = %q, want the inert sentinel\noutput:\n%s", got, out)
	}
	if n := ccCount(ccRead(t, tmuxLog), "argv: new-session -d -s wardyn agent-run --boot-seed"); n != 1 {
		t.Errorf("the start harness's --idle created %d boot sessions, want 1\ntmux log:\n%s", n, ccRead(t, tmuxLog))
	}
	if _, err := os.Stat(filepath.Join(home, ".wardyn", "prep-done")); err != nil {
		t.Errorf("the start harness never finished the shared prep: %v", err)
	}

	// The boot pane reaches the set launcher as `agent-run --boot-seed`: it must
	// run the seed in the START harness's CLI and in no other.
	code, out = runSet(t, set, home, binDir, tmuxLog, "10s", []string{"--boot-seed"},
		"WARDYN_START_HARNESS=codex-cli", "WARDYN_INTERACTIVE_START=agent", "WARDYN_RECORDING=", ccSeedEnv)
	if code != 0 {
		t.Fatalf("agent-run --boot-seed exited %d (output: %s)", code, out)
	}
	if calls := ccRead(t, filepath.Join(home, "calls.log")); !strings.Contains(calls, "codex") ||
		!strings.Contains(calls, "say hello in five words") || strings.Contains(calls, "claude") {
		t.Errorf("the boot seed must run in codex alone, calls:\n%s", calls)
	}
}

// TestHarnessSetImage_IdleWithNoStartHarness — a run that starts no CLI (Your own tools: the start
// harness is set but empty) is not refused as "start harness ” is not one of this run's harnesses":
// --idle holds the container open and every harness is prepared.
func TestHarnessSetImage_IdleWithNoStartHarness(t *testing.T) {
	set, home, binDir, tmuxLog := setImageLayout(t)
	code, out := runSet(t, set, home, binDir, tmuxLog, "3s", []string{"--idle"}, "WARDYN_START_HARNESS=", ccSeedEnv)
	if code != 124 {
		t.Fatalf("agent-run --idle with an empty start harness exited %d (output: %s), want 124", code, out)
	}
	if got := ccRead(t, filepath.Join(home, ".codex", "config.toml")); !strings.Contains(got, "openai_base_url") {
		t.Errorf("codex was not prepared when no harness starts\noutput:\n%s", out)
	}
}
