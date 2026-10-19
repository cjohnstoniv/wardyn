// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package testlive

import (
	"os"
	"os/exec"
	"path/filepath"
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

// ReadSecretFile refuses a file that is inside the checkout, whatever path it is named by (#2699): a
// symlink from outside the repo into it is judged by where it points, and a file outside is read.
func TestReadSecretFile_JudgesTheRealPath(t *testing.T) {
	const env = "WARDYN_TESTLIVE_SECRET_PROBE"
	dir := t.TempDir()
	inside := filepath.Join(repoRoot(), "go.mod")
	link := filepath.Join(dir, "looks-outside")
	if err := os.Symlink(inside, link); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "secret")
	if err := os.WriteFile(outside, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		path    string
		wantErr string
	}{
		"a path in the repo":                   {inside, "inside the repository"},
		"a symlink from outside into the repo": {link, "inside the repository"},
		"a file outside":                       {outside, ""},
		"a file that does not exist":           {filepath.Join(dir, "missing"), "not readable"},
	} {
		t.Setenv(env, tc.path)
		b, err := ReadSecretFile(env)
		switch {
		case tc.wantErr == "" && (err != nil || string(b) != "s3cret"):
			t.Errorf("%s: got %q, %v; want the file read", name, b, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: err = %v, want it to contain %q", name, err, tc.wantErr)
		case err != nil && (strings.Contains(err.Error(), tc.path) || strings.Contains(err.Error(), "s3cret")):
			t.Errorf("%s: the error names the path or the contents: %v", name, err)
		}
	}
}
