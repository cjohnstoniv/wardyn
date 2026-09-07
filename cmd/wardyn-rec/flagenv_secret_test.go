// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// TestRunToken_NeverPrintedInUsage is the wardyn-rec half of F157. `-run-token`
// took its default from os.Getenv("WARDYN_RUN_TOKEN"); flag captures whatever
// default it is handed as Flag.DefValue, and the FlagSet's usage block —
// printed on -help and on ANY parse error — renders a non-empty string default
// as `(default "…")`, putting the live run token into whatever collects this
// sidecar's stderr. The flag still reads the env; only the printed DEFAULT
// changed.
func TestRunToken_NeverPrintedInUsage(t *testing.T) {
	const secret = "wrt_SUPER_SECRET_RUN_TOKEN_9f3a"
	t.Setenv("WARDYN_RUN_TOKEN", secret)

	for _, args := range [][]string{{"-help"}, {"-nosuchflag"}} {
		stderr := captureStderr(t, func() {
			if err := run(args); err == nil {
				t.Errorf("run(%v) = nil, want a flag error", args)
			}
		})
		if strings.Contains(stderr, secret) {
			t.Errorf("run(%v) printed the run token to stderr:\n%s", args, stderr)
		}
	}
}

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
// The FlagSet in run() uses flag's default output, which resolves os.Stderr at
// write time, so swapping it here captures the usage block.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	os.Stderr = saved
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}
