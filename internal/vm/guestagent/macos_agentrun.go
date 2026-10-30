// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package guestagent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// The macOS guest's run launcher, /usr/local/bin/agent-run: the guest agent's own binary, reached by that
// name (cmd/wardyn-guest-agent). It keeps the Linux images' contract (deploy/images/README.md, "agent-run")
// so the control plane builds one argv for every sandbox: `agent-run "<task>"` runs the run's harness on
// the task, or the task as a shell command under WARDYN_TASK_MODE=exec, and `agent-run --selftest` checks
// the guest can start one. It ports the Linux launchers' prep (deploy/images/common/agent-run-lib.sh and
// each harness's agent-run) for what a macOS guest serves, and refuses the rest with a sentence and exit
// 64 before doing anything. It runs as the agent user, with the env the guest agent gives every exec.

// AgentRunRefused is the exit code of a launch the guest does not serve.
const AgentRunRefused = 64

// The launcher's refusals, the sentence the run's owner reads.
const (
	agentRunTasksRefused       = "A macOS VM sandbox runs one agent per run in this release, so this run's tasks for several agents were not started."
	agentRunInteractiveRefused = "A macOS VM sandbox has no interactive agent session in this release; its terminal opens a shell."
	agentRunServiceRefused     = "A macOS VM sandbox does not run a service in this release, so the run was not started."
	agentRunHoldRefused        = "A macOS VM sandbox cannot hold tool calls for approval in this release, so the run was not started. Turn held tool approvals off for this run."
	agentRunScanRefused        = "A macOS VM sandbox does not scan a workspace in this release, so the run was not started."
)

// The harnesses' CLIs, where the pins put them (deploy/macos-guest/tooling.json).
const (
	macClaudePath = MacToolsBinDir + "/claude"
	macCodexPath  = MacToolsBinDir + "/codex"
)

// managedFileNotInPlace is the pre-start check's refusal: a managed file Init wrote is gone or changed.
const managedFileNotInPlace = "This run was not started: its managed file %s is not in place as %s at mode %04o, so its walls cannot be vouched for."

// macManagedDirs is where each harness reads the files a run's walls are made of.
var macManagedDirs = map[string]string{"claude-code": MacClaudeCodeManagedDir, "codex-cli": MacCodexManagedDir}

// The managed-file checks' root and the top of the folder chain they read: variables so a test runs them
// as itself, in its own folder.
var (
	managedRootUID  = 0
	managedChainTop = "/"
)

// macClaudeMCPConfig is the MCP file an autonomous Claude Code run is passed when Init wrote one; a
// variable so a test can stand one in.
var macClaudeMCPConfig = MacClaudeMCPConfigPath

// AgentRun is one launch. Its hooks are the process boundary, so a test replaces them.
type AgentRun struct {
	// Env is the launcher's environment, which the started process inherits as changed here.
	Env            map[string]string
	Stdout, Stderr io.Writer
	// Exec replaces the launcher with argv[0] (syscall.Exec); it returns only on failure.
	Exec func(path string, argv, env []string) error
	// Run runs argv in dir to completion, with stdin, and returns its combined output.
	Run func(dir string, argv, env []string, stdin string) (string, error)
	// Chdir makes dir the started process's working directory.
	Chdir func(dir string) error
}

// AgentRunMain is the launcher's main: args are its arguments after argv[0].
func AgentRunMain(args []string) int {
	a := &AgentRun{Env: map[string]string{}, Stdout: os.Stdout, Stderr: os.Stderr, Exec: syscall.Exec, Run: runCommand, Chdir: os.Chdir}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			a.Env[k] = v
		}
	}
	return a.Main(args)
}

func runCommand(dir string, argv, env []string, stdin string) (string, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdin = dir, env, strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Main runs one launch and returns its exit code (a started process's own, as it replaces this one).
func (a *AgentRun) Main(args []string) int {
	arg := ""
	if len(args) > 0 {
		arg = args[0]
	}
	switch arg {
	case "--selftest":
		return a.selftest()
	case "--tasks":
		return a.refuse(agentRunTasksRefused)
	case "--idle", "--revive", "--boot-seed", "--continue":
		return a.refuse(agentRunInteractiveRefused)
	case "":
		a.say("task text is required (usage: agent-run '<task>')")
		return 1
	}
	switch {
	case a.Env["WARDYN_SERVICE"] == "1":
		return a.refuse(agentRunServiceRefused)
	case a.Env["WARDYN_TOOL_APPROVALS"] == "hold":
		return a.refuse(agentRunHoldRefused)
	case a.Env["WARDYN_SCAN_ONLY"] == "1":
		return a.refuse(agentRunScanRefused)
	}
	if err := a.managedFilesInPlace(); err != nil {
		a.say(err.Error())
		return 1
	}
	execMode := a.Env["WARDYN_TASK_MODE"] == "exec"
	harness := ""
	if !execMode {
		var err error
		if harness, err = a.harness(); err != nil {
			a.say(err.Error())
			return 1
		}
	}
	workdir, err := a.prepare(harness)
	if err != nil {
		a.say(err.Error())
		return 1
	}
	if err := a.Chdir(workdir); err != nil {
		a.say("the working folder " + workdir + ": " + err.Error())
		return 1
	}
	if execMode {
		a.say("exec task mode — running task as a shell command (no agent harness)")
		return a.exec("/bin/sh", []string{"/bin/sh", "-c", arg})
	}
	if harness == "codex-cli" {
		return a.codex(arg)
	}
	return a.claude(arg)
}

func (a *AgentRun) say(msg string) { fmt.Fprintln(a.Stderr, "agent-run: "+msg) }

func (a *AgentRun) refuse(sentence string) int {
	a.say(sentence)
	return AgentRunRefused
}

// exec starts path in the launcher's place with the launcher's env.
func (a *AgentRun) exec(path string, argv []string) int {
	err := a.Exec(path, argv, a.environ())
	if err == nil {
		return 0 // a test's Exec; the real one does not return
	}
	a.say("start " + path + ": " + err.Error())
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) {
		return 127
	}
	return 126
}

func (a *AgentRun) environ() []string {
	out := make([]string, 0, len(a.Env))
	for k, v := range a.Env {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}

// git runs git in dir; a failure is the caller's to report or ignore.
func (a *AgentRun) git(dir string, args ...string) error {
	_, err := a.Run(dir, append([]string{"git"}, args...), a.environ(), "")
	return err
}

// harness is the harness the run starts: WARDYN_START_HARNESS, else the run's first, as agent-run-set picks.
func (a *AgentRun) harness() (string, error) {
	hs := strings.Fields(a.Env["WARDYN_HARNESSES"])
	start := a.Env["WARDYN_START_HARNESS"]
	if start == "" && len(hs) > 0 {
		start = hs[0]
	}
	switch {
	case start == "":
		return "", errors.New("this run names no harness to start (WARDYN_START_HARNESS)")
	case len(hs) > 0 && !slices.Contains(hs, start):
		return "", fmt.Errorf("start harness %q is not one of this run's harnesses", start)
	case start != "claude-code" && start != "codex-cli":
		return "", fmt.Errorf("harness %q is not one this macOS base serves", start)
	}
	return start, nil
}

// prepare is the shared prep before the task: the harness's config, the files the control plane sent,
// the work folders, git's broker routes and the run's clones. It returns the folder the task starts in.
func (a *AgentRun) prepare(harness string) (string, error) {
	home := a.Env["HOME"]
	if !filepath.IsAbs(home) {
		return "", errors.New("HOME is not an absolute path")
	}
	cfg := a.Env["CLAUDE_CONFIG_DIR"]
	if cfg != "" {
		_ = os.MkdirAll(cfg, 0o700)
	}
	if harness == "claude-code" {
		cfg = cmpOr(cfg, filepath.Join(home, ".claude"))
		_ = os.MkdirAll(cfg, 0o700)
		for _, f := range []string{filepath.Join(home, ".claude.json"), filepath.Join(cfg, ".claude.json")} {
			if _, err := os.Lstat(f); errors.Is(err, os.ErrNotExist) {
				_ = os.WriteFile(f, []byte("{\"hasCompletedOnboarding\":true}\n"), 0o644)
			}
		}
		if b64 := a.Env["WARDYN_CLAUDE_MANAGED_B64"]; b64 != "" {
			if b, err := base64.StdEncoding.DecodeString(b64); err == nil {
				_ = writeFileMode(filepath.Join(cfg, ".credentials.json"), b, 0o600)
			}
		}
	}
	a.homeRecords("artifact", a.Env["WARDYN_ARTIFACT_CONFIG_B64"], 0o644)
	a.homeRecords("aws-sso", a.Env["WARDYN_AWS_SSO_CONFIG_B64"], 0o600)
	a.homeRecords("azure-cli", a.Env["WARDYN_AZURE_CLI_CONFIG_B64"], 0o600)
	a.homeRecords("kubectl", a.Env["WARDYN_KUBECTL_CONFIG_B64"], 0o600)
	if harness == "codex-cli" {
		if err := a.prepareCodex(home); err != nil {
			return "", err
		}
	}
	workdir := filepath.Join(home, "work")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return "", err
	}
	for _, d := range strings.Split(a.Env["WARDYN_EPHEMERAL_DIRS"], ",") {
		if d != "" && os.MkdirAll(d, 0o755) != nil {
			a.say("could not create ephemeral dir " + d)
		}
	}
	if d := a.Env["GOTMPDIR"]; d != "" {
		_ = os.MkdirAll(d, 0o755)
	}
	_ = a.git("", "config", "--global", "--add", "safe.directory", "*")
	a.gitBrokerRoutes()
	a.clones(workdir)
	return a.resolveWorkdir(workdir)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// homeRecords writes the "<home-relative path>\t<base64>" lines the control plane sends for a component's
// files under HOME. A record is written for the run's home, so a Linux home in its content (an AWS
// credential_process path) is the guest's.
func (a *AgentRun) homeRecords(label, records string, mode os.FileMode) {
	home := a.Env["HOME"]
	for _, line := range strings.Split(records, "\n") {
		rel, b64, _ := strings.Cut(line, "\t")
		if rel == "" || (mode == 0o644 && b64 == "") {
			continue
		}
		if filepath.IsAbs(rel) || strings.Contains(rel, "..") {
			a.say("skipping unsafe " + label + "-config path: " + rel)
			continue
		}
		b, err := base64.StdEncoding.DecodeString(b64)
		if err == nil && home != "/home/agent" {
			b = bytes.ReplaceAll(b, []byte("/home/agent/"), []byte(home+"/"))
		}
		dst := filepath.Join(home, rel)
		if err == nil {
			err = os.MkdirAll(filepath.Dir(dst), 0o755)
		}
		if err == nil {
			err = writeFileMode(dst, b, mode)
		}
		if err != nil {
			a.say("WARNING failed to write " + label + " config " + rel)
		}
	}
}

// writeFileMode writes b to path at exactly mode, whatever the umask.
func writeFileMode(path string, b []byte, mode os.FileMode) error {
	if err := os.WriteFile(path, b, mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

var (
	ghSlug   = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
	hostLike = regexp.MustCompile(`^([A-Za-z0-9-]+@)?[A-Za-z0-9.-]+$`)
)

// gitBrokerRoutes points git at the run proxy's brokers (configure_git_broker_insteadof and
// configure_git_pat_broker_insteadof): a GitHub repo of the run's github_token grant, and each host of
// the never-resident git_pat lane. The guest has no credential helper, so the brokers are its only route.
func (a *AgentRun) gitBrokerRoutes() {
	base := strings.TrimSuffix(cmpOr(a.Env["WARDYN_PROXY_URL"], "http://wardyn-proxy:3128"), "/")
	if a.Env["WARDYN_GITHUB_GRANT_ID"] != "" {
		records := a.Env["WARDYN_REPOS"]
		if records == "" && a.Env["WARDYN_REPO_SLUG"] != "" {
			records = "\t\t" + a.Env["WARDYN_REPO_SLUG"]
		}
		ssh := a.Env["WARDYN_SSH_GRANTS"] != "" && a.Env["WARDYN_SSH_GRANTS"] != "{}"
		for _, rec := range strings.Split(records, "\n") {
			slug := strings.TrimSuffix(repoField(rec, 2), ".git")
			for _, p := range []string{"https://github.com/", "http://github.com/"} {
				slug = strings.TrimPrefix(slug, p)
			}
			if !ghSlug.MatchString(slug) {
				continue
			}
			broker := base + "/wardyn/gh/" + slug
			_ = a.git("", "config", "--global", "url."+broker+".insteadOf", "https://github.com/"+slug)
			if !ssh {
				_ = a.git("", "config", "--global", "--add", "url."+broker+".insteadOf", "git@github.com:"+slug)
				_ = a.git("", "config", "--global", "--add", "url."+broker+".insteadOf", "ssh://git@github.com/"+slug)
			}
		}
	}
	for _, entry := range strings.Fields(a.Env["WARDYN_GIT_PAT_BROKER_HOSTS"]) {
		if !hostLike.MatchString(entry) {
			continue
		}
		host := entry[strings.Index(entry, "@")+1:]
		_ = a.git("", "config", "--global", "--add", "url."+base+"/wardyn/git/"+host+"/.insteadOf", "https://"+entry+"/")
	}
}

// repoField is field i (url, dest, slug, ref) of a WARDYN_REPOS record.
func repoField(rec string, i int) string {
	f := strings.Split(rec, "\t")
	if i < len(f) {
		return f[i]
	}
	return ""
}

// clones clones the run's repos (dispatch_repo_clones): each WARDYN_REPOS record into its folder, or
// WARDYN_REPO_URL into the work folder when no checkout is there yet.
func (a *AgentRun) clones(workdir string) {
	if recs := a.Env["WARDYN_REPOS"]; recs != "" {
		for _, rec := range strings.Split(recs, "\n") {
			if repoField(rec, 0) != "" {
				a.cloneOne(repoField(rec, 0), repoField(rec, 1), repoField(rec, 2), repoField(rec, 3))
			}
		}
		return
	}
	url := a.Env["WARDYN_REPO_URL"]
	if url == "" || firstCheckout(workdir) != "" {
		return
	}
	name := strings.TrimSuffix(url[strings.LastIndex(url, "/")+1:], ".git")
	a.cloneOne(url, filepath.Join(workdir, cmpOr(name, "repo")), a.Env["WARDYN_REPO_SLUG"], "")
}

// cloneOne is clone_one: a shallow clone through the proxy, at ref when one is named, onto the run's
// branch. A failed clone is a governance signal the run carries on past, as on Linux.
func (a *AgentRun) cloneOne(url, dest, slug, ref string) {
	if dest == "" {
		return
	}
	what := cmpOr(slug, url)
	if _, err := os.Lstat(filepath.Join(dest, ".git")); err == nil {
		return
	}
	if st, err := os.Stat(dest); err == nil {
		if ents, _ := os.ReadDir(dest); !st.IsDir() || len(ents) > 0 {
			a.say("clone SKIPPED for " + dest + " — it already holds files that are not a checkout of " + what + "; they are left as they are; continuing")
			return
		}
	}
	if os.MkdirAll(dest, 0o755) != nil {
		a.say("clone SKIPPED for " + dest + " — the folder cannot be created; continuing")
		return
	}
	at := ""
	if ref != "" {
		at = " @ " + ref
	}
	a.say("cloning " + what + at + " into " + dest + " (shallow, via wardyn-proxy)")
	ok := false
	if ref == "" {
		ok = a.git("", "clone", "--depth", "1", "--", url, dest) == nil
	} else if a.git("", "clone", "--depth", "1", "--branch", ref, "--", url, dest) == nil {
		ok = true
	} else {
		clearDir(dest)
		ok = a.git("", "clone", "--no-checkout", "--depth", "1", "--", url, dest) == nil &&
			a.git(dest, "fetch", "--depth", "1", "origin", ref) == nil &&
			a.git(dest, "checkout", "FETCH_HEAD") == nil
		if !ok {
			clearDir(dest)
		}
	}
	if !ok {
		a.say("clone FAILED for " + dest + at + " — the egress policy or credential broker may have blocked it (a governance signal); continuing")
		if why := a.Env["WARDYN_ADO_LANE_DECLINED"]; why != "" && a.adoHost(url) {
			a.say("cloning " + what + ": this run was not given an Azure DevOps credential (" + why + "); the run's Credentials panel and 'wardyn run get " + cmpOr(a.Env["WARDYN_RUN_ID"], "<run-id>") + "' say why")
		}
		return
	}
	a.say("clone OK (" + dest + at + ")")
	if id := a.Env["WARDYN_RUN_ID"]; id != "" {
		branch := "wardyn/" + id + "/work"
		_ = a.git("", "config", "--global", "push.default", "current")
		if a.git(dest, "checkout", "-b", branch) == nil {
			a.say("on run branch " + branch + " (" + dest + ")")
		} else {
			a.say("WARNING could not create run branch " + branch + " in " + dest + "; a push outside wardyn/" + id + "/* will be refused by the proxy")
		}
	}
}

// adoHost says whether url's host is one the Azure DevOps hint is for (ado_lane_hint): any, unless the
// control plane names the hosts.
func (a *AgentRun) adoHost(url string) bool {
	hosts := a.Env["WARDYN_ADO_LANE_DECLINED_HOSTS"]
	if hosts == "" {
		return true
	}
	h := url
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
		h, _, _ = strings.Cut(h, "/")
	} else {
		h, _, _ = strings.Cut(h, ":")
	}
	h = h[strings.LastIndex(h, "@")+1:]
	h, _, _ = strings.Cut(h, ":")
	return h != "" && slices.Contains(strings.Fields(hosts), strings.ToLower(h))
}

func clearDir(dir string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// firstCheckout is the first folder holding a .git within two levels under dir, or "".
func firstCheckout(dir string) string {
	for _, pat := range []string{"*/.git", "*/*/.git"} {
		if m, _ := filepath.Glob(filepath.Join(dir, pat)); len(m) > 0 {
			return filepath.Dir(m[0])
		}
	}
	return ""
}

// resolveWorkdir is resolve_workdir: WARDYN_START_FOLDER when set, else the work folder's checkout.
func (a *AgentRun) resolveWorkdir(workdir string) (string, error) {
	switch sf := a.Env["WARDYN_START_FOLDER"]; {
	case sf == "image_default":
		return workdir, nil
	case strings.HasPrefix(sf, "/"):
		real, err := filepath.EvalSymlinks(sf)
		if st, serr := os.Stat(sf); err != nil || real != sf || serr != nil || !st.IsDir() {
			return "", fmt.Errorf("the starting folder %s is missing or not a plain folder; not starting elsewhere", sf)
		}
		_ = a.git("", "config", "--global", "--add", "safe.directory", sf)
		return sf, nil
	case sf != "":
		return "", errors.New("WARDYN_START_FOLDER must be image_default or an absolute path")
	}
	if repo := firstCheckout(workdir); repo != "" {
		_ = a.git("", "config", "--global", "--add", "safe.directory", repo)
		return repo, nil
	}
	return workdir, nil
}

// anthropicMode is detect_anthropic_mode: a subscription's credentials file, else Bedrock, else the
// proxy's key gateway.
func (a *AgentRun) anthropicMode() string {
	home := a.Env["HOME"]
	cfg := cmpOr(a.Env["CLAUDE_CONFIG_DIR"], filepath.Join(home, ".claude"))
	for _, f := range []string{cfg + "/.credentials.json", cfg + "/credentials.json", home + "/.claude/.credentials.json", home + "/.claude/credentials.json"} {
		if st, err := os.Stat(f); err == nil && st.Mode().IsRegular() {
			return "subscription"
		}
	}
	if a.Env["CLAUDE_CODE_USE_BEDROCK"] != "" {
		return "bedrock"
	}
	return "apikey"
}

// claude starts Claude Code on the task as claude-code/agent-run's autonomous lane does: its auth mode's
// wiring, then `claude -p`, with the run's MCP servers under --strict-mcp-config when it has any. The
// run's ceiling is the managed settings Init wrote where Claude Code reads them on macOS.
func (a *AgentRun) claude(task string) int {
	mode := a.anthropicMode()
	switch mode {
	case "subscription":
		if a.Env["ANTHROPIC_BASE_URL"] == "https://api.anthropic.com" {
			delete(a.Env, "ANTHROPIC_BASE_URL")
		}
	case "bedrock":
		delete(a.Env, "ANTHROPIC_BASE_URL")
	default:
		a.Env["ANTHROPIC_API_KEY"] = cmpOr(a.Env["ANTHROPIC_API_KEY"], "wardyn-proxy-injected")
	}
	a.say("anthropic auth mode = " + mode)
	argv := []string{"claude", "-p", task, "--output-format", "text", "--dangerously-skip-permissions"}
	if unix.Access(macClaudeMCPConfig, unix.R_OK) == nil {
		argv = append(argv, "--mcp-config", macClaudeMCPConfig, "--strict-mcp-config")
	}
	return a.exec(macClaudePath, argv)
}

// codex starts Codex on the task as codex-cli/agent-run does: bound to the run's sandbox mode when the
// policy names one (the requirements Init wrote refuse any other), after checking Codex's own sandbox
// starts; otherwise with Codex's own approvals and sandbox off, the sandbox being the walls.
func (a *AgentRun) codex(task string) int {
	argv := []string{"codex", "exec", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox"}
	switch mode := a.Env["WARDYN_HARNESS_SANDBOX"]; mode {
	case "":
	case "read-only", "workspace-write":
		var cfg []string
		if mode == "workspace-write" {
			cfg = []string{"-c", "sandbox_workspace_write.network_access=true"}
		}
		probe := slices.Concat([]string{macCodexPath, "sandbox", "-c", `sandbox_mode="` + mode + `"`}, cfg, []string{"--", "true"})
		if out, err := a.Run("", probe, a.environ(), ""); err != nil {
			fmt.Fprint(a.Stderr, out)
			a.say("Codex's own sandbox (" + mode + ") did not start in this sandbox; the run's policy requires it, so the task was not run")
			return 1
		}
		argv = slices.Concat([]string{"codex", "exec", "--skip-git-repo-check", "-s", mode, "-c", `approval_policy="never"`}, cfg)
	default:
		a.say("WARDYN_HARNESS_SANDBOX=" + mode + " is not read-only or workspace-write; the task was not run")
		return 1
	}
	if m := a.Env["WARDYN_CODEX_MODEL"]; m != "" {
		argv = append(argv, "-m", m)
	}
	return a.exec(macCodexPath, append(argv, task))
}

// prepareCodex is prepare_codex_config: Codex's route and model in config.toml, the run's MCP servers,
// and the inert sentinel as its API key.
func (a *AgentRun) prepareCodex(home string) error {
	dir := cmpOr(a.Env["CODEX_HOME"], filepath.Join(home, ".codex"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "config.toml")
	b, _ := os.ReadFile(path)
	conf := string(b)
	if v := a.Env["WARDYN_CODEX_BASE_URL"]; v != "" {
		conf = setTOMLKey(conf, "openai_base_url", v)
	}
	if v := a.Env["WARDYN_CODEX_MODEL"]; v != "" {
		conf = setTOMLKey(conf, "model", v)
	}
	conf = setMCPServers(conf, a.Env["WARDYN_MCP_SERVERS_JSON"], a.Env["WARDYN_MCP_STDIO_TOML"])
	if conf != string(b) {
		if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
			return err
		}
	}
	if key := a.Env["CODEX_API_KEY"]; key != "" {
		if _, err := a.Run("", []string{macCodexPath, "login", "--with-api-key"}, a.environ(), key); err != nil {
			a.say("WARNING codex login --with-api-key failed; an interactive codex will ask to sign in")
		}
	}
	return nil
}

// setTOMLKey is codex_set_toml_key: key = "val" as a top-level line, before the first table, replacing
// the key's own top-level line.
func setTOMLKey(conf, key, val string) string {
	line := key + ` = "` + strings.ReplaceAll(strings.ReplaceAll(val, `\`, `\\`), `"`, `\"`) + `"`
	isKey := regexp.MustCompile(`^[[:space:]]*` + regexp.QuoteMeta(key) + `[[:space:]]*=`)
	var out []string
	top, put := true, false
	for _, l := range splitLines(conf) {
		if strings.HasPrefix(strings.TrimSpace(l), "[") {
			if top && !put {
				out, put = append(out, line), true
			}
			top = false
		}
		if top && isKey.MatchString(l) {
			continue
		}
		out = append(out, l)
	}
	if !put {
		out = append(out, line)
	}
	return strings.Join(out, "\n") + "\n"
}

var (
	mcpURLEntry = regexp.MustCompile(`"([a-z0-9-]+)":"(https?://[A-Za-z0-9._:/-]+)"`)
	mcpTable    = regexp.MustCompile(`^\[mcp_servers\.([a-z0-9-]+)\]$`)
)

// setMCPServers is codex_set_mcp_servers: one [mcp_servers.<key>] table per remote server (key to broker
// URL) and the local servers' tables, each replacing a table of the same key.
func setMCPServers(conf, servers, stdio string) string {
	if servers == "" && stdio == "" {
		return conf
	}
	drop := map[string]bool{}
	var tables strings.Builder
	for _, m := range mcpURLEntry.FindAllStringSubmatch(servers, -1) {
		drop["[mcp_servers."+m[1]+"]"] = true
		fmt.Fprintf(&tables, "[mcp_servers.%s]\nurl = \"%s\"\n", m[1], m[2])
	}
	for _, l := range strings.Split(stdio, "\n") {
		if m := mcpTable.FindStringSubmatch(l); m != nil {
			drop["[mcp_servers."+m[1]+"]"] = true
		}
	}
	tables.WriteString(stdio)
	var out []string
	skip := false
	for _, l := range splitLines(conf) {
		if t := strings.Join(strings.Fields(l), ""); strings.HasPrefix(strings.TrimSpace(l), "[") {
			skip = drop[t]
		}
		if !skip {
			out = append(out, l)
		}
	}
	kept := strings.Join(out, "\n")
	if kept != "" {
		kept += "\n"
	}
	return kept + tables.String()
}

func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// selftest is `agent-run --selftest`: whether this guest can start the run's harness (or, under
// WARDYN_TASK_MODE=exec, a command), with no key and no side effect. It writes nothing: prepare runs it
// in the base, where anything it left would reach every run.
func (a *AgentRun) selftest() int {
	fmt.Fprintln(a.Stdout, "=== wardyn agent-run selftest (macOS guest) ===")
	ok := true
	check := func(pass bool, what, why string) {
		if pass {
			fmt.Fprintln(a.Stdout, "  OK      "+what)
		} else {
			fmt.Fprintln(a.Stdout, "  FAIL    "+what+": "+why)
			ok = false
		}
	}
	execMode := a.Env["WARDYN_TASK_MODE"] == "exec"
	check(unix.Access("/bin/sh", unix.X_OK) == nil, "/bin/sh", "not executable")
	if !execMode || a.Env["WARDYN_REPO_URL"]+a.Env["WARDYN_REPOS"] != "" {
		out, err := a.Run("", CLTGitArgv, a.environ(), "")
		check(err == nil, "git ("+MacCLTGit+")", strings.TrimSpace(out+" "+errString(err)))
	}
	if !execMode {
		h, err := a.harness()
		check(err == nil, "start harness "+h, errString(err))
		if err == nil {
			bin := map[string]string{"claude-code": macClaudePath, "codex-cli": macCodexPath}[h]
			out, err := a.Run("", []string{bin, "--version"}, a.environ(), "")
			check(err == nil, bin+" --version", strings.TrimSpace(out+" "+errString(err)))
			if h == "claude-code" {
				fmt.Fprintln(a.Stdout, "  anthropic auth mode (detect only): "+a.anthropicMode())
			}
			dir := macManagedDirs[h]
			err = rootOnlyChain(dir)
			check(err == nil, "managed folder "+dir+" holds only root's files", errString(err))
		}
	}
	if a.Env[EnvManagedFiles] != "" {
		err := a.managedFilesInPlace()
		check(err == nil, "managed files in place", errString(err))
	}
	for _, k := range append([]string{caAddEnv}, caReplaceEnv...) {
		if f := a.Env[k]; f != "" {
			check(unix.Access(f, unix.R_OK) == nil, k+"="+f, "not readable")
		}
	}
	home := a.Env["HOME"]
	st, err := os.Stat(home)
	check(err == nil && st.IsDir() && unix.Access(home, unix.W_OK) == nil, "HOME="+home, "not a folder the agent user can write")
	if !ok {
		return 1
	}
	return 0
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// managedFilesInPlace is the pre-start check of the managed files the run's Init wrote
// (EnvManagedFiles): each is a regular file at its mode, root's in a folder only root can change, or a
// delivered secret that is the agent's own.
func (a *AgentRun) managedFilesInPlace() error {
	v := a.Env[EnvManagedFiles]
	if v == "" {
		return nil
	}
	var files []ManagedFileCheck
	if err := json.Unmarshal([]byte(v), &files); err != nil {
		return fmt.Errorf("%s: %w", EnvManagedFiles, err)
	}
	for _, f := range files {
		owner, who := managedRootUID, "root's"
		if f.AgentOwned {
			owner, who = os.Getuid(), "the agent's"
		}
		st, err := os.Lstat(f.Path)
		ok := err == nil && st.Mode().IsRegular() && fileUID(st) == owner && st.Mode().Perm() == f.Mode.Perm()
		if ok && !f.AgentOwned {
			ok = rootOnlyChain(filepath.Dir(f.Path)) == nil
		}
		if !ok {
			return fmt.Errorf(managedFileNotInPlace, f.Path, who, f.Mode.Perm())
		}
	}
	return nil
}

// rootOnlyChain says whether only root can create or replace a file in dir: every folder of its path that
// exists is root's and writable by no group or other, and a link on it is root's and leads to such a
// folder. A folder not there yet is root's to create (Init makes it).
func rootOnlyChain(dir string) error {
	var chain []string
	for p := filepath.Clean(dir); ; p = filepath.Dir(p) {
		chain = append(chain, p)
		if p == managedChainTop || p == filepath.Dir(p) {
			break
		}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		p := chain[i]
		st, err := os.Lstat(p)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return nil
		case err != nil:
			return err
		case fileUID(st) != managedRootUID:
			return fmt.Errorf("%s is not root's", p)
		case st.Mode()&os.ModeSymlink != 0:
			target, err := filepath.EvalSymlinks(p)
			if err != nil {
				return err
			}
			if err := rootOnlyChain(target); err != nil {
				return err
			}
		case !st.IsDir():
			return fmt.Errorf("%s is not a folder", p)
		case st.Mode().Perm()&0o022 != 0:
			return fmt.Errorf("%s can be written by others than root", p)
		}
	}
	return nil
}

func fileUID(st os.FileInfo) int {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return int(sys.Uid)
	}
	return -1
}
