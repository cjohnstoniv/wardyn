// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// offlineRemoteSink is a remote sink whose runnerd never answers (an outage),
// holding the given grant.
func offlineRemoteSink(through, bytes int64) *remoteSink {
	s := &remoteSink{res: map[uuid.UUID]spoolReservation{}, haveGrant: true, through: through, bytes: bytes}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// TestRemoteSink_BufferExhaustionRefusesAndKeepsRun: during a runnerd outage
// the proxy enforces policy until its grant is used up, then refuses new
// requests (counted for Drops) without ending the run; credit returns with a
// fresh grant after runnerd comes back.
func TestRemoteSink_BufferExhaustionRefusesAndKeepsRun(t *testing.T) {
	rs := offlineRemoteSink(3, 1<<20)
	p, _ := newTestProxy(t, types.RunPolicySpec{AllowAllEgress: true}, "", nil, nil)
	p.sink = &decisionSink{remote: rs}
	for i := range 3 {
		id, err := rs.reserve(100)
		if err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		rs.emit(egress.DecisionLog{Request: egress.Request{AuditReservation: id}, Decision: egress.Allow})
	}
	if _, err := rs.reserve(100); !errors.Is(err, runnerspool.ErrFull) {
		t.Fatalf("reserve past the grant = %v", err)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://example.com/"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("request past the grant: status %d", rec.Code)
	}
	rs.emit(egress.DecisionLog{Decision: egress.Deny}) // unreserved: no credit, refused
	rs.mu.Lock()
	refused, buffered, closed := rs.refused, len(rs.buf), rs.closed
	rs.mu.Unlock()
	if refused != 3 || buffered != 3 || closed {
		t.Fatalf("refused=%d buffered=%d closed=%v", refused, buffered, closed)
	}
	// runnerd back: acks and a fresh grant restore credit.
	rs.mu.Lock()
	rs.ackLocked(3)
	rs.base, rs.through, rs.sinceBase = 3, 6, 0
	rs.mu.Unlock()
	if _, err := rs.reserve(100); err != nil {
		t.Fatalf("reserve after a fresh grant: %v", err)
	}
}

// TestRemoteSink_ByteCreditBoundsReservations: held bytes count against the
// grant's byte credit.
func TestRemoteSink_ByteCreditBoundsReservations(t *testing.T) {
	rs := offlineRemoteSink(100, 1000)
	if _, err := rs.reserve(600); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.reserve(600); !errors.Is(err, runnerspool.ErrFull) {
		t.Fatalf("byte credit exceeded: %v", err)
	}
}

// fakeRunnerd answers one session per dial over a pipe.
type fakeRunnerd struct {
	t        *testing.T
	sessions chan net.Conn
}

func (f *fakeRunnerd) dial(context.Context) (net.Conn, error) {
	a, b := net.Pipe()
	f.sessions <- b
	return a, nil
}

func (f *fakeRunnerd) next() net.Conn {
	select {
	case c := <-f.sessions:
		return c
	case <-time.After(5 * time.Second):
		f.t.Fatal("proxy did not dial")
		return nil
	}
}

func readFrame(t *testing.T, c net.Conn) SinkFrame {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	f, err := ReadSinkFrame(c)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return f
}

func writeFrame(t *testing.T, c net.Conn, f SinkFrame) {
	t.Helper()
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := WriteSinkFrame(c, f); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// TestRemoteSink_ReconnectResendsAtNewGenAndDrains: a transport loss is a
// disconnect, not an end: the proxy reconnects, the accept's durable seq drops
// what runnerd already has, the unacked tail is resent at the new gen with
// identical bodies, and a drain answers final_seq and consumed (one held
// reservation is the exact loss).
func TestRemoteSink_ReconnectResendsAtNewGenAndDrains(t *testing.T) {
	f := &fakeRunnerd{t: t, sessions: make(chan net.Conn, 4)}
	key := hex.EncodeToString(make([]byte, 32))
	rs := &remoteSink{runID: uuid.New(), key: key, instance: "0123456789abcdef0123456789abcdef", dial: f.dial,
		retry: 10 * time.Millisecond, drainFor: 50 * time.Millisecond, res: map[uuid.UUID]spoolReservation{}}
	rs.cond = sync.NewCond(&rs.mu)
	go rs.run()

	c := f.next()
	if h := readFrame(t, c); h.T != FrameHello || h.Key != key || h.Instance != rs.instance || h.V != SinkVersion {
		t.Fatalf("hello = %+v", h)
	}
	writeFrame(t, c, SinkFrame{T: FrameAccept, Gen: 1})
	writeFrame(t, c, SinkFrame{T: FrameGrant, Gen: 1, Through: 10, Bytes: 1 << 20})
	waitGrant := func() {
		rs.mu.Lock()
		defer rs.mu.Unlock()
		for !rs.haveGrant {
			rs.cond.Wait()
		}
	}
	waitGrant()
	for i := 0; i < 3; i++ {
		id, err := rs.reserve(1000)
		if err != nil {
			t.Fatal(err)
		}
		rs.emit(egress.DecisionLog{Request: egress.Request{AuditReservation: id, Host: "h"}, Decision: egress.Allow})
	}
	bodies := map[int64]string{}
	for i := int64(1); i <= 3; i++ {
		r := readFrame(t, c)
		if r.T != FrameRecord || r.Seq != i || r.Gen != 1 {
			t.Fatalf("record = %+v", r)
		}
		bodies[i] = string(r.Body)
	}
	writeFrame(t, c, SinkFrame{T: FrameAck, Gen: 1, Seq: 1, Disposition: "appended"})
	c.Close() // transport loss

	c = f.next()
	readFrame(t, c)                                             // hello again, same instance
	writeFrame(t, c, SinkFrame{T: FrameAccept, Gen: 2, Seq: 2}) // runnerd committed 2 before the loss
	writeFrame(t, c, SinkFrame{T: FrameGrant, Gen: 2, Base: 2, Through: 12, Bytes: 1 << 20})
	r := readFrame(t, c)
	waitGrant()
	if r.T != FrameRecord || r.Seq != 3 || r.Gen != 2 || string(r.Body) != bodies[3] {
		t.Fatalf("resend = %+v", r)
	}
	h := sha256.Sum256(r.Body)
	if hex.EncodeToString(h[:]) != func() string { s := sha256.Sum256([]byte(bodies[3])); return hex.EncodeToString(s[:]) }() {
		t.Fatal("resend body hash differs")
	}
	writeFrame(t, c, SinkFrame{T: FrameAck, Gen: 2, Seq: 3})
	if _, err := rs.reserve(1000); err != nil { // held through the drain: the exact loss
		t.Fatal(err)
	}
	writeFrame(t, c, SinkFrame{T: FrameDrain, Gen: 2})
	d := readFrame(t, c)
	if d.T != FrameDrained || d.Gen != 2 || d.FinalSeq != 3 || d.Consumed != 4 {
		t.Fatalf("drained = %+v", d)
	}
	if _, err := rs.reserve(1); err == nil {
		t.Fatal("reserved after drain")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := rs.close(ctx); err != nil {
		t.Fatal(err)
	}
}

func selfSigned(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}, hex.EncodeToString(sum[:])
}

// TestRemoteSink_WrongPinNeverSendsHello: a sink whose key is not the pinned
// one gets no hello (and so never the run's key).
func TestRemoteSink_WrongPinNeverSendsHello(t *testing.T) {
	cert, pin := selfSigned(t)
	_, otherPin := selfSigned(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{SinkALPN}, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				if f, err := ReadSinkFrame(c); err == nil {
					got <- f.T
				} else {
					got <- "none"
				}
			}()
		}
	}()
	for _, tc := range []struct {
		pin  string
		want string
	}{{otherPin, "none"}, {pin, FrameHello}} {
		cfg := DecisionSinkConfig{URL: "tls://" + ln.Addr().String(), SPKISHA256: tc.pin, Key: hex.EncodeToString(make([]byte, 32))}
		rs, err := newRemoteSink(uuid.New(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case g := <-got:
			if g != tc.want {
				t.Fatalf("pin %s: server saw %q, want %q", tc.pin[:8], g, tc.want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no connection")
		}
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		_ = rs.close(ctx)
		cancel()
	}
}

func TestDecisionSinkConfigValidation(t *testing.T) {
	good := DecisionSinkConfig{URL: "tls://10.0.0.5:7443", SPKISHA256: hex.EncodeToString(make([]byte, 32)), Key: hex.EncodeToString(make([]byte, 32))}
	if err := good.validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []DecisionSinkConfig{
		{URL: "https://10.0.0.5:7443", SPKISHA256: good.SPKISHA256, Key: good.Key},
		{URL: "tls://10.0.0.5", SPKISHA256: good.SPKISHA256, Key: good.Key},
		{URL: good.URL, SPKISHA256: "AB", Key: good.Key},
		{URL: good.URL, SPKISHA256: good.SPKISHA256, Key: good.Key[:62]},
	} {
		if bad.validate() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	base := func() *Config {
		return &Config{RunID: uuid.New(), ControlPlaneURL: "http://127.0.0.1:8080", RunToken: "t"}
	}
	c := base()
	c.SelfLapse = true
	if c.applyDefaultsAndValidate() == nil {
		t.Error("self_lapse without decision_sink accepted")
	}
	c = base()
	c.DecisionSink = &good
	if c.applyDefaultsAndValidate() == nil {
		t.Error("decision_sink without self_lapse accepted")
	}
	c.SelfLapse, c.DecisionSpoolDir = true, "/spool"
	if c.applyDefaultsAndValidate() == nil {
		t.Error("decision_sink with decision_spool_dir accepted")
	}
	c.DecisionSpoolDir = ""
	if err := c.applyDefaultsAndValidate(); err != nil {
		t.Errorf("k8s config refused: %v", err)
	}
}
