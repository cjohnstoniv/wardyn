// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// http.Client.Timeout is a WHOLE-REQUEST deadline: it keeps running while the
// response BODY is read. GetRecording streams a .cast, which can be megabytes
// over a slow link, so the CLI's 30s poll bound (cmd/wardyn/main.go, sized for
// `run --wait`'s ~900 JSON polls) cut the download off mid-stream — leaving a
// truncated .cast that only fails much later, in a player.
//
// This drives GetRecording under a client with a SHORT whole-request timeout
// against a server that trickles the body past it. The download must complete:
// a streamed body is bounded by the caller's context, not by the deadline that
// exists to stop a hung JSON poll.
func TestGetRecording_SlowBodyIsNotCutOffByTheRequestTimeout(t *testing.T) {
	const chunks, chunkSize, delay = 8, 4096, 30 * time.Millisecond
	const total = chunks * chunkSize

	runID := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-asciicast")
		w.WriteHeader(http.StatusOK)
		buf := make([]byte, chunkSize)
		for i := range buf {
			buf[i] = 'a'
		}
		for i := 0; i < chunks; i++ {
			if _, err := w.Write(buf); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(delay)
		}
	}))
	t.Cleanup(srv.Close)

	// 100ms is well under the ~240ms the body takes: on the old code the copy
	// dies partway with "context deadline exceeded (Client.Timeout ...)".
	c := &client.Client{BaseURL: srv.URL, Token: testToken, HTTPClient: &http.Client{Timeout: 100 * time.Millisecond}}
	rc, err := c.GetRecording(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRecording: %v", err)
	}
	defer rc.Close()
	n, err := io.Copy(io.Discard, rc)
	if err != nil {
		t.Fatalf("read %d of %d bytes then failed: %v — the whole-request timeout cut the stream", n, total, err)
	}
	if n != total {
		t.Fatalf("read %d bytes, want %d — the recording arrived truncated", n, total)
	}
}
