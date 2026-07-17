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
