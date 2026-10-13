// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// shutdownDrainFixture is a Server whose HTTP side is held open by one sandbox
// connection and whose last audit row (a private-ip streak summary) is only
// produced by Shutdown's own memo flush, after the HTTP phase has ended. The
// control plane is slow to accept it, so the row lands only if the sink drain
// still has time of its own.
func shutdownDrainFixture(t *testing.T) (srv *Server, rows *atomic.Int32) {
	t.Helper()
	rows = new(atomic.Int32)
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(40 * time.Millisecond)
		if b, _ := io.ReadAll(r.Body); strings.Contains(string(b), "builtin:private-ip") {
			rows.Add(1)
		}
	}))
	t.Cleanup(cp.Close)

	sink := newDecisionSink(cp.URL, newTokenSource("t"), 16, cp.Client(), io.Discard)
	p := newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{}),
		Sink:     sink,
		Resolver: publicResolver{},
	})
	req := egress.Request{Host: "internal.example", Port: 443, Time: time.Now()}
	p.privateIPRefused(req, blockPrivate)
	if !p.privateIPMemoHit(req.Host, req.Port) {
		t.Fatal("the repeat did not hit the memo, so the flush has no row to emit")
	}

	entered, release := make(chan struct{}), make(chan struct{})
	srv = &Server{proxy: p, sink: sink, http: &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	})}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.http.Serve(ln) }()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { close(release); _ = conn.Close() })
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	<-entered
	return srv, rows
}

// TestShutdownKeepsADrainReserveForTheSink: a sandbox connection that never
// closes makes http.Shutdown use all it is given, so a bare ctx would leave the
// sink no time to post the rows the flush just queued.
func TestShutdownKeepsADrainReserveForTheSink(t *testing.T) {
	srv, rows := shutdownDrainFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	if err := srv.Shutdown(ctx); err == nil {
		t.Error("Shutdown reported success with a sandbox connection still held open")
	}
	if rows.Load() != 1 {
		t.Errorf("the control plane received %d flushed rows by the time Shutdown returned, want 1", rows.Load())
	}
}

// TestShutdownDrainsTheSinkAfterTheCallerIsCancelled: a cancelled ctx ends the
// HTTP phase at once but must not cut the sink drain short.
func TestShutdownDrainsTheSinkAfterTheCallerIsCancelled(t *testing.T) {
	srv, rows := shutdownDrainFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.http.RegisterOnShutdown(cancel)
	if err := srv.Shutdown(ctx); err == nil {
		t.Error("Shutdown reported success with a sandbox connection still held open")
	}
	if rows.Load() != 1 {
		t.Errorf("the control plane received %d flushed rows by the time Shutdown returned, want 1", rows.Load())
	}
}
