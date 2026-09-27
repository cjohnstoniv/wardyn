// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package testlive

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestRequire pins Require's skip-vs-fail decision (#463) without needing
// `-tags live`: a Skipf and a Fatalf both call runtime.Goexit, so the only
// way to observe the real outcome — including the process exit code an
// operator actually sees — is through a genuine *testing.T. Each case
// re-execs this test binary, running only TestRequireProbe below, which
// calls Require for real.
func TestRequire(t *testing.T) {
	const probeGate = "WARDYN_TESTLIVE_REQUIRE_PROBE_GATE"
	const probeName = "WARDYN_TESTLIVE_REQUIRE_PROBE_NAME"

	cases := []struct {
		desc      string
		env       []string // extra env vars for the probe process, "K=V"
		wantExit0 bool
		wantSkip  bool
	}{
		{
			desc:      "gate unset skips",
			env:       nil,
			wantExit0: true,
			wantSkip:  true,
		},
		{
			desc:      "gate set, required name unset fails",
			env:       []string{probeGate + "=1"},
			wantExit0: false,
			wantSkip:  false,
		},
		{
			desc:      "gate set, required name set: neither",
			env:       []string{probeGate + "=1", probeName + "=x"},
			wantExit0: true,
			wantSkip:  false,
		},
	}

	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestRequireProbe$", "-test.v")
			cmd.Env = append(append([]string{}, os.Environ()...), "WARDYN_TESTLIVE_REQUIRE_PROBE=1")
			cmd.Env = append(cmd.Env, c.env...)
			out, err := cmd.CombinedOutput()

			if gotExit0 := err == nil; gotExit0 != c.wantExit0 {
				t.Fatalf("exit0=%v, want %v; output:\n%s", gotExit0, c.wantExit0, out)
			}
			if gotSkip := strings.Contains(string(out), "--- SKIP"); gotSkip != c.wantSkip {
				t.Fatalf("skip=%v, want %v; output:\n%s", gotSkip, c.wantSkip, out)
			}
		})
	}
}

// TestRequireProbe only does anything when re-exec'd by TestRequire above;
// left to run normally it just skips. It is the harness that drives Require
// through a live *testing.T so the real Skipf/Fatalf semantics apply.
func TestRequireProbe(t *testing.T) {
	if os.Getenv("WARDYN_TESTLIVE_REQUIRE_PROBE") != "1" {
		t.Skip("not running as a TestRequire probe")
	}
	Require(t, "WARDYN_TESTLIVE_REQUIRE_PROBE_GATE", "WARDYN_TESTLIVE_REQUIRE_PROBE_NAME")
}
