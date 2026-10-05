// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// An azure_foundry run on the Responses harness pins its deployment with WARDYN_CODEX_MODEL. codex
// reads the model from config.toml, and -m is passed at the two argv sites only when the variable is
// set, so every other codex run is byte-for-byte what it was. PURE-SHELL tests over the REAL agent-run,
// like the config and boot tests beside them; the only fake is a `codex` that records its argv.

const cxModel = "deploy-responses"

// cxArgvCodex installs a `codex` that logs its argv (one line per call) and swallows the sentinel login.
func cxArgvCodex(t *testing.T) (home, binDir, codexLog string) {
	t.Helper()
	home, binDir, _ = ccIdleEnv(t)
	codexLog = filepath.Join(filepath.Dir(home), "codex.log")
	fake := "#!/bin/sh\ncase \"$1\" in login) cat >/dev/null ;; *) printf 'argv: %s\\n' \"$*\" >> \"$CODEX_LOG\" ;; esac\n"
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte(fake), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fake codex: %v", err)
	}
	return home, binDir, codexLog
}

// With HOME a fresh directory and CODEX_HOME unset, prep writes both keys into config.toml, at the top
// level and once: a person's own `model =` is replaced, not duplicated, and the project tables codex
// wrote stay where they were.
func TestCodexAgentRun_BaseURLAndModelBothLandInConfigToml(t *testing.T) {
	home, binDir, codexLog := cxArgvCodex(t)
	cfgPath := filepath.Join(home, ".codex", "config.toml")
	cxWrite(t, cfgPath, "model = \"theirs\"\n\n[projects.\"/home/agent/work\"]\ntrust_level = \"trusted\"\n")
	const u = "https://foundry.test/openai/v1"

	cxRunTask(t, home, binDir, codexLog, "WARDYN_CODEX_BASE_URL="+u, "WARDYN_CODEX_MODEL="+cxModel)
	cxRunTask(t, home, binDir, codexLog, "WARDYN_CODEX_BASE_URL="+u, "WARDYN_CODEX_MODEL="+cxModel) // a revive

	got := ccRead(t, cfgPath)
	for _, line := range []string{"openai_base_url = \"" + u + "\"", "model = \"" + cxModel + "\""} {
		if n := strings.Count(got, line+"\n"); n != 1 {
			t.Errorf("config.toml has %d copies of %q after two runs, want 1:\n%s", n, line, got)
		}
		if strings.Index(got, line) > strings.Index(got, "[projects") {
			t.Errorf("%q landed after the first table, where codex reads it as that table's key:\n%s", line, got)
		}
	}
	if strings.Contains(got, "theirs") || !strings.Contains(got, "trust_level = \"trusted\"") {
		t.Errorf("the person's model was kept or their table was lost:\n%s", got)
	}
}

// Unset, the variable changes nothing: no model line is written and a person's own survives.
func TestCodexAgentRun_ModelUnsetLeavesTheirModelAlone(t *testing.T) {
	home, binDir, codexLog := cxArgvCodex(t)
	cfgPath := filepath.Join(home, ".codex", "config.toml")
	cxWrite(t, cfgPath, "model = \"theirs\"\n")
	cxRunTask(t, home, binDir, codexLog, "WARDYN_CODEX_BASE_URL=http://p/wardyn/llm/openai/v1")
	if got, want := ccRead(t, cfgPath), "model = \"theirs\"\nopenai_base_url = \"http://p/wardyn/llm/openai/v1\"\n"; got != want {
		t.Errorf("config.toml = %q, want %q", got, want)
	}
}

// Task mode: `codex exec` gets -m with the variable set, and no -m at all without it.
func TestCodexAgentRun_TaskModeArgvCarriesModelOnlyWhenSet(t *testing.T) {
	for name, tc := range map[string]struct {
		env  []string
		want string
	}{
		"unset": {nil, "argv: exec a task\n"},
		"empty": {[]string{"WARDYN_CODEX_MODEL="}, "argv: exec a task\n"},
		"set":   {[]string{"WARDYN_CODEX_MODEL=" + cxModel}, "argv: exec -m " + cxModel + " a task\n"},
	} {
		t.Run(name, func(t *testing.T) {
			home, binDir, codexLog := cxArgvCodex(t)
			cxRunTask(t, home, binDir, codexLog, tc.env...)
			got := ccRead(t, codexLog)
			if got != tc.want {
				t.Errorf("codex argv = %q, want %q", got, tc.want)
			}
			if tc.env == nil && strings.Contains(got, " -m") {
				t.Errorf("a run with WARDYN_CODEX_MODEL unset passed -m: %q", got)
			}
		})
	}
}

// Boot seed: the seeded interactive start passes -m only when the variable is set. Prep is already done,
// so the pane starts at once.
func TestCodexAgentRun_BootSeedArgvCarriesModelOnlyWhenSet(t *testing.T) {
	for name, tc := range map[string]struct {
		env  []string
		want string
	}{
		"unset": {nil, "argv: say hello in five words\n"},
		"set":   {[]string{"WARDYN_CODEX_MODEL=" + cxModel}, "argv: -m " + cxModel + " say hello in five words\n"},
	} {
		t.Run(name, func(t *testing.T) {
			home, binDir, codexLog := cxArgvCodex(t)
			work := filepath.Join(home, "work")
			cxWrite(t, filepath.Join(home, ".wardyn", "workdir"), work+"\n")
			cxWrite(t, filepath.Join(home, ".wardyn", "prep-done"), "")
			if err := os.MkdirAll(work, 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("timeout", "20s", "bash", cxRunnableAgentRun(t), "--boot-seed")
			cmd.Env = append(os.Environ(), append([]string{
				"HOME=" + home,
				"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
				"CODEX_LOG=" + codexLog,
				"CODEX_HOME=",
				"WARDYN_CODEX_MODEL=",
				"WARDYN_INTERACTIVE_START=agent",
				"WARDYN_SEED_AUTO_TOOLS=",
				ccSeedEnv,
				"WARDYN_RECORDING=",
				"WARDYN_GIT_HELPER_SECRET=",
				"WARDYN_IDLE_PID=" + strconv.Itoa(os.Getpid()),
			}, tc.env...)...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("agent-run --boot-seed: %v\noutput: %s", err, out)
			}
			if got := ccRead(t, codexLog); got != tc.want {
				t.Errorf("codex argv = %q, want %q", got, tc.want)
			}
		})
	}
}
