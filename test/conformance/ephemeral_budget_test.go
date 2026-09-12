// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package conformance

// ephemeral_budget_test.go is an INTERNAL test (package conformance, not
// conformance_test) because both things it asserts are unexported: the eviction
// case's budget arithmetic and the fill script's exit-code contract. It needs no
// cluster, no Docker and no tag, so it runs under `make test-conformance-stub`
// alongside none_test.go.

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// makefileTimeoutRe reads the `-timeout <dur>` of a `go test` line. The k8s
// conformance recipe is found by its WARDYN_TEST_K8S=1 prefix rather than by line
// number, so re-ordering the Makefile does not silently disarm this.
var makefileTimeoutRe = regexp.MustCompile(`-timeout\s+(\S+)`)

// TestEphemeralCaseBudgetFitsTheMakefileTimeout is the arithmetic finding 3 was
// about: one sub-case could wait 3m for r.Wait, 4m for the eviction, and a
// further FRESH 4m for the Status poll — 11m against a package `-timeout 10m`. A
// Go -timeout expiry is not a red case; it is a panic that kills the package and
// discards every verdict the other seven cases already produced. So the failure
// mode of a slow eviction was a LOST conformance run.
//
// It reads both numbers rather than restating either: the budget from the code
// (ephemeralCaseBudget) and the ceiling from the Makefile recipe that actually
// runs this suite. Raising one without the other reds here.
func TestEphemeralCaseBudgetFitsTheMakefileTimeout(t *testing.T) {
	b, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	var timeout time.Duration
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.Contains(line, "WARDYN_TEST_K8S=1") || !strings.Contains(line, "./test/conformance/") {
			continue
		}
		m := makefileTimeoutRe.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("the k8s conformance recipe carries no -timeout: %q — this case can outrun go test's 10m default", line)
		}
		if timeout, err = time.ParseDuration(m[1]); err != nil {
			t.Fatalf("parse -timeout %q: %v", m[1], err)
		}
		break
	}
	if timeout == 0 {
		t.Fatal("no `WARDYN_TEST_K8S=1 go test … ./test/conformance/…` recipe found in the Makefile — " +
			"re-point this guard at the target that runs the k8s conformance suite rather than deleting it")
	}

	// Default options: what `make test-conformance-k8s` runs with unless a driver
	// harness overrides Timeout, and the shape conformance_k8s_test.go uses (3m).
	for _, opts := range []Options{{}, {Timeout: 3 * time.Minute}} {
		if got := ephemeralCaseBudget(opts); got >= timeout {
			t.Errorf("the eviction case may run %s against `go test -timeout %s` (Options.Timeout=%s): a -timeout "+
				"expiry PANICS the package and throws away every verdict already produced, so the whole "+
				"conformance run is lost rather than one case going red. Either shrink the budget or raise the "+
				"Makefile's -timeout.", got, timeout, opts.timeout())
		}
	}
}

// TestEphemeralFillScriptCodesAreDistinct pins the fill script's exit-code
// contract, which is what the case BRANCHES on — 90 skips as an image verdict, 91
// as an environment one, and anything else is read as "the fill ran".
//
// It is a source assertion and not an execution: running busybox here would need
// Docker, and the thing that broke was not the shell but WHERE it wrote. So the
// two facts asserted are the two that were wrong: the target is not $HOME (which
// is `/` for uid 1000 on the agent image, and root-owned), and the open is probed
// under its own code so an unwritable target cannot be reported as a substrate
// that does not enforce.
func TestEphemeralFillScriptCodesAreDistinct(t *testing.T) {
	if strings.Contains(ephemeralFillScript, "$HOME") {
		t.Error("the fill writes under $HOME, which runc answers as `/` for the agent image's uid 1000 — " +
			"root-owned, so every fill dies `Permission denied` with exit 1, the coded skip never fires, and " +
			"the case reports a substrate that does not enforce. Write under /tmp.")
	}
	if !strings.Contains(ephemeralFillScript, "/tmp/") {
		t.Error("the fill target is not under /tmp — it must be a path that is writable in every image this " +
			"gate runs against AND metered by the kubelet as local ephemeral storage")
	}
	for _, code := range []string{"exit 90", "exit 91"} {
		if strings.Count(ephemeralFillScript, code) != 1 {
			t.Errorf("the fill script names %q %d times, want exactly once — 90 is the missing-applet IMAGE "+
				"verdict and 91 the unwritable-target ENVIRONMENT one, and the case skips on both",
				code, strings.Count(ephemeralFillScript, code))
		}
	}
	// The zero-byte probe is what separates "cannot open" from "out of space":
	// ENOSPC is the PASS path here, so folding the two would hide an eviction
	// behind an environment skip.
	if !strings.Contains(ephemeralFillScript, "count=0") {
		t.Error("the open is not probed separately (no count=0 create) — a single dd cannot tell EACCES from " +
			"ENOSPC, and ENOSPC is this case's pass path")
	}
}
