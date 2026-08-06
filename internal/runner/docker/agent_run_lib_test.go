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

// agentRunLibPath resolves deploy/images/common/agent-run-lib.sh relative to
// this package (go test's working directory is always the package dir).
const agentRunLibPath = "../../../deploy/images/common/agent-run-lib.sh"

// TestAgentRunLib_MakeToolchainDirsCreatesGOTMPDIR is the shell-level sibling
// of TestAttach_OpensInteractiveShellNotTrackedAsAgentExec's GOTMPDIR pin: the
// attach shell's inline guard (session.go's attachShell) and agent-run-lib.sh's
// make_toolchain_dirs are the SAME contract on two runtime paths — this run
// actually sources the real script and calls the real function (no daemon
// needed, so it stays cheap), proving it creates GOTMPDIR rather than merely
// pattern-matching the guard's source text.
func TestAgentRunLib_MakeToolchainDirsCreatesGOTMPDIR(t *testing.T) {
	libPath, err := filepath.Abs(agentRunLibPath)
	if err != nil {
		t.Fatalf("resolve agent-run-lib.sh path: %v", err)
	}
	if _, err := os.Stat(libPath); err != nil {
		t.Fatalf("agent-run-lib.sh not found at %s: %v", libPath, err)
	}
	target := filepath.Join(t.TempDir(), "gotmp")

	// "$0" absorbs bash -c's own script-name slot so $1/$2 are libPath/target,
	// never the -c string itself.
	out, err := exec.Command("bash", "-c",
		`set -euo pipefail; source "$1"; GOTMPDIR="$2" make_toolchain_dirs`,
		"agent-run-lib-test", libPath, target,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("make_toolchain_dirs failed: %v\noutput: %s", err, out)
	}
	if fi, statErr := os.Stat(target); statErr != nil || !fi.IsDir() {
		t.Fatalf("GOTMPDIR %q was not created by make_toolchain_dirs (stat err: %v)", target, statErr)
	}
}

// TestAgentRunLib_MakeToolchainDirsNoopWhenUnset asserts the guard's other
// half: an env without GOTMPDIR set (a run whose scans found no Go) does
// nothing — no error, no directory conjured from nothing.
func TestAgentRunLib_MakeToolchainDirsNoopWhenUnset(t *testing.T) {
	libPath, err := filepath.Abs(agentRunLibPath)
	if err != nil {
		t.Fatalf("resolve agent-run-lib.sh path: %v", err)
	}
	out, err := exec.Command("bash", "-c",
		`set -euo pipefail; unset GOTMPDIR; source "$1"; make_toolchain_dirs; echo ok`,
		"agent-run-lib-test", libPath,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("make_toolchain_dirs (unset GOTMPDIR) failed: %v\noutput: %s", err, out)
	}
	if got := string(out); got != "ok\n" {
		t.Errorf("output = %q, want just \"ok\\n\" (no side effects when GOTMPDIR is unset)", got)
	}
}
