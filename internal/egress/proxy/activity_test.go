// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestConnectTunnel_BytesCountAsActivity: bytes moved through an opaque
// CONNECT tunnel mark the run in use, which is what keeps a long download or a
// model stream from looking like nobody is there. The CONNECT itself does not.
func TestConnectTunnel_BytesCountAsActivity(t *testing.T) {
	p, _ := newTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"tls.test"}}, startEcho(t), nil, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	conn, status := connectThrough(t, proxySrv.URL, "tls.test:443")
	defer conn.Close()
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT response = %q, want 200", status)
	}
	if p.streamMoved.Load() {
		t.Fatal("the CONNECT alone marked the run active")
	}
	_, _ = io.WriteString(conn, "ping")
	buf := make([]byte, 4)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !p.streamMoved.Load() {
		t.Error("bytes through the tunnel did not mark the run active")
	}
}

// TestActivityReporter_ReportsOnlyAfterBytesMoved: the reporter posts to the
// control plane's activity route, with the run token, only on a tick after
// bytes moved, and clears the mark when it does.
func TestActivityReporter_ReportsOnlyAfterBytesMoved(t *testing.T) {
	var (
		mu    sync.Mutex
		posts []string
	)
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		posts = append(posts, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer cp.Close()
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(posts) }

	var moved atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runActivityReporter(ctx, &moved, newTokenSource("run-tok"), cp.URL, cp.Client(), 10*time.Millisecond)

	time.Sleep(100 * time.Millisecond)
	if n := count(); n != 0 {
		t.Fatalf("posts = %d with no bytes moved, want 0", n)
	}
	moved.Store(true)
	deadline := time.Now().Add(3 * time.Second)
	for count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posts) != 1 || posts[0] != "POST /api/v1/internal/activity Bearer run-tok" {
		t.Fatalf("posts = %q, want one POST /api/v1/internal/activity with the run token", posts)
	}
	if moved.Load() {
		t.Error("the mark was not cleared by the report")
	}
}
