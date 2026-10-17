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

// codex 0.149.1 reads neither OPENAI_BASE_URL nor OPENAI_API_KEY: its base URL
// comes only from config.toml `openai_base_url`, task mode authenticates from
// CODEX_API_KEY, and the interactive tool reads auth.json, which
// `codex login --with-api-key` writes. agent-run's prepare_codex_config turns
// the two variables dispatch sets (WARDYN_CODEX_BASE_URL, CODEX_API_KEY) into
// those. PURE-SHELL tests over the REAL agent-run, like the boot tests beside
// them; the only fake is a `codex` that records what it was asked to do.

const cxSentinel = "wardyn-proxy-injected"

// cxFakeCodex installs a `codex` in a fresh bin dir. `login --with-api-key`
// writes the key it read on stdin to auth.json under $CODEX_HOME (default
// ~/.codex) and logs the call; `exec` logs the config.toml it would have read.
func cxFakeCodex(t *testing.T) (home, binDir, codexLog string) {
	t.Helper()
	home, binDir, _ = ccIdleEnv(t)
	codexLog = filepath.Join(filepath.Dir(home), "codex.log")
	fake := `#!/bin/sh
dir="${CODEX_HOME:-$HOME/.codex}"
case "$1 $2" in
  "login --with-api-key")
    key="$(cat)"
    mkdir -p "$dir"
    printf '{"OPENAI_API_KEY":"%s"}' "$key" > "$dir/auth.json"
    printf 'login stdin=%s\n' "$key" >> "$CODEX_LOG" ;;
  exec*)
    printf 'exec saw config: %s\n' "$(cat "$dir/config.toml" 2>/dev/null | tr '\n' '|')" >> "$CODEX_LOG" ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte(fake), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fake codex: %v", err)
	}
	return home, binDir, codexLog
}

// cxRunTask runs the real `agent-run <task>` to completion (the fake codex
// returns at once). WARDYN_CODEX_BASE_URL and CODEX_API_KEY start empty, so a
// test sets only the ones it means to.
func cxRunTask(t *testing.T, home, binDir, codexLog string, extraEnv ...string) string {
	t.Helper()
	cmd := exec.Command("timeout", "20s", "bash", cxRunnableAgentRun(t), "a task")
	cmd.Env = append(os.Environ(), append([]string{
		"HOME=" + home,
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"CODEX_LOG=" + codexLog,
		"CODEX_HOME=",
		"WARDYN_CODEX_BASE_URL=",
		"CODEX_API_KEY=",
		"WARDYN_REPOS=",
		"WARDYN_REPO_URL=",
		"WARDYN_MITM_CA_PEM=",
		"WARDYN_GITHUB_GRANT_ID=",
	}, extraEnv...)...)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("agent-run: %v\noutput: %s", err, b)
	}
	return string(b)
}

func cxWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestCodexAgentRun_NeitherVariableLeavesCodexConfigAlone — NEGATIVE CONTROL:
// a run dispatch gave neither variable (an older control plane, a non-model
// run) must keep the person's own config.toml and auth.json byte for byte, and
// never call `codex login`.
func TestCodexAgentRun_NeitherVariableLeavesCodexConfigAlone(t *testing.T) {
	home, binDir, codexLog := cxFakeCodex(t)
	const cfg = "model = \"x\"\n\n[projects.\"/home/agent/work\"]\ntrust_level = \"trusted\"\n"
	const auth = `{"OPENAI_API_KEY":"theirs"}`
	cxWrite(t, filepath.Join(home, ".codex", "config.toml"), cfg)
	cxWrite(t, filepath.Join(home, ".codex", "auth.json"), auth)

	cxRunTask(t, home, binDir, codexLog)

	if got := ccRead(t, filepath.Join(home, ".codex", "config.toml")); got != cfg {
		t.Errorf("config.toml changed with neither variable set:\n%s", got)
	}
	if got := ccRead(t, filepath.Join(home, ".codex", "auth.json")); got != auth {
		t.Errorf("auth.json changed with neither variable set: %s", got)
	}
	if log := ccRead(t, codexLog); strings.Contains(log, "login") {
		t.Errorf("codex login ran with CODEX_API_KEY unset:\n%s", log)
	}
}

// TestCodexAgentRun_BaseURLGoesBeforeFirstTableOnce — the base URL must be a
// TOP-LEVEL key. Appended at the end it lands inside the last
// `[projects."<path>"]` table codex wrote, where codex reads it as that table's
// key. A second run (a revive) must replace the line, not add another, and a
// stale top-level one must go.
func TestCodexAgentRun_BaseURLGoesBeforeFirstTableOnce(t *testing.T) {
	home, binDir, codexLog := cxFakeCodex(t)
	cfgPath := filepath.Join(home, ".codex", "config.toml")
	cxWrite(t, cfgPath, "model = \"x\"\nopenai_base_url = \"http://stale/\"\n\n[projects.\"/home/agent/work\"]\ntrust_level = \"trusted\"\n")
	const u = "http://wardyn-proxy:3128/wardyn/llm/openai/v1"

	cxRunTask(t, home, binDir, codexLog, "WARDYN_CODEX_BASE_URL="+u)
	cxRunTask(t, home, binDir, codexLog, "WARDYN_CODEX_BASE_URL="+u)

	got := ccRead(t, cfgPath)
	want := "model = \"x\"\n\nopenai_base_url = \"" + u + "\"\n[projects.\"/home/agent/work\"]\ntrust_level = \"trusted\"\n"
	if got != want {
		t.Errorf("config.toml after two runs:\n%s\nwant:\n%s", got, want)
	}
	if n := strings.Count(got, "openai_base_url"); n != 1 {
		t.Errorf("openai_base_url appears %d times after a second run, want 1", n)
	}
	if !strings.Contains(ccRead(t, codexLog), "openai_base_url = \""+u+"\"") {
		t.Errorf("codex exec did not see the base URL; prep must finish before the agent starts:\n%s", ccRead(t, codexLog))
	}
}

// TestCodexAgentRun_ModelKeptWhenOnlyBaseURLSet — codex 0.149.1 persists the
// model picker's choice as a top-level `model =`; a run that sets only the base
// URL must not touch it. Also the no-file shape: the first run creates the file.
func TestCodexAgentRun_ModelKeptWhenOnlyBaseURLSet(t *testing.T) {
	home, binDir, codexLog := cxFakeCodex(t)
	cfgPath := filepath.Join(home, ".codex", "config.toml")
	cxWrite(t, cfgPath, "model = \"x\"\n")
	cxRunTask(t, home, binDir, codexLog, "WARDYN_CODEX_BASE_URL=http://p/wardyn/llm/openai/v1")
	if got, want := ccRead(t, cfgPath), "model = \"x\"\nopenai_base_url = \"http://p/wardyn/llm/openai/v1\"\n"; got != want {
		t.Errorf("config.toml = %q, want %q", got, want)
	}

	home2, binDir2, codexLog2 := cxFakeCodex(t)
	cxRunTask(t, home2, binDir2, codexLog2, "WARDYN_CODEX_BASE_URL=http://p/wardyn/llm/openai/v1")
	if got, want := ccRead(t, filepath.Join(home2, ".codex", "config.toml")), "openai_base_url = \"http://p/wardyn/llm/openai/v1\"\n"; got != want {
		t.Errorf("fresh config.toml = %q, want %q", got, want)
	}
}

// TestCodexAgentRun_SentinelLoginWritesOnlyTheSentinel — the interactive tool
// reads auth.json, written by `codex login --with-api-key` from stdin: the
// inert sentinel and nothing else, under CODEX_HOME when it is set. With no
// base URL, config.toml is not created.
func TestCodexAgentRun_SentinelLoginWritesOnlyTheSentinel(t *testing.T) {
	home, binDir, codexLog := cxFakeCodex(t)
	codexHome := filepath.Join(home, "elsewhere")
	cxRunTask(t, home, binDir, codexLog, "CODEX_API_KEY="+cxSentinel, "CODEX_HOME="+codexHome)

	if got, want := ccRead(t, filepath.Join(codexHome, "auth.json")), `{"OPENAI_API_KEY":"`+cxSentinel+`"}`; got != want {
		t.Errorf("auth.json = %s, want %s", got, want)
	}
	if !strings.Contains(ccRead(t, codexLog), "login stdin="+cxSentinel+"\n") {
		t.Errorf("codex login did not receive the sentinel on stdin:\n%s", ccRead(t, codexLog))
	}
	if _, err := os.Stat(filepath.Join(codexHome, "config.toml")); err == nil {
		t.Error("config.toml was created with no base URL set")
	}
}

// TestCodexAgentRun_IdlePrepWritesCodexConfig — an interactive run takes the
// --idle path, not task mode; the same prep must run there, before prep-done.
func TestCodexAgentRun_IdlePrepWritesCodexConfig(t *testing.T) {
	home, binDir, codexLog := cxFakeCodex(t)
	tmuxLog := filepath.Join(filepath.Dir(home), "tmux.log")
	const u = "http://wardyn-proxy:3128/wardyn/llm/openai/v1"
	code, out := cxRunIdle(t, home, binDir, tmuxLog, "CODEX_LOG="+codexLog, "CODEX_HOME=", "WARDYN_CODEX_BASE_URL="+u, "CODEX_API_KEY="+cxSentinel)
	if code != 124 {
		t.Fatalf("agent-run --idle exited %d (output: %s), want 124", code, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".wardyn", "prep-done")); err != nil {
		t.Fatalf("idle prep never finished: %v", err)
	}
	if got, want := ccRead(t, filepath.Join(home, ".codex", "config.toml")), "openai_base_url = \""+u+"\"\n"; got != want {
		t.Errorf("config.toml = %q, want %q", got, want)
	}
	if got, want := ccRead(t, filepath.Join(home, ".codex", "auth.json")), `{"OPENAI_API_KEY":"`+cxSentinel+`"}`; got != want {
		t.Errorf("auth.json = %s, want %s", got, want)
	}
}
