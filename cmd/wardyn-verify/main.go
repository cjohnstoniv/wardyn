// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardyn-verify is Wardyn's in-sandbox environment VERIFY step. It runs
// INSIDE a governed verify run (launchVerifyRun), in the workspace's BUILT
// devcontainer image, executes the OPERATOR-APPROVED setup commands
// (install → build → test/lint) in order, captures each step's exit code +
// a bounded head+tail of its combined output, and ships the VerifyResult back
// to the control plane so the workspace's verified state is DERIVED
// control-plane-side (result-out, not authority-out).
//
// The approved commands arrive via WARDYN_VERIFY_COMMANDS (JSON
// []workspacescan.SetupCommand) — non-secret, operator-authored. Secrets are
// NEVER in the sandbox env: the broker proxy-injects api-keys and brokers git
// creds at request time, so the setup commands reach their registries/hosts
// through the wardyn-proxy without any secret ever being resident here.
//
// Upload contract (mirrors wardyn-scan — the proxy injects the run token):
//
//	PUT ${WARDYN_PROXY_URL}/wardyn/v1/verify-results/${WARDYN_RUN_ID}
//	body: json(workspacescan.VerifyResult)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
	"github.com/cjohnstoniv/wardyn/internal/sidecar"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

const (
	defaultWorkspaceDir = "/home/agent/work"
	perCommandTimeout   = 15 * time.Minute
	totalTimeout        = 40 * time.Minute
	logHeadCap          = 4 << 10
	logTailCap          = 4 << 10
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "wardyn-verify:", err)
		os.Exit(1)
	}
}

func run() error {
	dir := cliutil.EnvOr("WARDYN_WORKSPACE_DIR", defaultWorkspaceDir)
	url, err := sidecar.ProxyRunURL("verify")
	if err != nil {
		return err
	}

	// Go requires GOTMPDIR/GOCACHE to EXIST. dispatch points them at the agent's
	// exec-allowed HOME (the sandbox /tmp is noexec, so `go test` can't exec its
	// test binaries there) — but a generic/user image won't have pre-created the
	// dirs, so ensure them here. Best-effort: a non-Go workspace is unaffected.
	for _, k := range []string{"GOTMPDIR", "GOCACHE"} {
		if p := os.Getenv(k); p != "" {
			_ = os.MkdirAll(p, 0o755)
		}
	}

	var cmds []workspacescan.SetupCommand
	if raw := os.Getenv("WARDYN_VERIFY_COMMANDS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cmds); err != nil {
			return fmt.Errorf("parse WARDYN_VERIFY_COMMANDS: %w", err)
		}
	}

	// execute streams PROGRESS uploads (Done=false) as each step starts/finishes,
	// then returns the final result which we upload with Done=true. Every upload
	// is best-effort: a failed progress upload just leaves the UI a beat behind.
	result := execute(dir, cmds, func(partial workspacescan.VerifyResult) {
		if b, err := json.Marshal(partial); err == nil {
			_ = sidecar.Upload(url, b) // best-effort progress
		}
	})

	body, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal verify result: %w", err)
	}
	if derr := sidecar.Upload(url, body); derr != nil {
		// Non-fatal: the run already ran; a failed FINAL upload leaves the
		// workspace "verifying" (an honest signal) rather than crashing the run.
		fmt.Fprintln(os.Stderr, "wardyn-verify: result upload failed (non-fatal):", derr)
	}
	return nil
}

// execute runs each approved command in order with a per-command and a total
// time budget, stopping the sequence on the first failure (a build can't be
// meaningfully tested if install failed). It calls onProgress with a Done=false
// snapshot as each step STARTS (so the UI shows the running step) and after each
// step FINISHES (so the UI shows its result), then returns the Done=true result.
func execute(dir string, cmds []workspacescan.SetupCommand, onProgress func(workspacescan.VerifyResult)) workspacescan.VerifyResult {
	total := len(cmds)
	var completed []workspacescan.VerifyStepResult
	ok := total > 0
	deadline := time.Now().Add(totalTimeout)
	progress := func(running *workspacescan.VerifyStepResult, done bool) {
		steps := append([]workspacescan.VerifyStepResult(nil), completed...)
		if running != nil {
			steps = append(steps, *running)
		}
		snap := workspacescan.VerifyResult{Steps: steps, OK: ok, Ran: total > 0, Total: total, Done: done}
		if onProgress != nil {
			onProgress(snap)
		}
	}
	for _, c := range cmds {
		if time.Now().After(deadline) {
			completed = append(completed, workspacescan.VerifyStepResult{
				Stage: c.Stage, Command: c.Command, ExitCode: -1, TimedOut: true,
				LogTail: "skipped: total verify time budget exceeded",
			})
			ok = false
			break
		}
		// Announce the step as RUNNING so the UI shows "building… (N/total)".
		progress(&workspacescan.VerifyStepResult{Stage: c.Stage, Command: c.Command, Running: true}, false)
		step := runStep(dir, c)
		completed = append(completed, step)
		if step.ExitCode != 0 || step.TimedOut {
			ok = false
			progress(nil, false) // publish the failed step before stopping
			break                // stop at the first failing stage
		}
		progress(nil, false) // publish the completed step
	}
	return workspacescan.VerifyResult{Steps: completed, OK: ok, Ran: total > 0, Total: total, Done: true}
}

func runStep(dir string, c workspacescan.SetupCommand) workspacescan.VerifyStepResult {
	ctx, cancel := context.WithTimeout(context.Background(), perCommandTimeout)
	defer cancel()
	// Operator-approved command; run via a NON-login shell (`-c`, not `-lc`).
	// A login shell sources /etc/profile which RESETS PATH to the system default,
	// dropping the image's `ENV PATH` — the fat image's go/rust-not-found failure.
	// `-c` is the docker-native behavior (what `docker exec`/devcontainer features
	// expect): the image's ENV PATH + our sandboxEnv (GOTMPDIR, MAVEN_OPTS, …) are
	// inherited intact. CI=true keeps interactive tools non-blocking; the sandbox
	// confinement + egress proxy bound what it can do.
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", c.Command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CI=true", "TERM=dumb", "GIT_TERMINAL_PROMPT=0")
	collector := newRollingCollector(logHeadCap, logTailCap)
	cmd.Stdout = collector
	cmd.Stderr = collector
	// A lingering background child (e.g. the command backgrounds a daemon that
	// inherits the stdout/stderr pipes) would otherwise hold Wait() open
	// forever even after our own process exits or the context is canceled —
	// the per-command timeout above bounds the context, not the pipe read.
	// WaitDelay makes Wait() give up on the pipe copies 10s after the process
	// itself exits, so the step always terminates.
	cmd.WaitDelay = 10 * time.Second

	start := time.Now()
	err := cmd.Run()
	dur := time.Since(start).Milliseconds()

	step := workspacescan.VerifyStepResult{
		Stage: c.Stage, Command: c.Command, DurationMs: dur,
		LogHead: collector.head(), LogTail: collector.tail(),
	}
	if ctx.Err() == context.DeadlineExceeded {
		step.TimedOut = true
		step.ExitCode = -1
		return step
	}
	if err == nil {
		step.ExitCode = 0
		return step
	}
	if ee, ok := err.(*exec.ExitError); ok {
		step.ExitCode = ee.ExitCode()
	} else {
		step.ExitCode = -1
		step.LogTail += "\n" + err.Error()
	}
	return step
}
