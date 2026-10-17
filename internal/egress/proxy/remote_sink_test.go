// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

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

// readUntil reads frames, acking records, until one of type want.
func readUntil(t *testing.T, c net.Conn, want string) SinkFrame {
	t.Helper()
	for {
		f := readFrame(t, c)
		if f.T == want {
			return f
		}
		if f.T == FrameRecord {
			writeFrame(t, c, SinkFrame{T: FrameAck, Gen: f.Gen, Seq: f.Seq, Disposition: "appended"})
		}
	}
}

func writeFrame(t *testing.T, c net.Conn, f SinkFrame) {
	t.Helper()
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := WriteSinkFrame(c, f); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

func testKey() string { return hex.EncodeToString(make([]byte, 32)) }

// startedSink is a remote sink dialing a fake runnerd; no session yet.
func startedSink(t *testing.T) (*remoteSink, *fakeRunnerd) {
	t.Helper()
	f := &fakeRunnerd{t: t, sessions: make(chan net.Conn, 4)}
	rs := &remoteSink{runID: uuid.New(), key: testKey(), instance: "0123456789abcdef0123456789abcdef", dial: f.dial,
		retry: 20 * time.Millisecond, drainFor: 200 * time.Millisecond, res: map[uuid.UUID]spoolReservation{}}
	rs.cond = sync.NewCond(&rs.mu)
	go rs.run()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_ = rs.close(ctx)
	})
	return rs, f
}

// accept answers a session's hello with accept and a grant, and waits until
// the proxy holds that grant.
func accept(t *testing.T, rs *remoteSink, c net.Conn, gen uint64, seq, through, bytes int64) net.Conn {
	t.Helper()
	if h := readFrame(t, c); h.T != FrameHello {
		t.Fatalf("hello = %+v", h)
	}
	writeFrame(t, c, SinkFrame{T: FrameAccept, Gen: gen, Seq: seq})
	writeFrame(t, c, SinkFrame{T: FrameGrant, Gen: gen, Base: seq, Through: through, Bytes: bytes})
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for rs.gen != gen || rs.through != through || rs.bytes != bytes {
		rs.cond.Wait()
	}
	return c
}

func short(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}

func refusedTotal(rs *remoteSink) uint64 { rs.mu.Lock(); defer rs.mu.Unlock(); return rs.refused }

// TestRemoteSink_GrantSurvivesTransportLoss (H1, M4, design Q5 and §6.2):
// after a transport loss the proxy keeps enforcing on its outstanding grant;
// once that is used up it refuses new requests, counted (a held, exhausted
// grant) and forwarded on the reconnect, and keeps the run; the reconnect's
// grant restores credit.
func TestRemoteSink_GrantSurvivesTransportLoss(t *testing.T) {
	rs, f := startedSink(t)
	c := accept(t, rs, f.next(), 1, 0, 3, 1<<20)
	if _, err := rs.reserve(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	c.Close() // transport loss with two record credits outstanding
	// The redial is pending (its hello unanswered): the session end is fully
	// processed and runnerd is unreachable.
	next := f.next()
	for i := range 2 {
		if _, err := rs.reserve(short(t), 100); err != nil {
			t.Fatalf("reserve %d during the outage: %v", i, err)
		}
	}
	if _, err := rs.reserve(short(t), 100); !errors.Is(err, runnerspool.ErrFull) {
		t.Fatalf("reserve past the grant during the outage = %v", err)
	}
	rs.mu.Lock()
	closed := rs.closed
	rs.mu.Unlock()
	if refusedTotal(rs) != 1 || closed {
		t.Fatalf("refused=%d closed=%v: an outage refusal past the held grant counts once, and the run is kept", refusedTotal(rs), closed)
	}
	next = accept(t, rs, next, 2, 0, 10, 1<<20)
	if r := readUntil(t, next, FrameRefused); r.Total != 1 || r.Gen != 2 {
		t.Fatalf("refused total after the reconnect = %+v", r)
	}
	if _, err := rs.reserve(t.Context(), 100); err != nil {
		t.Fatalf("reserve after the reconnect grant: %v", err)
	}
}

// TestRemoteSink_RefusalsCountedOnlyWhenFull (M1): only a held, exhausted grant
// on a live session counts for Drops; no grant, a drain, and a late admitted
// emit after the drained reply (already in consumed) never count.
func TestRemoteSink_RefusalsCountedOnlyWhenFull(t *testing.T) {
	rs, f := startedSink(t)
	if _, err := rs.reserve(short(t), 100); !errors.Is(err, runnerspool.ErrFull) || refusedTotal(rs) != 0 {
		t.Fatalf("no grant yet: err=%v refused=%d", err, refusedTotal(rs))
	}
	c := accept(t, rs, f.next(), 1, 0, 2, 1<<20)
	id, err := rs.reserve(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rs.reserve(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.reserve(short(t), 100); !errors.Is(err, runnerspool.ErrFull) || refusedTotal(rs) != 1 {
		t.Fatalf("exhausted live grant: err=%v refused=%d, want counted once", err, refusedTotal(rs))
	}
	if r := readUntil(t, c, FrameRefused); r.Total != 1 {
		t.Fatalf("refused frame %+v", r)
	}
	writeFrame(t, c, SinkFrame{T: FrameDrain, Gen: 1})
	d := readUntil(t, c, FrameDrained)
	if d.Consumed-uint64(d.FinalSeq) != 2 {
		t.Fatalf("drained %+v: two held reservations are the exact loss", d)
	}
	if _, err := rs.reserve(short(t), 100); err == nil {
		t.Fatal("reserved while drained")
	}
	rs.emit(egress.DecisionLog{Request: egress.Request{AuditReservation: id, Host: "h"}, Decision: egress.Allow})
	rs.emit(egress.DecisionLog{Decision: egress.Deny})
	if refusedTotal(rs) != 1 {
		t.Fatalf("refused=%d: a drain refusal or a post-drain emit was counted", refusedTotal(rs))
	}
}

// TestRemoteSink_CancelledAdmissionUncounted (M5): a client that cancels its
// admission against an exhausted grant is not a Drop; a deadline that expires
// on the exhausted grant is.
func TestRemoteSink_CancelledAdmissionUncounted(t *testing.T) {
	rs, f := startedSink(t)
	accept(t, rs, f.next(), 1, 0, 1, 1<<20)
	if _, err := rs.reserve(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := rs.reserve(gone, 100); !errors.Is(err, context.Canceled) || refusedTotal(rs) != 0 {
		t.Fatalf("cancelled admission: err=%v refused=%d, want Canceled uncounted", err, refusedTotal(rs))
	}
	if _, err := rs.reserve(short(t), 100); !errors.Is(err, runnerspool.ErrFull) || refusedTotal(rs) != 1 {
		t.Fatalf("expired admission on an exhausted grant: err=%v refused=%d, want ErrFull counted", err, refusedTotal(rs))
	}
}

// TestRemoteSink_SixteenScannedRequestsInFlight (H2): the byte credit admits
// GrantReservations requests reserved at the largest record; the next waits
// for credit and is admitted by a regrant, uncounted.
func TestRemoteSink_SixteenScannedRequestsInFlight(t *testing.T) {
	rs, f := startedSink(t)
	c := accept(t, rs, f.next(), 1, 0, runnerspool.GrantRecords, runnerspool.GrantBytes)
	for i := range runnerspool.GrantReservations {
		if _, err := rs.reserve(short(t), runnerspool.MaxRecordBytes); err != nil {
			t.Fatalf("scanned request %d refused: %v", i+1, err)
		}
	}
	got := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := rs.reserve(ctx, runnerspool.MaxRecordBytes)
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("17th request did not wait: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	writeFrame(t, c, SinkFrame{T: FrameGrant, Gen: 1, Base: 0, Through: runnerspool.GrantRecords, Bytes: 2 * runnerspool.GrantBytes})
	if err := <-got; err != nil || refusedTotal(rs) != 0 {
		t.Fatalf("17th after the regrant: err=%v refused=%d", err, refusedTotal(rs))
	}
}

// TestRemoteSink_LapseDrainsBeforeLapsed (M2): a lapse answers drained before
// lapsed, so runnerd settles the exact loss (here 0), not the upper bound.
func TestRemoteSink_LapseDrainsBeforeLapsed(t *testing.T) {
	rs, f := startedSink(t)
	c := accept(t, rs, f.next(), 1, 0, 10, 1<<20)
	id, err := rs.reserve(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	rs.emit(egress.DecisionLog{Request: egress.Request{AuditReservation: id, Host: "h"}, Decision: egress.Allow})
	go rs.lapse(LapseDeadline)
	r := readFrame(t, c)
	if r.T != FrameRecord {
		t.Fatalf("first frame %+v", r)
	}
	writeFrame(t, c, SinkFrame{T: FrameAck, Gen: 1, Seq: r.Seq})
	d := readFrame(t, c)
	if d.T != FrameDrained || d.FinalSeq != 1 || d.Consumed != 1 {
		t.Fatalf("after the record: %+v, want drained final 1 consumed 1", d)
	}
	if l := readFrame(t, c); l.T != FrameLapsed || l.Reason != LapseDeadline {
		t.Fatalf("after drained: %+v", l)
	}
}

// TestRemoteSink_DrainClosesOpenTunnels (M3, §3.2 item 5): a drain closes an
// open CONNECT tunnel, so a normal stop answers an exact loss of 0.
func TestRemoteSink_DrainClosesOpenTunnels(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); c.Close() }()
		}
	}()
	rs, f := startedSink(t)
	c := accept(t, rs, f.next(), 1, 0, 10, 1<<20)
	p, _ := newTestProxy(t, types.RunPolicySpec{AllowAllEgress: true}, echo.Addr().String(), nil, nil)
	p.sink = &decisionSink{remote: rs}
	srv := httptest.NewServer(p)
	defer srv.Close()
	client, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, _ = client.Write([]byte("CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n"))
	br := bufio.NewReader(client)
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if line, err := br.ReadString('\n'); err != nil || !strings.Contains(line, "200") {
		t.Fatalf("CONNECT answer %q, %v", line, err)
	}
	_, _ = br.ReadString('\n')
	writeFrame(t, c, SinkFrame{T: FrameDrain, Gen: 1})
	d := readUntil(t, c, FrameDrained)
	if d.Consumed != uint64(d.FinalSeq) {
		t.Fatalf("drained %+v: a normal stop with an open tunnel lost decisions", d)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("tunnel still open after the drain: %v", err)
	}
}

// TestRemoteSink_DrainEndsInFlightMITMRequest: a drain with a
// MITM inner request still waiting on its upstream closes the agent's connection, the inner request
// ends at once (the upstream sees its context cancelled), and its final decision is emitted before the
// drained reply, so the drain is exact and the record is in it.
func TestRemoteSink_DrainEndsInFlightMITMRequest(t *testing.T) {
	const host = "mirror.corp"
	started := make(chan struct{}, 1)
	ended := make(chan time.Time, 1)
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
		ended <- time.Now()
	}))
	defer up.Close()
	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	rs, f := startedSink(t)
	c := accept(t, rs, f.next(), 1, 0, 10, 1<<20)
	p := newProxy(Options{
		RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{host}}),
		Sink: &decisionSink{remote: rs}, Scanner: forwardScanEngine(t, "block"), CA: ca, MITMHosts: []string{host},
		Resolver: publicResolver{}, TLSClientConfig: testInsecureTLSConfig, Dial: redirectDial(upstreamAddr(up)),
	})
	srv := httptest.NewServer(p)
	defer srv.Close()
	conn, status := connectThrough(t, srv.URL, host+":443")
	defer conn.Close()
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status = %q", status)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	tc := tls.Client(conn, &tls.Config{ServerName: host, RootCAs: pool})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("handshake through the MITM tunnel: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/slow", nil)
	if err := req.Write(tc); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the inner request never reached the upstream")
	}
	drainAt := time.Now()
	writeFrame(t, c, SinkFrame{T: FrameDrain, Gen: 1})
	var recorded bool
	var d SinkFrame
	for d.T != FrameDrained {
		d = readFrame(t, c)
		if d.T == FrameRecord {
			recorded = recorded || strings.Contains(string(d.Body), "/slow")
			writeFrame(t, c, SinkFrame{T: FrameAck, Gen: d.Gen, Seq: d.Seq, Disposition: "appended"})
		}
	}
	drainedAt := time.Now()
	select {
	case at := <-ended:
		if took := at.Sub(drainAt); took > 3*time.Second {
			t.Fatalf("inner request ended %s after the drain began, past the 3 s drain", took)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the inner request was still waiting on its upstream 5 s after the drained reply (drain took %s; drained %+v, recorded %v)", drainedAt.Sub(drainAt), d, recorded)
	}
	if !recorded || d.Consumed != uint64(d.FinalSeq) {
		t.Fatalf("drained %+v, /slow decision recorded %v: want an exact drain carrying the inner request's decision", d, recorded)
	}
}

// TestRemoteSink_ReconnectResendsAtNewGenAndDrains: a transport loss is a
// disconnect, not an end: the proxy reconnects, the accept's durable seq drops
// what runnerd already has, the unacked tail is resent at the new gen with
// identical bodies, and a drain answers final_seq and consumed (one held
// reservation is the exact loss).
func TestRemoteSink_ReconnectResendsAtNewGenAndDrains(t *testing.T) {
	rs, f := startedSink(t)
	c := accept(t, rs, f.next(), 1, 0, 10, 1<<20)
	for range 3 {
		id, err := rs.reserve(t.Context(), 1000)
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

	c = accept(t, rs, f.next(), 2, 2, 12, 1<<20) // runnerd committed 2 before the loss
	r := readFrame(t, c)
	if r.T != FrameRecord || r.Seq != 3 || r.Gen != 2 || string(r.Body) != bodies[3] {
		t.Fatalf("resend = %+v", r)
	}
	writeFrame(t, c, SinkFrame{T: FrameAck, Gen: 2, Seq: 3})
	if _, err := rs.reserve(t.Context(), 1000); err != nil { // held through the drain: the exact loss
		t.Fatal(err)
	}
	writeFrame(t, c, SinkFrame{T: FrameDrain, Gen: 2})
	d := readFrame(t, c)
	if d.T != FrameDrained || d.Gen != 2 || d.FinalSeq != 3 || d.Consumed != 4 {
		t.Fatalf("drained = %+v", d)
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
	if err := c.applyDefaultsAndValidate(); err != nil {
		t.Errorf("Docker runner config (decision_sink alone) refused: %v", err)
	}
	c = base()
	c.DecisionSink = &good
	c.SelfLapse, c.DecisionSpoolDir = true, "/spool"
	if c.applyDefaultsAndValidate() == nil {
		t.Error("decision_sink with decision_spool_dir accepted")
	}
	c.DecisionSpoolDir = ""
	if err := c.applyDefaultsAndValidate(); err != nil {
		t.Errorf("k8s config refused: %v", err)
	}
}
