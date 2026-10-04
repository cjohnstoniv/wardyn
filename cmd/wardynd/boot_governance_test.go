// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestParseBootFlagsGovernanceSettings pins the governance four-eyes boot settings: the switch itself
// boots (the covered set and the console are pending-aware, so the refusal an earlier build carried is
// gone), and a change TTL that is not positive is an exit 2 before anything is served. The refusal runs
// in a re-exec'd child because cliutil's exit is os.Exit.
func TestParseBootFlagsGovernanceSettings(t *testing.T) {
	if os.Getenv("GOVERNANCE_BOOT_CHILD") == "1" {
		resetFlags(t)
		os.Args = []string{"wardynd-test"}
		parseBootFlags()
		return // not refused: the child exits 0
	}
	run := func(env ...string) (int, string) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestParseBootFlagsGovernanceSettings$", "-test.timeout=60s")
		cmd.Env = append(os.Environ(), append([]string{"GOVERNANCE_BOOT_CHILD=1",
			"WARDYN_GOVERNANCE_SECOND_HUMAN=", "WARDYN_GOVERNANCE_CHANGE_TTL="}, env...)...)
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), string(out)
		}
		if err != nil {
			t.Fatalf("run child: %v\n%s", err, out)
		}
		return 0, string(out)
	}
	if code, out := run(); code != 0 {
		t.Fatalf("with neither set the boot exits %d, want 0:\n%s", code, out)
	}
	if code, out := run("WARDYN_GOVERNANCE_CHANGE_TTL=48h"); code != 0 {
		t.Fatalf("a positive TTL exits %d, want 0:\n%s", code, out)
	}
	// The switch boots, on or off.
	for _, v := range []string{"true", "1", "on", "false"} {
		if code, out := run("WARDYN_GOVERNANCE_SECOND_HUMAN=" + v); code != 0 {
			t.Errorf("WARDYN_GOVERNANCE_SECOND_HUMAN=%s exits %d, want 0:\n%s", v, code, out)
		}
	}
	for _, v := range []string{"0s", "-5m"} {
		code, out := run("WARDYN_GOVERNANCE_CHANGE_TTL=" + v)
		if code != 2 || !strings.Contains(out, "WARDYN_GOVERNANCE_CHANGE_TTL") {
			t.Errorf("WARDYN_GOVERNANCE_CHANGE_TTL=%s exits %d, want 2 naming the variable:\n%s", v, code, out)
		}
	}
	if code, out := run("WARDYN_GOVERNANCE_CHANGE_TTL=soon"); code != 2 || !strings.Contains(out, "WARDYN_GOVERNANCE_CHANGE_TTL") {
		t.Errorf("an unparsable TTL exits %d, want 2 naming the variable:\n%s", code, out)
	}
}
