// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// oracleAgentRunPath resolves deploy/images/oracle/agent-run relative to this
// package (go test's working directory is always the package dir) — the same
// resolution pattern agent_run_lib_test.go uses for agent-run-lib.sh.
const oracleAgentRunPath = "../../../deploy/images/oracle/agent-run"

// TestOracleAgentRun_IdleHoldsOpen pins W15-W15d-interactive-attach-3: both
// runners (driver.go, k8s/sandbox.go) launch `agent-run --idle` as the ENTIRE
// main process for every interactive run — never Exec'ing a task into it — so
// an image's agent-run MUST hold that process open, not exit. Before this fix
// the oracle image (the e2e stand-in) had no --idle branch: `--idle` fell
// through to normal task mode, where it is treated as a missing solution.sh
// path and exits 1 almost instantly, stranding an interactive run RUNNING
// with a dead main process and no attach target.
//
// Proof without a real container: run the actual script under `timeout`. A
// script that exits immediately reports ITS OWN exit code before the timeout
// fires; a script that holds the process open (the fix: `exec sleep infinity`)
// gets killed BY timeout, which reports 124. That distinction is the fix.
func TestOracleAgentRun_IdleHoldsOpen(t *testing.T) {
	scriptPath, err := filepath.Abs(oracleAgentRunPath)
	if err != nil {
		t.Fatalf("resolve oracle agent-run path: %v", err)
	}
	if _, err := os.Stat(scriptPath); err != nil {
		t.Fatalf("oracle agent-run not found at %s: %v", scriptPath, err)
	}

	out, err := exec.Command("timeout", "0.3s", "sh", scriptPath, "--idle").CombinedOutput()
	exitErr, isExit := err.(*exec.ExitError)
	if !isExit {
		t.Fatalf("unexpected error running agent-run --idle under timeout: %v\noutput: %s", err, out)
	}
	if code := exitErr.ExitCode(); code != 124 {
		t.Fatalf("agent-run --idle exited with code %d (output: %s), want 124 (timeout had to kill it) — "+
			"a non-124 exit means the idle process died on its own instead of holding the container open for attach",
			code, out)
	}
}
