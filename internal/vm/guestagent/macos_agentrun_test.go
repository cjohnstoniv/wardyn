// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package guestagent

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// launchRig is one launch with its process boundary recorded: what it would exec, every helper it ran,
// and the folder it moved to.
type launchRig struct {
	a        *AgentRun
	home     string
	execPath string
	execArgv []string
	execEnv  []string
	ran      [][]string
	dir      string
	stderr   bytes.Buffer
	stdout   bytes.Buffer
	// fail makes a helper whose argv starts with this fail.
	fail []string
	// onRun runs for each helper, as the helper's side effect.
	onRun func(dir string, argv []string)
}

func newLaunchRig(t *testing.T, env map[string]string) *launchRig {
	t.Helper()
	r := &launchRig{home: t.TempDir()}
	full := map[string]string{"HOME": r.home, "PATH": "/usr/bin:/bin"}
	for k, v := range env {
		full[k] = v
	}
	r.a = &AgentRun{Env: full, Stdout: &r.stdout, Stderr: &r.stderr,
		Exec: func(path string, argv, env []string) error {
			r.execPath, r.execArgv, r.execEnv = path, argv, env
			return nil
		},
		Run: func(dir string, argv, env []string, stdin string) (string, error) {
			r.ran = append(r.ran, argv)
			if r.onRun != nil {
				r.onRun(dir, argv)
			}
			if r.fail != nil && len(argv) >= len(r.fail) && slices.Equal(argv[:len(r.fail)], r.fail) {
				return "boom", errors.New("exit status 1")
			}
			return "", nil
		},
		Chdir: func(dir string) error { r.dir = dir; return nil },
	}
	return r
}

func (r *launchRig) env(k string) (string, bool) {
	for _, kv := range r.execEnv {
		if key, v, _ := strings.Cut(kv, "="); key == k {
			return v, true
		}
	}
	return "", false
}

func (r *launchRig) ranArgv(argv ...string) bool {
	return slices.ContainsFunc(r.ran, func(a []string) bool { return slices.Equal(a, argv) })
}

// A command workload (WARDYN_TASK_MODE=exec) runs as `/bin/sh -c` in the work folder, with no harness.
func TestAgentRunExecMode(t *testing.T) {
	r := newLaunchRig(t, map[string]string{"WARDYN_TASK_MODE": "exec"})
	if code := r.a.Main([]string{"make test"}); code != 0 {
		t.Fatalf("exit %d: %s", code, r.stderr.String())
	}
	if r.execPath != "/bin/sh" || !slices.Equal(r.execArgv, []string{"/bin/sh", "-c", "make test"}) {
		t.Fatalf("exec %s %q", r.execPath, r.execArgv)
	}
	if r.dir != filepath.Join(r.home, "work") {
		t.Errorf("started in %s", r.dir)
	}
}

// An agent_task run starts Claude Code as claude-code/agent-run's autonomous lane does, wired for its auth
// mode.
func TestAgentRunClaudeAuthModes(t *testing.T) {
	for _, tc := range []struct {
		name, mode   string
		env          map[string]string
		creds        bool
		wantBase     string // "" = unset
		wantPlaceKey bool
	}{
		{name: "apikey", mode: "apikey", env: map[string]string{"ANTHROPIC_BASE_URL": "http://wardyn-proxy:3128/wardyn/llm/anthropic"},
			wantBase: "http://wardyn-proxy:3128/wardyn/llm/anthropic", wantPlaceKey: true},
		{name: "bedrock", mode: "bedrock", env: map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", "ANTHROPIC_BASE_URL": "http://x"}},
		{name: "subscription default", mode: "subscription", creds: true, env: map[string]string{"ANTHROPIC_BASE_URL": "https://api.anthropic.com"}},
		{name: "subscription gateway", mode: "subscription", creds: true, env: map[string]string{"ANTHROPIC_BASE_URL": "https://gw.internal"},
			wantBase: "https://gw.internal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"WARDYN_HARNESSES": "claude-code", "WARDYN_START_HARNESS": "claude-code"}
			for k, v := range tc.env {
				env[k] = v
			}
			r := newLaunchRig(t, env)
			if tc.creds {
				cfg := filepath.Join(r.home, ".claude-run")
				r.a.Env["CLAUDE_CONFIG_DIR"] = cfg
				if err := os.MkdirAll(cfg, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cfg, ".credentials.json"), []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if code := r.a.Main([]string{"fix the bug"}); code != 0 {
				t.Fatalf("exit %d: %s", code, r.stderr.String())
			}
			want := []string{"claude", "-p", "fix the bug", "--output-format", "text", "--dangerously-skip-permissions"}
			if r.execPath != "/usr/local/bin/claude" || !slices.Equal(r.execArgv, want) {
				t.Fatalf("exec %s %q", r.execPath, r.execArgv)
			}
			if !strings.Contains(r.stderr.String(), "anthropic auth mode = "+tc.mode) {
				t.Errorf("mode: %s", r.stderr.String())
			}
			if base, _ := r.env("ANTHROPIC_BASE_URL"); base != tc.wantBase {
				t.Errorf("ANTHROPIC_BASE_URL = %q, want %q", base, tc.wantBase)
			}
			if key, _ := r.env("ANTHROPIC_API_KEY"); (key == "wardyn-proxy-injected") != tc.wantPlaceKey {
				t.Errorf("ANTHROPIC_API_KEY = %q", key)
			}
			if _, err := os.Stat(filepath.Join(r.home, ".claude.json")); err != nil {
				t.Errorf("onboarding not seeded: %v", err)
			}
		})
	}
}

// The run's MCP servers reach an autonomous Claude Code run under --strict-mcp-config, from the file Init
// wrote at its macOS path.
func TestAgentRunClaudeMCPConfig(t *testing.T) {
	f := filepath.Join(t.TempDir(), "wardyn-mcp.json")
	if err := os.WriteFile(f, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := macClaudeMCPConfig
	macClaudeMCPConfig = f
	t.Cleanup(func() { macClaudeMCPConfig = old })
	r := newLaunchRig(t, map[string]string{"WARDYN_HARNESSES": "claude-code"})
	if code := r.a.Main([]string{"t"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if want := []string{"--mcp-config", f, "--strict-mcp-config"}; !slices.Equal(r.execArgv[len(r.execArgv)-3:], want) {
		t.Fatalf("argv %q", r.execArgv)
	}
}

// Codex runs as codex-cli/agent-run starts it: bypass by default; bound to the run's sandbox mode, after
// its own sandbox is shown to start, when the policy names one.
func TestAgentRunCodex(t *testing.T) {
	r := newLaunchRig(t, map[string]string{"WARDYN_HARNESSES": "codex-cli", "WARDYN_CODEX_MODEL": "gpt-x"})
	if code := r.a.Main([]string{"t"}); code != 0 {
		t.Fatalf("exit %d: %s", code, r.stderr.String())
	}
	if want := []string{"codex", "exec", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", "-m", "gpt-x", "t"}; r.execPath != "/usr/local/bin/codex" || !slices.Equal(r.execArgv, want) {
		t.Fatalf("exec %s %q", r.execPath, r.execArgv)
	}

	r = newLaunchRig(t, map[string]string{"WARDYN_HARNESSES": "codex-cli", "WARDYN_HARNESS_SANDBOX": "workspace-write"})
	if code := r.a.Main([]string{"t"}); code != 0 {
		t.Fatalf("exit %d: %s", code, r.stderr.String())
	}
	if !r.ranArgv("/usr/local/bin/codex", "sandbox", "-c", `sandbox_mode="workspace-write"`, "-c", "sandbox_workspace_write.network_access=true", "--", "true") {
		t.Errorf("no sandbox probe: %q", r.ran)
	}
	if want := []string{"codex", "exec", "--skip-git-repo-check", "-s", "workspace-write", "-c", `approval_policy="never"`, "-c", "sandbox_workspace_write.network_access=true", "t"}; !slices.Equal(r.execArgv, want) {
		t.Fatalf("argv %q", r.execArgv)
	}

	r = newLaunchRig(t, map[string]string{"WARDYN_HARNESSES": "codex-cli", "WARDYN_HARNESS_SANDBOX": "read-only"})
	r.fail = []string{"/usr/local/bin/codex", "sandbox"}
	if code := r.a.Main([]string{"t"}); code != 1 || r.execArgv != nil {
		t.Fatalf("a failed sandbox probe ran the task: exit %d, exec %q", code, r.execArgv)
	}
}

// Codex's config gets its route, model and MCP servers; the sentinel key logs it in on stdin.
func TestAgentRunCodexConfig(t *testing.T) {
	r := newLaunchRig(t, map[string]string{"WARDYN_HARNESSES": "codex-cli", "WARDYN_CODEX_BASE_URL": "http://wardyn-proxy:3128/wardyn/llm/openai/v1",
		"WARDYN_MCP_SERVERS_JSON": `{"linear":"http://wardyn-proxy:3128/wardyn/mcp/tok/linear"}`, "CODEX_API_KEY": "sentinel"})
	dir := filepath.Join(r.home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("openai_base_url = \"old\"\n[mcp_servers.linear]\nurl = \"old\"\n[other]\nx = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := r.a.Main([]string{"t"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	want := "openai_base_url = \"http://wardyn-proxy:3128/wardyn/llm/openai/v1\"\n[other]\nx = 1\n[mcp_servers.linear]\nurl = \"http://wardyn-proxy:3128/wardyn/mcp/tok/linear\"\n"
	if string(b) != want {
		t.Errorf("config.toml\n%s\nwant\n%s", b, want)
	}
	if !r.ranArgv("/usr/local/bin/codex", "login", "--with-api-key") {
		t.Errorf("no login: %q", r.ran)
	}
}

// What the guest does not serve is refused with exit 64 and a sentence, before anything is prepared.
func TestAgentRunRefusals(t *testing.T) {
	for _, tc := range []struct {
		args []string
		env  map[string]string
		want string
	}{
		{[]string{"--tasks", "claude-code", "a", "codex-cli", "b"}, nil, agentRunTasksRefused},
		{[]string{"--idle"}, nil, agentRunInteractiveRefused},
		{[]string{"t"}, map[string]string{"WARDYN_TASK_MODE": "exec", "WARDYN_SERVICE": "1"}, agentRunServiceRefused},
		{[]string{"t"}, map[string]string{"WARDYN_TOOL_APPROVALS": "hold"}, agentRunHoldRefused},
		{[]string{"t"}, map[string]string{"WARDYN_SCAN_ONLY": "1"}, agentRunScanRefused},
	} {
		env := map[string]string{"WARDYN_HARNESSES": "claude-code", "WARDYN_REPO_URL": "https://github.com/o/r"}
		for k, v := range tc.env {
			env[k] = v
		}
		r := newLaunchRig(t, env)
		if code := r.a.Main(tc.args); code != AgentRunRefused {
			t.Errorf("%q: exit %d", tc.args, code)
		}
		if r.stderr.String() != "agent-run: "+tc.want+"\n" {
			t.Errorf("%q: said %q", tc.args, r.stderr.String())
		}
		if ents, _ := os.ReadDir(r.home); r.execArgv != nil || r.ran != nil || len(ents) > 0 {
			t.Errorf("%q: refused but exec %q, ran %q, home %v", tc.args, r.execArgv, r.ran, ents)
		}
	}
}

func homeRecord(rel, content string) string {
	return rel + "\t" + base64.StdEncoding.EncodeToString([]byte(content))
}

// The files the control plane sends for HOME land under the guest's HOME at their modes; a Linux home in
// their content is the guest's; an unsafe path is skipped.
func TestAgentRunHomeRecords(t *testing.T) {
	r := newLaunchRig(t, map[string]string{"WARDYN_TASK_MODE": "exec",
		"WARDYN_ARTIFACT_CONFIG_B64":  homeRecord(".npmrc", "registry=x") + "\n" + homeRecord("../evil", "x"),
		"WARDYN_AWS_SSO_CONFIG_B64":   homeRecord(".aws/config", "credential_process = cat /home/agent/.aws/wardyn-sigv4.json\n"),
		"WARDYN_KUBECTL_CONFIG_B64":   homeRecord(".kube/config", "k"),
		"WARDYN_AZURE_CLI_CONFIG_B64": homeRecord(".azure/azureProfile.json", "{}"),
	})
	if code := r.a.Main([]string{"true"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for rel, mode := range map[string]os.FileMode{".npmrc": 0o644, ".aws/config": 0o600, ".kube/config": 0o600, ".azure/azureProfile.json": 0o600} {
		st, err := os.Stat(filepath.Join(r.home, rel))
		if err != nil || st.Mode().Perm() != mode {
			t.Errorf("%s: %v %v", rel, st, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(r.home, ".aws/config"))
	if want := "credential_process = cat " + r.home + "/.aws/wardyn-sigv4.json\n"; string(b) != want {
		t.Errorf(".aws/config = %q, want %q", b, want)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(r.home), "evil")); err == nil {
		t.Error("an unsafe record path was written")
	}
}

// git reaches the run's repos only through the proxy's brokers.
func TestAgentRunGitBrokerRoutes(t *testing.T) {
	r := newLaunchRig(t, map[string]string{"WARDYN_TASK_MODE": "exec", "WARDYN_PROXY_URL": "http://wardyn-proxy:3128/",
		"WARDYN_GITHUB_GRANT_ID": "g", "WARDYN_REPOS": "https://github.com/o/r\t\to/r\t",
		"WARDYN_GIT_PAT_BROKER_HOSTS": "dev.azure.com org@dev.azure.com bad/host"})
	r.fail = []string{"git", "clone"}
	if code := r.a.Main([]string{"true"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range [][]string{
		{"git", "config", "--global", "url.http://wardyn-proxy:3128/wardyn/gh/o/r.insteadOf", "https://github.com/o/r"},
		{"git", "config", "--global", "--add", "url.http://wardyn-proxy:3128/wardyn/gh/o/r.insteadOf", "git@github.com:o/r"},
		{"git", "config", "--global", "--add", "url.http://wardyn-proxy:3128/wardyn/git/dev.azure.com/.insteadOf", "https://dev.azure.com/"},
		{"git", "config", "--global", "--add", "url.http://wardyn-proxy:3128/wardyn/git/dev.azure.com/.insteadOf", "https://org@dev.azure.com/"},
	} {
		if !r.ranArgv(want...) {
			t.Errorf("missing %q in %q", want, r.ran)
		}
	}
	if slices.ContainsFunc(r.ran, func(a []string) bool { return strings.Contains(strings.Join(a, " "), "bad/host") }) {
		t.Error("a malformed broker host became a route")
	}
}

// The run's repo is cloned shallow onto the run branch, and the task starts in its checkout.
func TestAgentRunClonesAndStartsInTheCheckout(t *testing.T) {
	r := newLaunchRig(t, map[string]string{"WARDYN_TASK_MODE": "exec", "WARDYN_RUN_ID": "run1", "WARDYN_REPO_URL": "https://github.com/o/r.git"})
	r.onRun = func(_ string, argv []string) {
		if len(argv) > 2 && argv[1] == "clone" {
			_ = os.MkdirAll(filepath.Join(argv[len(argv)-1], ".git"), 0o755)
		}
	}
	if code := r.a.Main([]string{"true"}); code != 0 {
		t.Fatalf("exit %d: %s", code, r.stderr.String())
	}
	dest := filepath.Join(r.home, "work", "r")
	if !r.ranArgv("git", "clone", "--depth", "1", "--", "https://github.com/o/r.git", dest) {
		t.Errorf("no clone: %q", r.ran)
	}
	if !r.ranArgv("git", "checkout", "-b", "wardyn/run1/work") {
		t.Errorf("no run branch: %q", r.ran)
	}
	if r.dir != dest {
		t.Errorf("started in %s, want %s", r.dir, dest)
	}

	r = newLaunchRig(t, map[string]string{"WARDYN_TASK_MODE": "exec", "WARDYN_START_FOLDER": "/no/such/folder"})
	if code := r.a.Main([]string{"true"}); code != 1 || r.execArgv != nil {
		t.Fatalf("a missing starting folder started elsewhere: exit %d", code)
	}
}

// A run names a harness this base serves, or nothing starts.
func TestAgentRunNeedsAServedHarness(t *testing.T) {
	for _, env := range []map[string]string{
		{},
		{"WARDYN_HARNESSES": "gemini-cli"},
		{"WARDYN_HARNESSES": "claude-code", "WARDYN_START_HARNESS": "codex-cli"},
	} {
		r := newLaunchRig(t, env)
		if code := r.a.Main([]string{"t"}); code != 1 || r.execArgv != nil {
			t.Errorf("%v: exit %d, exec %q", env, code, r.execArgv)
		}
	}
}

// --selftest checks the guest can start the harness (or a command) and writes nothing in HOME.
func TestAgentRunSelftest(t *testing.T) {
	r := newLaunchRig(t, map[string]string{"WARDYN_HARNESSES": "claude-code", "WARDYN_START_HARNESS": "claude-code"})
	if code := r.a.Main([]string{"--selftest"}); code != 0 {
		t.Fatalf("exit %d: %s", code, r.stdout.String())
	}
	if !r.ranArgv(CLTGitArgv...) || !r.ranArgv("/usr/local/bin/claude", "--version") {
		t.Errorf("checks run %q", r.ran)
	}
	if ents, _ := os.ReadDir(r.home); len(ents) > 0 || r.execArgv != nil {
		t.Errorf("the selftest left %v in HOME / exec'd %q", ents, r.execArgv)
	}

	r = newLaunchRig(t, map[string]string{"WARDYN_HARNESSES": "codex-cli"})
	r.fail = []string{"/usr/local/bin/codex", "--version"}
	if code := r.a.Main([]string{"--selftest"}); code != 1 || !strings.Contains(r.stdout.String(), "FAIL    /usr/local/bin/codex --version") {
		t.Errorf("a failing harness passed: exit %d\n%s", code, r.stdout.String())
	}

	// exec mode: no harness, and git only with repo wiring.
	r = newLaunchRig(t, map[string]string{"WARDYN_TASK_MODE": "exec"})
	r.fail = []string{CLTGitArgv[0]}
	if code := r.a.Main([]string{"--selftest"}); code != 0 || len(r.ran) != 0 {
		t.Errorf("exec-mode selftest: exit %d, ran %q\n%s", code, r.ran, r.stdout.String())
	}

	r = newLaunchRig(t, map[string]string{"WARDYN_TASK_MODE": "exec", "SSL_CERT_FILE": "/no/such/ca.pem"})
	if code := r.a.Main([]string{"--selftest"}); code != 1 {
		t.Errorf("a missing CA file passed: %s", r.stdout.String())
	}
}
