// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestEventSink_ReadsAndRefreshesTokenFromFile is the U009 recovery-path regression:
// the file-backed token source (WARDYN_GROUNDTRUTH_TOKEN_FILE) is the ONLY wiring that
// survives the ~1h token TTL, and its disk-reading branch had zero test coverage. The
// sink must seed its token from the file AND, on refresh (the 401 path), re-read the
// file — so once the wardynd rotator rewrites it, the ingest recovers.
func TestEventSink_ReadsAndRefreshesTokenFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gt-token")
	if err := os.WriteFile(path, []byte("file-token-1\n"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	t.Setenv("WARDYN_GROUNDTRUTH_TOKEN_FILE", path)

	sink := newEventSink("http://x", "unused-static-env", 4, 2, time.Hour, &http.Client{})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		sink.close(ctx)
	})

	if got := sink.currentToken(); got != "file-token-1" {
		t.Fatalf("sink must seed its token from the file, not the static env (U009); got %q", got)
	}

	// The rotator rewrites the file with a fresh token; the 401 refresh re-reads it.
	if err := os.WriteFile(path, []byte("file-token-2\n"), 0o600); err != nil {
		t.Fatalf("rotate file: %v", err)
	}
	if got := sink.refreshToken(); got != "file-token-2" {
		t.Fatalf("refreshToken must re-read the rotated file (the blind-recovery path); got %q", got)
	}
}

// TestBootTokenGuard_AcceptsRotatorTokenFileWithEmptyEnvToken pins the two halves
// of U009 together: the boot guard must accept the wiring the compose stack and
// README ship as the DEFAULT — rotator-written token file, WARDYN_GROUNDTRUTH_TOKEN
// deliberately empty. A guard that only consults the env token exits 1 at boot, so
// the ground-truth stream is dead at t=0 rather than merely blind after the ~1h TTL.
func TestBootTokenGuard_AcceptsRotatorTokenFileWithEmptyEnvToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gt-token")
	if err := os.WriteFile(path, []byte("rotator-token\n"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	t.Setenv("WARDYN_GROUNDTRUTH_TOKEN_FILE", path)
	t.Setenv("WARDYN_GROUNDTRUTH_TOKEN", "")

	if err := checkBootTokenSource(""); err != nil {
		t.Fatalf("boot guard must accept the rotator token file with an empty env token (the documented default); got %v", err)
	}
}

// TestBootTokenGuard_AcceptsNotYetWrittenTokenFile covers compose start ordering:
// the sidecar starts when wardynd is healthy, which can precede the rotator's first
// write. A configured-but-absent file must not kill the sidecar — the sink's 401
// refresh re-reads it, and the gap is counted as drops.
func TestBootTokenGuard_AcceptsNotYetWrittenTokenFile(t *testing.T) {
	t.Setenv("WARDYN_GROUNDTRUTH_TOKEN_FILE", filepath.Join(t.TempDir(), "not-written-yet"))
	t.Setenv("WARDYN_GROUNDTRUTH_TOKEN", "")

	if err := checkBootTokenSource(""); err != nil {
		t.Fatalf("boot guard must tolerate a configured token file the rotator has not written yet; got %v", err)
	}
}

// TestBootTokenGuard_FailsClosedWithNeitherSource is the fail-closed half: with no
// token source configured at all the sensor must refuse to start rather than run
// unauthenticated and 401 every POST while looking alive.
func TestBootTokenGuard_FailsClosedWithNeitherSource(t *testing.T) {
	t.Setenv("WARDYN_GROUNDTRUTH_TOKEN_FILE", "")
	t.Setenv("WARDYN_GROUNDTRUTH_TOKEN", "")

	if err := checkBootTokenSource(""); err == nil {
		t.Fatal("boot guard must fail closed when neither a token nor a token file is configured")
	}
}
