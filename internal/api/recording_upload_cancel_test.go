// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

type stalledRecordingBody struct {
	closed chan struct{}
	once   sync.Once
	reads  atomic.Int32
}

func (b *stalledRecordingBody) Read([]byte) (int, error) {
	b.reads.Add(1)
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *stalledRecordingBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestUploadRecording_StorageFailureDoesNotWaitForBody(t *testing.T) {
	root := filepath.Join(t.TempDir(), "recordings")
	store, err := recording.NewFSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	// A directory replaced after startup makes SaveCast fail before reading.
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	runID := uuid.New()
	reg := secretmask.NewRegistry()
	reg.Add(runID, []byte("registered-recording-secret"))
	h := newRecordingHarness(t, store, reg)
	body := &stalledRecordingBody{closed: make(chan struct{})}
	defer body.Close()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/internal/recordings/"+runID.String(), body)
	req.Host, req.RemoteAddr = "127.0.0.1", "127.0.0.1:54321"
	req.Header.Set("Authorization", "Bearer "+h.mintRunToken(t, runID))
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.srv.Handler().ServeHTTP(w, req)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		// Release the synthetic source so the red case leaves no goroutine behind.
		_ = body.Close()
		<-done
		t.Fatal("storage already failed, but upload cleanup waited for the request body")
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if body.reads.Load() != 0 {
		t.Fatal("request body was read even though storage failed before consuming it")
	}
	if event := lastAuditEvent(t, h.audit.events, "recording.upload"); event.Outcome != "failure" {
		t.Fatalf("upload outcome = %q, want failure", event.Outcome)
	}
}
