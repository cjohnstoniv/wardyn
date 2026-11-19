// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestEventSink_ReadsAndRefreshesTokenFromFile covers the recovery path: the
// file-backed token source (WARDYN_GROUNDTRUTH_TOKEN_FILE) is the only wiring that
// survives the ~1h token TTL. The sink must seed its token from the file and, on
// refresh (the 401 path), re-read the file — so once the wardynd rotator rewrites it,
// the ingest recovers.
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
		t.Fatalf("sink must seed its token from the file, not the static env; got %q", got)
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
// of together: the boot guard must accept the wiring the compose stack and
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

// TestEventSink_RotatedFileIsReadAfterRejection is the whole recovery path on a file source:
// the sink holds the old token in memory, the file now holds a new one (a Secret mount updated
// by an atomic replace), the control plane rejects the old token, and the same batch is
// re-sent with the new one and accepted. Nothing reads the file until the rejection, so a
// sensor that was never rejected keeps using its cached token.
func TestEventSink_RotatedFileIsReadAfterRejection(t *testing.T) {
	const oldTok, newTok = "old-file-token", "new-file-token"
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte(oldTok), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	t.Setenv("WARDYN_GROUNDTRUTH_TOKEN_FILE", path)

	var rejected, accepted atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+newTok {
			rejected.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		accepted.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := newEventSink(srv.URL, "", 16, 8, 20*time.Millisecond, srv.Client())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		sink.close(ctx)
	})

	// The rotation lands: write beside, then rename over, as the kubelet swaps a projected file.
	next := filepath.Join(dir, "token.next")
	if err := os.WriteFile(next, []byte(newTok), 0o600); err != nil {
		t.Fatalf("write rotated file: %v", err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatalf("replace file: %v", err)
	}
	if sink.currentToken() != oldTok {
		t.Fatal("the sink re-read the file before any rejection; the cached token is what the test must see rejected")
	}

	sink.emit(types.AuditEvent{Action: "kernel.process.exec", Outcome: "success"})
	deadline := time.Now().Add(3 * time.Second)
	for sink.postedCount() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if sink.postedCount() < 1 || sink.droppedCount() != 0 {
		t.Fatalf("batch not accepted after rejection: posted=%d dropped=%d", sink.postedCount(), sink.droppedCount())
	}
	if rejected.Load() < 1 || accepted.Load() != 1 {
		t.Fatalf("want the cached token rejected at least once and the batch accepted once; rejected=%d accepted=%d", rejected.Load(), accepted.Load())
	}
	if sink.currentToken() != newTok {
		t.Fatal("the sink did not cache the re-read token")
	}
}
