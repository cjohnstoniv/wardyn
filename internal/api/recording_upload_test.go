// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

// fakeRecordingStore is a hand-rolled recording.Store used by the upload tests.
// SaveCast either drains the reader (default) or aborts the read side early to
// simulate a store/path error after only a partial read — the leak path for the
// masking pipe goroutine.
type fakeRecordingStore struct {
	// abortAfter, when > 0, makes SaveCast read at most abortAfter bytes then
	// return saveErr WITHOUT draining the rest of r (the abort path).
	abortAfter int
	saveErr    error
	saved      []byte
}

func (f *fakeRecordingStore) SaveCast(_ context.Context, _ string, r io.Reader) error {
	if f.abortAfter > 0 {
		buf := make([]byte, f.abortAfter)
		n, _ := io.ReadFull(r, buf)
		f.saved = buf[:n]
		if f.saveErr == nil {
			f.saveErr = errors.New("fake store: aborted read")
		}
		return f.saveErr
	}
	b, err := io.ReadAll(r)
	f.saved = b
	if err != nil {
		return err
	}
	return f.saveErr
}

func (f *fakeRecordingStore) SaveCastNamed(ctx context.Context, runID, suffix string, r io.Reader) error {
	return f.SaveCast(ctx, recording.CastKey(runID, suffix), r)
}

func (f *fakeRecordingStore) OpenCast(_ context.Context, _ string) (io.ReadCloser, error) {
	return nil, recording.ErrNotFound
}

// newRecordingHarness builds a harness whose Server has a RecordingStore wired
// at construction time. The internal recordings route is only mounted when
// RecordingStore is non-nil at New(), so it must be set up front (not mutated
// after the fact). The supplied MaskRegistry may be nil (pass-through masking).
func newRecordingHarness(t *testing.T, store recording.Store, reg *secretmask.Registry) *harness {
	t.Helper()
	h := newHarness(t)
	cfg := h.srv.cfg
	cfg.RecordingStore = store
	cfg.MaskRegistry = reg
	h.srv = New(cfg)
	return h
}

// uploadRecording mints a run token for runID and PUTs body to the internal
// recording upload endpoint, returning the response status code.
func (h *harness) uploadRecording(t *testing.T, runID uuid.UUID, body string) int {
	t.Helper()
	tok := h.mintRunToken(t, runID)
	w := do(t, h.srv, http.MethodPut, "/api/v1/internal/recordings/"+runID.String(), tok, body)
	return w.Code
}

// TestUploadRecording_SizeLimit (Finding 3): an authenticated agent must not be
// able to disk-exhaust the control plane with an unbounded cast upload. An
// over-cap body must be rejected with 413 and NOT persisted.
func TestUploadRecording_SizeLimit(t *testing.T) {
	store := &fakeRecordingStore{}
	h := newRecordingHarness(t, store, nil)

	// One byte over the cap.
	oversized := strings.Repeat("A", maxRecordingUploadBytes+1)
	if code := h.uploadRecording(t, uuid.New(), oversized); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload: code = %d, want 413", code)
	}

	// A within-cap body still succeeds (no false positive at the boundary).
	store2 := &fakeRecordingStore{}
	h2 := newRecordingHarness(t, store2, nil)
	atCap := strings.Repeat("B", maxRecordingUploadBytes)
	if code := h2.uploadRecording(t, uuid.New(), atCap); code != http.StatusNoContent {
		t.Fatalf("at-cap upload: code = %d, want 204", code)
	}
	if len(store2.saved) != maxRecordingUploadBytes {
		t.Errorf("at-cap upload persisted %d bytes, want %d", len(store2.saved), maxRecordingUploadBytes)
	}
}

// TestBuildMaskingBody_NoGoroutineLeakOnAbort (Finding 2): when SaveCast aborts
// the read side (reads only part of the body then errors), the masking pipe's
// copy goroutine must NOT leak — the handler must close the reader and await the
// writer so the goroutine unblocks instead of blocking forever on pw.Write.
func TestBuildMaskingBody_NoGoroutineLeakOnAbort(t *testing.T) {
	reg := secretmask.NewRegistry()
	runID := uuid.New()
	// Register a secret so buildMaskingBody takes the pipe/goroutine branch
	// (a non-empty snapshot is what triggers the masking pipe).
	reg.Add(runID, []byte("super-secret-token-value"))

	before := runtime.NumGoroutine()

	// Large body so the copy goroutine is still trying to write when the reader
	// is closed mid-stream (forces the blocked-write-on-no-reader scenario).
	src := strings.NewReader(strings.Repeat("x", 1<<20))
	body, cleanup := buildMaskingBody(src, reg, runID)

	// Read only a little, then abort like SaveCast would on an early error.
	buf := make([]byte, 16)
	if _, err := io.ReadFull(body, buf); err != nil {
		t.Fatalf("partial read: %v", err)
	}
	// The handler's abort path: drain+close the reader and await the writer.
	cleanup()

	// The copy goroutine must have exited; poll briefly to avoid flakes.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: before=%d now=%d (copy goroutine did not exit after cleanup)", before, runtime.NumGoroutine())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestUploadRecording_AbortingStoreNoLeak (Finding 2, end-to-end): drive the
// real handler with a MaskRegistry (so the pipe/goroutine masking path is taken)
// and a store that aborts mid-read. The handler must return (not hang) and leave
// no leaked copy goroutine behind — proving handleUploadRecording's deferred
// cleanup runs on the abort path.
func TestUploadRecording_AbortingStoreNoLeak(t *testing.T) {
	runID := uuid.New()
	reg := secretmask.NewRegistry()
	reg.Add(runID, []byte("super-secret-token-value"))
	store := &fakeRecordingStore{abortAfter: 8} // read 8 bytes then error

	h := newRecordingHarness(t, store, reg)

	before := runtime.NumGoroutine()
	// Body large enough that the copy goroutine would block on pw.Write once the
	// store stops reading after 8 bytes.
	big := strings.Repeat("z", 1<<20)
	code := h.uploadRecording(t, runID, big)
	if code != http.StatusInternalServerError {
		t.Fatalf("aborting store: code = %d, want 500", code)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("handler leaked a masking goroutine: before=%d now=%d", before, runtime.NumGoroutine())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
