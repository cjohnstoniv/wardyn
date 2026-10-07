// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func uploadErasureContract(t *testing.T, s recording.Store) {
	t.Helper()
	h := newRecordingHarness(t, s, nil)
	run := uuid.New()
	token := h.mintRunToken(t, run)
	for _, suffix := range []string{"", "/parts/2"} {
		body, writer := io.Pipe()
		defer body.Close()
		defer writer.Close()
		req := httptest.NewRequest(http.MethodPut, "/api/v1/internal/recordings/"+run.String()+suffix, body)
		req.Host, req.RemoteAddr = "127.0.0.1", "127.0.0.1:54321"
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			defer close(done)
			panicFails(t, h.srv.Handler()).ServeHTTP(w, req)
		}()
		written := make(chan error, 1)
		go func() { _, err := writer.Write([]byte("stream before erase")); written <- err }()
		select {
		case err := <-written:
			if err != nil {
				t.Fatal(err)
			}
		case <-done:
			t.Fatalf("upload ended before streaming: %d %s", w.Code, w.Body)
		case <-time.After(5 * time.Second):
			t.Fatal("upload did not read its body")
		}
		if _, err := s.(recording.RunDeleter).DeleteRun(t.Context(), run.String()); err != nil {
			t.Fatal(err)
		}
		_ = writer.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("late upload did not finish")
		}
		if w.Code != http.StatusGone || !strings.Contains(w.Body.String(), reasonRecordingErased) {
			t.Fatalf("late upload = %d %s", w.Code, w.Body)
		}
		if ev := lastAuditEvent(t, h.audit.events, "recording.upload"); ev.Outcome != "failure" || !strings.Contains(string(ev.Data), reasonRecordingErased) {
			t.Fatalf("late upload audit = %+v", ev)
		}
	}
	for _, ev := range h.audit.events {
		if ev.Action == "recording.upload" && ev.Outcome == "success" {
			t.Fatal("erased upload audited as success")
		}
	}
	if _, err := s.OpenCast(t.Context(), run.String()); !errors.Is(err, recording.ErrErased) {
		t.Fatalf("late upload persisted a cast: %v", err)
	}
	if code := h.uploadRecording(t, uuid.New(), "new run"); code != http.StatusNoContent {
		t.Fatalf("new run upload = %d", code)
	}
}

func TestUploadRecording_ErasureDuringStream(t *testing.T) {
	s, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	uploadErasureContract(t, s)
}

func TestPGUploadRecording_ErasureDuringStream(t *testing.T) {
	uploadErasureContract(t, recording.NewPGStore(throwawayPGPool(t)))
}

func TestUploadRecording_ErasurePreservesRefusalPriority(t *testing.T) {
	s, _ := recording.NewFSStore(t.TempDir())
	h := newRecordingHarness(t, s, nil)
	run := uuid.New()
	if _, err := s.DeleteRun(t.Context(), run.String()); err != nil {
		t.Fatal(err)
	}
	token := h.mintRunToken(t, run)
	path := "/api/v1/internal/recordings/" + run.String()
	for _, tc := range []struct {
		name, path, token, body string
		status                  int
	}{
		{"auth", path, "", "cast", http.StatusUnauthorized},
		{"cross-run", path, h.mintRunToken(t, uuid.New()), "cast", http.StatusForbidden},
		{"part", path + "/parts/" + strconv.Itoa(types.RecordingMaxParts+1), token, "cast", http.StatusRequestEntityTooLarge},
		{"size", path, token, strings.Repeat("x", maxRecordingUploadBytes+1), http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, h.srv, http.MethodPut, tc.path, tc.token, tc.body)
			if w.Code != tc.status || strings.Contains(w.Body.String(), reasonRecordingErased) {
				t.Fatalf("erasure displaced prior refusal: %d %s", w.Code, w.Body)
			}
		})
	}
	masked := newRecordingHarness(t, &fakeRecordingStore{saveErr: errMaskUncovered}, nil)
	w := do(t, masked.srv, http.MethodPut, path, masked.mintRunToken(t, run), "cast")
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), reasonRecordingErased) {
		t.Fatalf("mid-stream masking refusal = %d %s", w.Code, w.Body)
	}
}
