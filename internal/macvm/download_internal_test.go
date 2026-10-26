// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type alwaysAC struct{}

func (alwaysAC) OnAC(context.Context) (bool, error) { return true, nil }

// A download that restarts with a non-empty partial file writes its next range after that partial, so the
// finished file is byte-identical to the image.
func TestDownloadResumesAfterThePartialBytes(t *testing.T) {
	body := make([]byte, 10_000)
	for i := range body {
		body[i] = byte(i * 7)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "image.ipsw", time.Time{}, bytes.NewReader(body))
	}))
	t.Cleanup(srv.Close)
	part := filepath.Join(t.TempDir(), "image.ipsw.part")
	if err := os.WriteFile(part, body[:4096], 0o600); err != nil {
		t.Fatal(err)
	}
	d := &downloader{client: srv.Client(), power: alwaysAC{}, poll: time.Millisecond, rng: 3000}
	if err := d.fetch(context.Background(), srv.URL, part); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(part)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("resumed download differs from the image: %d bytes, want %d", len(got), len(body))
	}
}
