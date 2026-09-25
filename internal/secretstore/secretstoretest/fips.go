// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretstoretest

import (
	"crypto/fips140"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const fipsChildEnv = "WARDYN_TEST_FIPS_CHILD"

// UnderFIPSOnly runs the calling test again in a child process started with
// GODEBUG=fips140=only, which Go reads only at process start. It returns true
// in that child, where the test does its work, and false in the parent once the
// child has passed.
//
//	if !secretstoretest.UnderFIPSOnly(t) {
//		return
//	}
func UnderFIPSOnly(t *testing.T) bool {
	t.Helper()
	if os.Getenv(fipsChildEnv) == "1" {
		if !fips140.Enforced() {
			t.Fatal("the child is not running with GODEBUG=fips140=only")
		}
		return true
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), "GODEBUG=fips140=only", fipsChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s under GODEBUG=fips140=only: %v\n%s", t.Name(), err, out)
	}
	if !strings.Contains(string(out), "--- PASS: "+t.Name()) {
		t.Fatalf("%s did not run under GODEBUG=fips140=only:\n%s", t.Name(), out)
	}
	return false
}
