// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

var defaultOnGrantSwitches = []struct {
	env   string
	loose func(*bootFlags) bool // the api.Config field the switch set off turns on
}{
	{"WARDYN_GOVERNANCE_UNIT_PUT_EXISTING_ONLY", (*bootFlags).unitPutCreates},
}

func bootFlagsWith(t *testing.T, env, val string) *bootFlags {
	t.Helper()
	if val == "" {
		ensureUnset(t, env)
	} else {
		t.Setenv(env, val)
	}
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = []string{"wardynd-test"}
	resetFlags(t)
	return parseBootFlags()
}

// Unset, each switch is on: api.Config gets the tight behaviour.
func TestDefaultOnGrantSwitches_UnsetIsOn(t *testing.T) {
	for _, sw := range defaultOnGrantSwitches {
		if sw.loose(bootFlagsWith(t, sw.env, "")) {
			t.Errorf("%s unset: the switch reads off, want on", sw.env)
		}
	}
}

// Set off, in any spelling the flag reads as false, each switch gives back the old behaviour; set on,
// it stays tight.
func TestDefaultOnGrantSwitches_ExplicitOffIsTheOldBehaviour(t *testing.T) {
	for _, sw := range defaultOnGrantSwitches {
		for _, tc := range []struct {
			val   string
			loose bool
		}{{"false", true}, {"0", true}, {"off", true}, {"no", true}, {"true", false}, {"1", false}} {
			if got := sw.loose(bootFlagsWith(t, sw.env, tc.val)); got != tc.loose {
				t.Errorf("%s=%s: old behaviour = %v, want %v", sw.env, tc.val, got, tc.loose)
			}
		}
	}
}

// A value that is not a boolean refuses boot with exit 2, naming the variable and the value.
// cliutil's exit is os.Exit, so the refusal runs in a re-exec'd child.
func TestDefaultOnGrantSwitches_MalformedRefusesBoot(t *testing.T) {
	if os.Getenv("GRANT_SWITCH_BOOT_CHILD") == "1" {
		resetFlags(t)
		os.Args = []string{"wardynd-test"}
		parseBootFlags()
		return // not refused: the child exits 0 and the parent fails
	}
	for _, sw := range defaultOnGrantSwitches {
		t.Run(sw.env, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestDefaultOnGrantSwitches_MalformedRefusesBoot$", "-test.timeout=60s")
			cmd.Env = append(os.Environ(), "GRANT_SWITCH_BOOT_CHILD=1", sw.env+"=treu")
			out, err := cmd.CombinedOutput()
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.ExitCode() != 2 {
				t.Fatalf("%s=treu: parseBootFlags must exit 2 at boot; err=%v\n%s", sw.env, err, out)
			}
			if !strings.Contains(string(out), sw.env) || !strings.Contains(string(out), "treu") {
				t.Fatalf("the refusal must name the variable and the value:\n%s", out)
			}
		})
	}
}
