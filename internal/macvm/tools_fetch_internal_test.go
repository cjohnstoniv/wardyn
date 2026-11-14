// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Two updates that fetch the same pin into a shared cache at once both get the complete, checked bytes:
// the second waits for the first's download and reuses it, and neither touches the other's file.
func TestTwoFetchesOfOnePinShareTheCacheSafely(t *testing.T) {
	body := bytes.Repeat([]byte("claude-binary-2.1.310 "), 4096)
	sum := sha256.Sum256(body)
	first, release := make(chan struct{}), make(chan struct{})
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			// The first download stops half-way until the second fetch has started.
			_, _ = w.Write(body[:len(body)/2])
			w.(http.Flusher).Flush()
			close(first)
			<-release
			_, _ = w.Write(body[len(body)/2:])
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	pin := ToolPin{ID: HarnessClaudeCode, Version: "2.1.310", URL: srv.URL + "/claude", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body))}
	dir := t.TempDir()
	type res struct {
		b   []byte
		err error
	}
	fetch := func(out chan<- res) {
		f, err := fetchPinned(context.Background(), srv.Client(), pin, dir)
		if err != nil {
			out <- res{err: err}
			return
		}
		b, err := io.ReadAll(f)
		_ = f.Close()
		out <- res{b, err}
	}
	a, b := make(chan res, 1), make(chan res, 1)
	go fetch(a)
	<-first
	go fetch(b)
	time.Sleep(200 * time.Millisecond) // the second fetch, unserialised, would have finished by now
	close(release)
	for name, ch := range map[string]chan res{"first": a, "second": b} {
		r := <-ch
		if r.err != nil || !bytes.Equal(r.b, body) {
			t.Fatalf("%s fetch = %d bytes, %v; want the %d checked bytes", name, len(r.b), r.err, len(body))
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("%d downloads, want the second fetch to reuse the first's", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "claude-code-2.1.310.download")); err != nil {
		t.Fatalf("the checked download is not in the cache: %v", err)
	}
}
