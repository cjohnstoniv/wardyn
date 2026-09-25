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

// TestParseBootFlagsRefusesGarbageEnvToggle pins #202's boot refusal for the
// toggles internal/api reads per request. Without api.ValidateEnvToggles in
// parseBootFlags, WARDYN_EGRESS_SECOND_HUMAN=treu on an SSO deployment booted
// cleanly and then exited 2 on the first approval decision. parseBootFlags is
// the first step of run(), so exit 2 here is exit 2 before anything is served.
//
// cliutil's exit is os.Exit, so the refusal runs in a re-exec'd child.
func TestParseBootFlagsRefusesGarbageEnvToggle(t *testing.T) {
	if os.Getenv("ENV_TOGGLE_BOOT_CHILD") == "1" {
		resetFlags(t)
		os.Args = []string{"wardynd-test"}
		parseBootFlags()
		return // not refused: the child exits 0 and the parent fails
	}
	for _, name := range []string{
		"WARDYN_EGRESS_SECOND_HUMAN",
		"WARDYN_ALLOW_MEMBER_ENV_SECRET",
		"WARDYN_ALLOW_AGENT_TELEMETRY",
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestParseBootFlagsRefusesGarbageEnvToggle$", "-test.timeout=60s")
			cmd.Env = append(os.Environ(), "ENV_TOGGLE_BOOT_CHILD=1", name+"=treu")
			out, err := cmd.CombinedOutput()
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.ExitCode() != 2 {
				t.Fatalf("%s=treu: parseBootFlags must exit 2 at boot; err=%v\n%s", name, err, out)
			}
			if !strings.Contains(string(out), name) || !strings.Contains(string(out), "treu") {
				t.Fatalf("the refusal must name the variable and the value:\n%s", out)
			}
		})
	}
}
