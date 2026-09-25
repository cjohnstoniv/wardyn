// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
// same signal ccRunIdle/runIdle/cxRunIdle wait for, via `timeout`, in the
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
	if err := cmd.Start(); err != nil {
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
// ccRunIdle and cxRunIdle build, minus the `timeout` wrapper this test replaces
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
// runIdle builds, minus the `timeout` wrapper.
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
