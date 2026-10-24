// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestAgentRunIdle_ExitsOnSignal pins wardyn issue #810, a follow-up from the
// #773 review (issue #468): TestAgentIdleScript_ExitsOnSIGTERM
// (internal/runner/sandbox_test.go) proves the Go-embedded NON-interactive idle
// script's TERM handling, but nothing automated covered the three bash
// `agent-run --idle` loops that INTERACTIVE runs use as their main process
// (claude-code, codex-cli, aws-sso). Each now installs
// `trap 'exit 143' TERM; trap 'exit 130' INT` around a
// `while :; do sleep 3600 & wait $! || :; done` loop instead of
// `exec sleep infinity`: as PID 1, a bare `sleep` ignores TERM/INT outright, so
// a stop would sit out the full kill timeout instead of exiting promptly.
//
// The trap is what makes that PID-1 dependency moot — a trap installed by the
// shell fires the same whether that shell is a literal PID 1 or merely a
// process GROUP leader — so this runs each REAL script as its own process
// group leader (the idiom TestAgentIdleScript_ExitsOnSIGTERM already uses) and
// signals only the leader — matching how docker stop and the kubelet deliver
// a stop signal, which target PID 1 only, never the whole process group —
// pinning the exit codes 143/130 the same way TestAgentIdleScript_ExitsOnSIGTERM
// pins the sh script.
func TestAgentRunIdle_ExitsOnSignal(t *testing.T) {
	images := []struct {
		name  string
		setup func(t *testing.T) (script string, env []string, prepDone string)
	}{
		{"claude-code", ccEnvIdleSignalSetup(ccRunnableAgentRun)},
		{"codex-cli", ccEnvIdleSignalSetup(cxRunnableAgentRun)},
		{"aws-sso", awsSSOIdleSignalSetup},
		{"agent-base", ccEnvIdleSignalSetup(baseRunnableAgentRun)},
	}
	signals := []struct {
		name   string
		signal syscall.Signal
		want   int
	}{
		{"TERM", syscall.SIGTERM, 143},
		{"INT", syscall.SIGINT, 130},
	}
	for _, img := range images {
		for _, sig := range signals {
			t.Run(img.name+"/"+sig.name, func(t *testing.T) {
				script, env, prepDone := img.setup(t)
				code := runIdleAndSignal(t, script, env, prepDone, sig.signal)
				if code != sig.want {
					t.Fatalf("%s agent-run --idle exited %d on %s, want %d — an out-of-band stop must read as a signal kill downstream, not the full kill-timeout wait #468 fixed",
						img.name, code, sig.name, sig.want)
				}
			})
		}
	}
}

// runIdleAndSignal starts the real agent-run --idle script as its own process
// group leader, waits for it to reach the hold-open point (prepDone written —
// same signal ccRunIdle/runIdle/cxRunIdle wait for in the
// sibling boot-ordering tests) and then for the idle loop's `sleep` child,
// sends sig to the LEADER ONLY (matching docker stop / the kubelet), and
// returns the exit code.
func runIdleAndSignal(t *testing.T, script string, env []string, prepDone string, sig syscall.Signal) int {
	t.Helper()
	cmd := exec.Command("bash", script, "--idle")
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A REAL file, not a pipe: bash backgrounds `sleep 3600 &` without job
	// control (a non-interactive script), and bash's own documented behaviour
	// for an async command in that mode is to make the CHILD ignore SIGINT
	// (not SIGTERM) — so on the INT case that sleep outlives the trap's `exit
	// 130` and keeps holding the write end of a piped stdout/stderr open.
	// cmd.Wait() would then block forever on the pipe-copy goroutine reaching
	// EOF even though bash itself already exited; a plain *os.File needs no
	// such goroutine, so cmd.Wait() only waits on bash.
	outFile, err := os.CreateTemp(t.TempDir(), "agent-run-idle-out")
	if err != nil {
		t.Fatalf("create output file: %v", err)
	}
	cmd.Stdout, cmd.Stderr = outFile, outFile
	readOutput := func() string {
		b, _ := os.ReadFile(outFile.Name())
		return string(b)
	}
	// A signal ignored when this test binary started (a background job of a
	// non-interactive shell ignores SIGINT) stays ignored across exec, and bash
	// cannot trap a signal it starts with ignored, so the INT cases would sit
	// out the wait below. Handling it here makes exec hand bash the default.
	ignored := make(chan os.Signal, 1)
	signal.Notify(ignored, sig)
	err = cmd.Start()
	signal.Stop(ignored)
	if err != nil {
		t.Fatalf("start agent-run --idle: %v", err)
	}
	// Reaches every process in the group, including the orphaned `sleep` the
	// INT case can leave behind.
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })

	waitFor := func(what string, ready func() bool) {
		deadline := time.Now().Add(5 * time.Second)
		for !ready() {
			if time.Now().After(deadline) {
				t.Fatalf("agent-run --idle never reached the hold-open point (%s)\noutput: %s", what, readOutput())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFor("prep-done never appeared", func() bool {
		_, err := os.Stat(prepDone)
		return err == nil
	})
	// Each script writes prep-done a few lines BEFORE its two traps; a signal
	// in that window meets the default TERM/INT action and the case flakes.
	// The loop's `sleep 3600` child only exists once both traps are in place.
	waitFor("the idle loop's sleep child never appeared", func() bool {
		return hasSleepChild(cmd.Process.Pid)
	})

	// Leader only, matching docker stop / the kubelet, which signal PID 1
	// and never the whole process group.
	if err := syscall.Kill(cmd.Process.Pid, sig); err != nil {
		t.Fatalf("signal the process leader: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("agent-run --idle on signal %v: %v (want a *exec.ExitError with a signal-triggered exit code)\noutput: %s", sig, err, readOutput())
		}
		return exitErr.ExitCode()
	case <-time.After(3 * time.Second):
		t.Fatalf("agent-run --idle did not exit within 3s of signal %v\noutput: %s", sig, readOutput())
	}
	return -1
}

// hasSleepChild reports whether pid has a direct child that has exec'd
// `sleep` (a forked-but-not-yet-exec'd subshell still reads as bash).
func hasSleepChild(pid int) bool {
	children, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, pid))
	if err != nil {
		return false
	}
	for _, child := range strings.Fields(string(children)) {
		comm, err := os.ReadFile("/proc/" + child + "/comm")
		if err == nil && strings.TrimSpace(string(comm)) == "sleep" {
			return true
		}
	}
	return false
}

// ccEnvIdleSignalSetup reuses the real-script plumbing (ccRunnableAgentRun or
// cxRunnableAgentRun) over the claude-code fake tmux (ccIdleEnv) — the same env
// ccRunIdle and cxRunIdle build, minus the prep-done wait this test replaces
// with a signal.
func ccEnvIdleSignalSetup(runnable func(*testing.T) string) func(*testing.T) (string, []string, string) {
	return func(t *testing.T) (script string, env []string, prepDone string) {
		home, binDir, tmuxLog := ccIdleEnv(t)
		env = append(os.Environ(),
			"HOME="+home,
			"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			"TMUX_LOG="+tmuxLog,
			"CLAUDE_CONFIG_DIR="+filepath.Join(home, "cfg"),
			"WARDYN_REPOS=",
			"WARDYN_REPO_URL=",
			"WARDYN_MITM_CA_PEM=",
			"WARDYN_GITHUB_GRANT_ID=",
			"WARDYN_CLAUDE_MANAGED_B64=",
		)
		return runnable(t), env, filepath.Join(home, ".wardyn", "prep-done")
	}
}

// awsSSOIdleSignalSetup reuses awssso_agent_run_idle_test.go's real-script and
// fake-tmux plumbing (runnableAgentRun, fakeTmuxBin, mitmNonce) — the same env
// runIdle builds, minus the prep-done wait.
func awsSSOIdleSignalSetup(t *testing.T) (script string, env []string, prepDone string) {
	home, binDir, tmuxLog := fakeTmuxBin(t)
	nonce := mitmNonce(t)
	env = append(os.Environ(),
		"HOME="+home,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMUX_LOG="+tmuxLog,
		"MITM_NONCE="+nonce,
		"WARDYN_MITM_CA_PEM=-----BEGIN CERTIFICATE-----\n"+nonce+"\n-----END CERTIFICATE-----",
		"WARDYN_AWS_SSO_CONFIG_B64=.aws/config\tW3Nzby1zZXNzaW9uIHdhcmR5bl0K",
		"WARDYN_REPOS=",
		"WARDYN_REPO_URL=",
	)
	return runnableAgentRun(t), env, filepath.Join(home, ".wardyn", "prep-done")
}

// baseRunnableAgentRun: agent-base's agent-run (deploy/images/common/agent-run-stub),
// which agent-vscode and agent-novnc inherit unchanged — same real-script copy
// idiom as cxRunnableAgentRun.
func baseRunnableAgentRun(t *testing.T) string {
	t.Helper()
	body := ccRead(t, ccAbs(t, "../../../deploy/images/common/agent-run-stub"))
	const from = "source /usr/local/bin/agent-run-lib.sh"
	if !strings.Contains(body, from) {
		t.Fatalf("agent-run-stub no longer sources %s", from)
	}
	body = strings.Replace(body, from, "source "+ccAbs(t, ccAgentRunLibPath), 1)
	dst := filepath.Join(t.TempDir(), "agent-run")
	if err := os.WriteFile(dst, []byte(body), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write runnable agent-run: %v", err)
	}
	return dst
}

// agent-base's exec task runs in the run's accepted starting folder, not wherever the launcher
// put it; with no starting folder (the setup connectivity probe) it runs untouched.
func TestAgentBaseExecTaskRunsInTheStartingFolder(t *testing.T) {
	script := baseRunnableAgentRun(t)
	home := t.TempDir()
	start, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	launch := t.TempDir()
	run := func(startFolder string) string {
		t.Helper()
		cmd := exec.Command("bash", script, "pwd -P")
		cmd.Dir = launch
		cmd.Env = append(os.Environ(), "HOME="+home, "WARDYN_TASK_MODE=exec", "WARDYN_START_FOLDER="+startFolder,
			"WARDYN_REPOS=", "WARDYN_REPO_URL=", "WARDYN_MITM_CA_PEM=", "WARDYN_GITHUB_GRANT_ID=")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("agent-run exec task: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	if got := run(start); got != start {
		t.Errorf("exec task ran in %q, want the starting folder %q", got, start)
	}
	wantLaunch, _ := filepath.EvalSymlinks(launch)
	if got := run(""); got != wantLaunch {
		t.Errorf("exec task with no starting folder ran in %q, want the launch directory %q", got, wantLaunch)
	}
}

// An exec task with a starting folder gets the same credential prep --idle does before the task
// starts: artifact config files written and, for a git grant, the helper secret provisioned and exported.
func TestAgentBaseExecTaskPreparesCredentialsBeforeTheTask(t *testing.T) {
	script := baseRunnableAgentRun(t)
	home := t.TempDir()
	start := t.TempDir()
	cmd := exec.Command("bash", script, `test "$(cat "$HOME/cfg/x.conf")" = hi && test -f "$HOME/.wardyn/git-helper.secret" && test -n "$WARDYN_GIT_HELPER_SECRET" && echo ready`)
	cmd.Dir = start
	cmd.Env = append(os.Environ(), "HOME="+home, "WARDYN_TASK_MODE=exec", "WARDYN_START_FOLDER="+start,
		"WARDYN_REPOS=", "WARDYN_REPO_URL=", "WARDYN_MITM_CA_PEM=", "WARDYN_GITHUB_GRANT_ID=grant-1",
		"WARDYN_ARTIFACT_CONFIG_B64=cfg/x.conf\t"+base64.StdEncoding.EncodeToString([]byte("hi")))
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "ready" {
		t.Fatalf("exec task did not see the prepared credentials: err=%v out=%q", err, out)
	}
}

// gitHostRE finds the host in a URL (scheme://[user@]host[:port]) or scp-like (user@host:path) git argument.
var gitHostRE = regexp.MustCompile(`(?:://(?:[^/@\s]+@)?|\b[a-z]+@)([a-z0-9.-]+(?::\d+)?)`)

// fakeGitDir returns a PATH dir whose `git` logs every call (args plus whether the helper secret was
// exported) to the returned file and fakes a checkout for `git clone`, so a test sees what agent-base's
// exec branch asks git to do without any network.
func fakeGitDir(t *testing.T) (dir, log string) {
	t.Helper()
	dir, log = t.TempDir(), filepath.Join(t.TempDir(), "git.log")
	body := `#!/bin/sh
echo "secret=${WARDYN_GIT_HELPER_SECRET:+set} $*" >> "` + log + `"
if [ "$1" = clone ]; then
    for a in "$@"; do d="$a"; done
    mkdir -p "$d/.git"
fi
`
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(body), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	return dir, log
}

// An exec task whose run clones a repository (WARDYN_REPOS, or the legacy WARDYN_REPO_URL) but names no
// starting folder gets the workspace prep a harness run does: git is first pointed at the proxy broker, the
// git helper secret is provisioned and exported before the clone, and the clone is the workspace's own URL
// into the workspace folder through the lib's clone_one, with no other git call naming a host. A run with
// no workspace gets no helper, no clone and no git call at all.
func TestAgentBaseExecTaskClonesTheWorkspace(t *testing.T) {
	script := baseRunnableAgentRun(t)
	const url = "https://github.com/acme/app.git"
	const broker = "http://wardyn-proxy:3128/wardyn/gh/acme/app"
	run := func(mode string) (home, pwd, gitLog string) {
		t.Helper()
		home = t.TempDir()
		bin, glog := fakeGitDir(t)
		env := []string{"HOME=" + home, "PATH=" + bin + ":" + os.Getenv("PATH"), "WARDYN_TASK_MODE=exec",
			"WARDYN_START_FOLDER=", "WARDYN_REPOS=", "WARDYN_REPO_URL=", "WARDYN_MITM_CA_PEM=",
			"WARDYN_GITHUB_GRANT_ID=grant-1"}
		switch mode {
		case "repos":
			env = append(env, "WARDYN_REPOS="+url+"\t"+filepath.Join(home, "work", "app")+"\tacme/app\t")
		case "repo_url":
			env = append(env, "WARDYN_REPO_URL="+url, "WARDYN_REPO_SLUG=acme/app")
		}
		cmd := exec.Command("bash", script, "pwd -P")
		cmd.Dir = t.TempDir()
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("agent-run exec task: %v", err)
		}
		b, _ := os.ReadFile(glog)
		return home, strings.TrimSpace(string(out)), string(b)
	}

	for _, mode := range []string{"repos", "repo_url"} {
		t.Run(mode, func(t *testing.T) {
			// 1. With a workspace: checkout made, helper ready before it, task runs in it.
			home, pwd, gitLog := run(mode)
			dest, _ := filepath.EvalSymlinks(filepath.Join(home, "work", "app"))
			if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
				t.Errorf("exec task with a workspace has no checkout at %s: %v", dest, err)
			}
			if _, err := os.Stat(filepath.Join(home, ".wardyn", "git-helper.secret")); err != nil {
				t.Errorf("exec task with a workspace has no git helper secret: %v", err)
			}
			if pwd != dest {
				t.Errorf("exec task with a workspace ran in %q, want the clone %q", pwd, dest)
			}
			// 3. git is asked to clone only the workspace, with the helper secret exported, after it was pointed at
			// the proxy broker; every host it is told about is the workspace's own or the broker -- no new host.
			rewrite := "secret= config --global url." + broker + ".insteadOf https://github.com/acme/app"
			rewriteAt, cloneAt, clones := -1, -1, 0
			for n, line := range strings.Split(strings.TrimSpace(gitLog), "\n") {
				if line == rewrite {
					rewriteAt = n
				}
				if strings.Contains(line, " clone ") {
					clones++
					cloneAt = n
					if want := "secret=set clone --depth 1 -- " + url + " " + filepath.Join(home, "work", "app"); line != want {
						t.Errorf("git call %q, want %q", line, want)
					}
				}
				for _, m := range gitHostRE.FindAllStringSubmatch(line, -1) {
					if h := m[1]; h != "github.com" && h != "wardyn-proxy:3128" {
						t.Errorf("git was told about host %q, which is neither the workspace's nor the proxy broker: %s", h, line)
					}
				}
			}
			if clones != 1 {
				t.Errorf("clones: %d, want exactly the workspace clone\n%s", clones, gitLog)
			}
			// The legacy single-URL form gets no rewrite from configure_git_broker_insteadof on any image, so only
			// WARDYN_REPOS is held to it.
			if mode == "repos" && (rewriteAt < 0 || rewriteAt > cloneAt) {
				t.Errorf("the broker rewrite %q must be set before the clone (rewrite at %d, clone at %d)\n%s", rewrite, rewriteAt, cloneAt, gitLog)
			}
		})
	}

	// 2. Without one: no helper, no clone, no git call at all, as before.
	t.Run("none", func(t *testing.T) {
		home, _, gitLog := run("none")
		if _, err := os.Stat(filepath.Join(home, ".wardyn", "git-helper.secret")); err == nil {
			t.Error("exec task with no workspace provisioned a git helper secret")
		}
		if _, err := os.Stat(filepath.Join(home, "work")); err == nil {
			t.Error("exec task with no workspace made a work folder")
		}
		if gitLog != "" {
			t.Errorf("exec task with no workspace called git:\n%s", gitLog)
		}
	})
}
