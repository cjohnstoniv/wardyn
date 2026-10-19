// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"testing"
)

// A setup failure (here: the login wrote no token cache) still prints the fail marker on
// stdout before the helper exits 1, so the login pane ends instead of waiting out its watch.
// main exits the process, so it runs in a child copy of this test binary.
func TestMain_SetupFailurePrintsTheFailMarker(t *testing.T) {
	if os.Getenv("WARDYN_AWS_SSO_MAIN_CHILD") == "1" {
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMain_SetupFailurePrintsTheFailMarker$", "-test.count=1")
	cmd.Env = append(os.Environ(), "WARDYN_AWS_SSO_MAIN_CHILD=1", "HOME="+t.TempDir(),
		"WARDYN_PROXY_URL=http://127.0.0.1:1", "WARDYN_RUN_ID=00000000-0000-0000-0000-000000000001")
	out, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Fatalf("child exit = %v, want exit status 1; stdout:\n%s", err, out)
	}
	assertFailLine(t, string(out))
}
