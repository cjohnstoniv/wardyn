// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/agentpolicy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// Every managed file a harness reads lands where that harness reads it on macOS, with its mode and owner.
func TestMacGuestFilesMapsEveryManagedFile(t *testing.T) {
	cases := map[string]string{
		agentpolicy.ClaudeCodeManagedSettingsPath:                  "/Library/Application Support/ClaudeCode/managed-settings.json",
		"/etc/claude-code/managed-mcp.json":                        "/Library/Application Support/ClaudeCode/managed-mcp.json",
		"/etc/claude-code/wardyn-mcp.json":                         "/Library/Application Support/ClaudeCode/wardyn-mcp.json",
		agentpolicy.CodexRequirementsPath:                          "/etc/codex/requirements.toml",
		runner.ComponentSecretDir + "/wardyn-toolgate-claude-code": "/var/run/wardyn/secrets/wardyn-toolgate-claude-code",
	}
	for linux, mac := range cases {
		f := runner.ManagedFile{Path: linux, Mode: 0o644, Content: []byte("x")}
		if strings.HasPrefix(linux, runner.ComponentSecretDir) {
			f.Mode, f.AgentOwned = runner.ComponentSecretFileMode, true
		}
		got, err := macGuestFiles([]runner.ManagedFile{f})
		if err != nil {
			t.Fatalf("%s: %v", linux, err)
		}
		want := guestagent.File{Path: mac, Mode: f.Mode, AgentOwned: f.AgentOwned, Content: f.Content}
		if len(got) != 1 || got[0].Path != want.Path || got[0].Mode != want.Mode || got[0].AgentOwned != want.AgentOwned || string(got[0].Content) != "x" {
			t.Errorf("%s -> %+v, want %+v", linux, got, want)
		}
	}
}

// Unmapped -> refused: a managed file the table does not know refuses the run with a plain reason; it is
// never dropped and never written at its Linux path.
func TestMacGuestFilesRefusesAnUnmappedFile(t *testing.T) {
	for _, p := range []string{"/etc/claude-code/managed-settings.d/10-org.json", "/etc/claude-code/other.json", "/etc/codex/config.toml"} {
		files := []runner.ManagedFile{{Path: agentpolicy.ClaudeCodeManagedSettingsPath}, {Path: p}}
		got, err := macGuestFiles(files)
		if err == nil {
			t.Fatalf("%s was forwarded: %+v", p, got)
		}
		if want := "This run was not started: its managed file " + p + " has no place in a macOS VM sandbox, and the run never starts without it."; err.Error() != want {
			t.Errorf("refusal %q, want %q", err, want)
		}
	}
}

// Path-valued env moves with the files: Bedrock's AWS config under /Users/agent, a secret's path under
// /var/run/wardyn, each path in a list or a JSON value, and nothing that only looks like one.
func TestMacGuestEnvTranslatesPaths(t *testing.T) {
	env := map[string]string{
		"AWS_CONFIG_FILE":             "/home/agent/.aws/config",
		"AWS_SHARED_CREDENTIALS_FILE": "/home/agent/.aws/credentials",
		"CLAUDE_CONFIG_DIR":           "/home/agent/.claude-run",
		"WARDYN_EPHEMERAL_DIRS":       "/home/agent/work,/home/agent/.cache",
		"WARDYN_REPOS":                `[{"url":"https://x/r","target":"/home/agent/work/r"}]`,
		"WARDYN_USER_DRIVE":           "/home/agent/drive:rw",
		"SECRET_AT":                   "/run/wardyn/secrets/token",
		"HOME_ITSELF":                 "/home/agent",
		"NOT_A_PATH":                  "https://example.com/home/agent/x",
		"LONGER_NAME":                 "/opt/home/agentx",
		"FOO":                         "bar",
	}
	if err := macGuestEnv(env); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"AWS_CONFIG_FILE":             "/Users/agent/.aws/config",
		"AWS_SHARED_CREDENTIALS_FILE": "/Users/agent/.aws/credentials",
		"CLAUDE_CONFIG_DIR":           "/Users/agent/.claude-run",
		"WARDYN_EPHEMERAL_DIRS":       "/Users/agent/work,/Users/agent/.cache",
		"WARDYN_REPOS":                `[{"url":"https://x/r","target":"/Users/agent/work/r"}]`,
		"WARDYN_USER_DRIVE":           "/Users/agent/drive:rw",
		"SECRET_AT":                   "/var/run/wardyn/secrets/token",
		"HOME_ITSELF":                 "/Users/agent",
		"NOT_A_PATH":                  "https://example.com/home/agent/x",
		"LONGER_NAME":                 "/opt/home/agentx",
		"FOO":                         "bar",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
}

// A /home or /run path the table cannot place refuses the run, naming the variable.
func TestMacGuestEnvRefusesAnUnmappedPath(t *testing.T) {
	for _, v := range []string{"/run/other/x", "/home/someone/.aws/config", "/home/agentx/y"} {
		err := macGuestEnv(map[string]string{"SOME_FILE": v})
		if want := "This run was not started: SOME_FILE names " + v + ", a path a macOS VM sandbox does not have."; err == nil || err.Error() != want {
			t.Errorf("%s: err %v, want %q", v, err, want)
		}
	}
}

// A managed file with no mode is written at the default one, never at 0000 (the guest writes exactly the
// mode it is handed), and the launcher is told what to check before the run starts.
func TestMacGuestFilesResolveTheModeAndNameThemForTheCheck(t *testing.T) {
	got, err := macGuestFiles([]runner.ManagedFile{
		{Path: agentpolicy.ClaudeCodeManagedSettingsPath},
		{Path: runner.ComponentSecretDir + "/tok", Mode: runner.ComponentSecretFileMode, AgentOwned: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Mode != runner.DefaultManagedFileMode {
		t.Fatalf("mode %04o, want %04o", got[0].Mode, runner.DefaultManagedFileMode)
	}
	want := `[{"path":"/Library/Application Support/ClaudeCode/managed-settings.json","mode":420},{"path":"/var/run/wardyn/secrets/tok","mode":256,"agent_owned":true}]`
	if env := macGuestFilesEnv(got); env != want {
		t.Errorf("env %s, want %s", env, want)
	}
	if macGuestFilesEnv(nil) != "" {
		t.Error("no files still named a check")
	}
}
