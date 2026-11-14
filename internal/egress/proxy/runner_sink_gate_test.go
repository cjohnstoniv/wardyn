// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// tlsRunnerd is wardyn-runnerd's decision sink over real TLS, as a proxy built from its Config dials it:
// it answers each hello with accept, then grants credit once release is closed, then acks every record,
// and asks for a drain once drain is closed. got holds the records and drained replies it reads.
type tlsRunnerd struct {
	cfg     DecisionSinkConfig
	hello   chan struct{} // a session was accepted
	release chan struct{} // close to send the grant
	drain   chan struct{} // close to send a drain
	got     chan SinkFrame
	done    <-chan struct{}
}

func newTLSRunnerd(t *testing.T) *tlsRunnerd {
	t.Helper()
	cert, pin := selfSigned(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{SinkALPN}, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	r := &tlsRunnerd{cfg: DecisionSinkConfig{URL: "tls://" + ln.Addr().String(), SPKISHA256: pin, Key: testKey()},
		hello: make(chan struct{}, 1), release: make(chan struct{}), drain: make(chan struct{}), got: make(chan SinkFrame, 16),
		done: t.Context().Done()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go r.serve(c)
		}
	}()
	return r
}

func (r *tlsRunnerd) serve(c net.Conn) {
	defer c.Close()
	if f, err := ReadSinkFrame(c); err != nil || f.T != FrameHello {
		return
	}
	if WriteSinkFrame(c, SinkFrame{T: FrameAccept, Gen: 1}) != nil {
		return
	}
	select {
	case r.hello <- struct{}{}:
	default:
	}
	select {
	case <-r.release:
	case <-r.done:
		return
	}
	var wmu sync.Mutex
	write := func(f SinkFrame) error { wmu.Lock(); defer wmu.Unlock(); return WriteSinkFrame(c, f) }
	if write(SinkFrame{T: FrameGrant, Gen: 1, Through: 100, Bytes: 1 << 30}) != nil {
		return
	}
	go func() {
		select {
		case <-r.drain:
			_ = write(SinkFrame{T: FrameDrain, Gen: 1})
		case <-r.done:
		}
	}()
	for {
		f, err := ReadSinkFrame(c)
		if err != nil {
			return
		}
		if f.T == FrameRecord && write(SinkFrame{T: FrameAck, Gen: 1, Seq: f.Seq}) != nil {
			return
		}
		select {
		case r.got <- f:
		default:
		}
	}
}

// startRunnerSinkSidecar builds a sidecar from Config with its decisions going to r, as cmd/wardyn-proxy
// does on a runner, and runs ListenAndServe. Every dial it makes reaches upstream; edit, if any, adjusts the
// Config first.
func startRunnerSinkSidecar(t *testing.T, r *tlsRunnerd, upstream string, edit ...func(*Config)) (*Server, string, chan error) {
	t.Helper()
	addr, sink := freeAddr(t), r.cfg
	cfg := &Config{RunID: uuid.New(), RunToken: "seed", Listen: addr, DecisionBufferSize: defaultBufferSize,
		DecisionSink: &sink, Policy: types.RunPolicySpec{AllowAllEgress: true}}
	for _, e := range edit {
		e(cfg)
	}
	srv, err := newServer(context.Background(), cfg, http.DefaultClient, io.Discard,
		serverHooks{dial: redirectDial(upstream), res: publicResolver{}})
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv, addr, served
}

func awaitHello(t *testing.T, r *tlsRunnerd) {
	t.Helper()
	select {
	case <-r.hello:
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy never said hello to its sink")
	}
}

// TestServer_RunnerSinkProxyListensOnlyAfterItsFirstGrant: a proxy whose decisions go to a runner sink binds
// nothing until that sink has granted it credit, so no request reaches a proxy with no session to record it
// in, and a run whose proxy never had one owes no loss. A Shutdown before the grant ends ListenAndServe.
func TestServer_RunnerSinkProxyListensOnlyAfterItsFirstGrant(t *testing.T) {
	r := newTLSRunnerd(t)
	_, addr, _ := startRunnerSinkSidecar(t, r, "127.0.0.1:1")
	awaitHello(t, r)
	if isListening(addr, 300*time.Millisecond) {
		t.Fatal("the proxy listened once its sink accepted it, before the sink granted it credit")
	}
	close(r.release)
	if !isListening(addr, 5*time.Second) {
		t.Fatal("the proxy never listened after its sink's first grant")
	}

	t.Run("a Shutdown before the first grant", func(t *testing.T) {
		r := newTLSRunnerd(t) // never released
		srv, addr, served := startRunnerSinkSidecar(t, r, "127.0.0.1:1")
		awaitHello(t, r)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		select {
		case err := <-served:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("ListenAndServe kept waiting for a grant after Shutdown")
		}
		if isListening(addr, 100*time.Millisecond) {
			t.Fatal("a proxy shut down before its first grant listened")
		}
	})
}

// TestServer_ShutdownEndsTunnelsBeforeTheRunnerSinkCloses: the runner sink counts every request being
// served and closes only once they have ended, so none writes a record after the session that would carry
// it; Shutdown ends the ones http.Shutdown leaves, an open CONNECT tunnel here, rather than wait them out.
func TestServer_ShutdownEndsTunnelsBeforeTheRunnerSinkCloses(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	go func() {
		for {
			c, err := held.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c); c.Close() }() // answers nothing
		}
	}()
	r := newTLSRunnerd(t)
	close(r.release)
	srv, addr, served := startRunnerSinkSidecar(t, r, held.Addr().String())
	if !isListening(addr, 5*time.Second) {
		t.Fatal("the proxy never listened")
	}
	client, answer := connectThrough(t, "http://"+addr, "held.example:443")
	defer client.Close()
	if !strings.Contains(answer, "200") {
		t.Fatalf("CONNECT answer %q", answer)
	}
	rs := srv.sink.remote
	inFlight := func() int { rs.mu.Lock(); defer rs.mu.Unlock(); return rs.inFlight }
	if !gateEventually(5*time.Second, func() bool { return inFlight() == 1 }) {
		t.Fatalf("requests counted as served with a tunnel open = %d, want 1", inFlight())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	shut := make(chan error, 1)
	go func() { shut <- srv.Shutdown(ctx) }()
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("the tunnel was not ended by Shutdown: %v", err)
	}
	select {
	case err := <-shut:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown waited out its deadline for a tunnel it should have ended")
	}
	if n := inFlight(); n != 0 {
		t.Fatalf("requests counted as served after Shutdown = %d, want 0", n)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

// TestServer_RunnerSinkWritesTheUpstreamHopRowOnceGranted: a runner-sink proxy started with an upstream
// proxy writes that hop's start-up row only once its sink has granted credit, so the row reaches the sink
// and a drain answers no loss (consumed equals the last sequence). Written as the proxy was built, before
// any grant, the row was lost and its drained reply counted a loss.
func TestServer_RunnerSinkWritesTheUpstreamHopRowOnceGranted(t *testing.T) {
	r := newTLSRunnerd(t)
	_, addr, _ := startRunnerSinkSidecar(t, r, "127.0.0.1:1", func(c *Config) { c.UpstreamProxyURL = "http://192.0.2.7:3128" })
	awaitHello(t, r)
	close(r.release)
	if !isListening(addr, 5*time.Second) {
		t.Fatal("the proxy never listened after its sink's first grant")
	}
	close(r.drain)
	var rows []string
	for {
		select {
		case f := <-r.got:
			switch f.T {
			case FrameRecord:
				var b RecordBody
				if err := json.Unmarshal(f.Body, &b); err != nil {
					t.Fatal(err)
				}
				rows = append(rows, b.Decision.RuleSource)
			case FrameDrained:
				if f.Consumed != uint64(f.FinalSeq) || !slices.Equal(rows, []string{egress.RuleSourceUpstreamProxy}) {
					t.Fatalf("drained %+v after records %v; want the upstream proxy's row sent and no loss", f, rows)
				}
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no drained reply; records %v", rows)
		}
	}
}

// TestNewProxy_WritesTheUpstreamHopRowAtStartWithoutARunnerSink: with no runner sink the start-up row is
// written as the proxy is built, as before.
func TestNewProxy_WritesTheUpstreamHopRowAtStartWithoutARunnerSink(t *testing.T) {
	up, err := parseUpstreamProxy("http://192.0.2.7:3128")
	if err != nil {
		t.Fatal(err)
	}
	sink := &decisionSink{out: io.Discard, ch: make(chan egress.DecisionLog, 4)}
	newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{}), Sink: sink, Upstream: up})
	select {
	case d := <-sink.ch:
		if d.RuleSource != egress.RuleSourceUpstreamProxy || d.Decision != egress.Allow {
			t.Fatalf("start-up row = %s %s; want the upstream proxy's allow", d.Decision, d.RuleSource)
		}
	default:
		t.Fatal("a proxy without a runner sink wrote no upstream proxy row as it was built")
	}
}
