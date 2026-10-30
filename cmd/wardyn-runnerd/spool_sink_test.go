// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
)

type sinkHarness struct {
	t      *testing.T
	sink   *spoolSink
	addr   string
	dir    string
	id     uuid.UUID
	key    string
	pin    string
	mu     sync.Mutex
	ended  int
	lapsed []string
}

func newSinkHarness(t *testing.T) *sinkHarness {
	t.Helper()
	h := &sinkHarness{t: t, dir: t.TempDir(), id: uuid.New()}
	cert, pin, err := loadOrCreateSinkCert(filepath.Join(h.dir, "sink.key"))
	if err != nil {
		t.Fatal(err)
	}
	h.pin = pin
	h.sink = newSpoolSink(spoolSinkOptions{
		Cert:         cert,
		SpoolDir:     func(id uuid.UUID) string { return filepath.Join(h.dir, "spools", id.String()) },
		ActivityDir:  func(id uuid.UUID) string { return filepath.Join(h.dir, "spools", id.String()) },
		OnLapsed:     func(_ uuid.UUID, r string) { h.mu.Lock(); h.lapsed = append(h.lapsed, r); h.mu.Unlock() },
		OnSessionEnd: func(uuid.UUID) { h.mu.Lock(); h.ended++; h.mu.Unlock() },
	})
	if h.sink.SPKIPin() != pin {
		t.Fatal("pin mismatch")
	}
	key, hash, err := newSinkKey()
	if err != nil {
		t.Fatal(err)
	}
	h.key = key
	if err := h.sink.Register(h.id, hash); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.addr = ln.Addr().String()
	served := make(chan struct{})
	go func() {
		h.sink.Serve(ln) //nolint:errcheck // ends at Close
		close(served)
	}()
	// A session goroutine logs as it ends, so the sink is not stopped until every one has returned.
	t.Cleanup(func() {
		h.sink.Close()
		<-served
		h.sink.handlers.Wait()
	})
	return h
}

func (h *sinkHarness) dial(key, instance string) *tls.Conn {
	h.t.Helper()
	c, err := tls.Dial("tcp", h.addr, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{proxy.SinkALPN}, MinVersion: tls.VersionTLS13}) //nolint:gosec // test
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { c.Close() })
	if err := proxy.WriteSinkFrame(c, proxy.SinkFrame{T: proxy.FrameHello, V: proxy.SinkVersion, RunID: h.id.String(), Instance: instance, Key: key}); err != nil {
		h.t.Fatal(err)
	}
	return c
}

func read(t *testing.T, c net.Conn) (proxy.SinkFrame, error) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	return proxy.ReadSinkFrame(c)
}

func mustRead(t *testing.T, c net.Conn, want string) proxy.SinkFrame {
	t.Helper()
	f, err := read(t, c)
	if err != nil || f.T != want {
		t.Fatalf("read %s: %+v, %v", want, f, err)
	}
	return f
}

func (h *sinkHarness) record(c net.Conn, gen uint64, seq int64, admitted bool) proxy.SinkFrame {
	h.t.Helper()
	body, _ := json.Marshal(proxy.RecordBody{Admitted: admitted, Decision: egress.DecisionLog{
		Request: egress.Request{RunID: h.id, Host: "example.test", Time: time.Unix(int64(seq), 0).UTC()}, Decision: egress.Allow, RuleSource: "policy:allowed"}})
	if err := proxy.WriteSinkFrame(c, proxy.SinkFrame{T: proxy.FrameRecord, Gen: gen, Seq: seq, Body: body}); err != nil {
		h.t.Fatal(err)
	}
	return mustRead(h.t, c, proxy.FrameAck)
}

func (h *sinkHarness) spool() *runnerspool.Spool {
	h.t.Helper()
	s, err := runnerspool.Open(filepath.Join(h.dir, "spools", h.id.String()), h.id)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { s.Close() })
	return s
}

const inst = "0123456789abcdef0123456789abcdef"

// waitEnded waits for the sink to see n session ends.
func (h *sinkHarness) waitEnded(n int) {
	h.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		h.mu.Lock()
		e := h.ended
		h.mu.Unlock()
		if e >= n {
			return
		}
	}
	h.t.Fatalf("sink saw fewer than %d session ends", n)
}

// TestSpoolSinkHandshake: a wrong key gets no reply (the compare is constant
// time over sha256(key)); an unknown run likewise; the right key is accepted
// with a persisted gen and a write-ahead grant; a second session supersedes
// the first at gen+1 and closes it.
func TestSpoolSinkHandshake(t *testing.T) {
	h := newSinkHarness(t)
	bad := h.dial(hex.EncodeToString(make([]byte, 32)), inst)
	if f, err := read(t, bad); err == nil {
		t.Fatalf("wrong key answered: %+v", f)
	}
	c1 := h.dial(h.key, inst)
	acc := mustRead(t, c1, proxy.FrameAccept)
	g := mustRead(t, c1, proxy.FrameGrant)
	if acc.Gen != 1 || g.Through != runnerspool.GrantRecords {
		t.Fatalf("accept %+v grant %+v", acc, g)
	}
	if l, _ := h.sink.Liability(t.Context(), h.id); l.Gen != 1 || l.Grant == nil {
		t.Fatalf("grant not persisted before it was sent: %+v", l)
	}
	c2 := h.dial(h.key, inst)
	if acc2 := mustRead(t, c2, proxy.FrameAccept); acc2.Gen != 2 {
		t.Fatalf("supersession gen = %d", acc2.Gen)
	}
	mustRead(t, c2, proxy.FrameGrant)
	if _, err := read(t, c1); err == nil {
		t.Fatal("superseded session still open")
	}
	other := h.dial(h.key, "ffffffffffffffffffffffffffffffff")
	if _, err := read(t, other); err == nil {
		t.Fatal("a second instance was accepted")
	}
}

// TestSpoolSinkDisconnectIsNotAnExit (P1, R3-M1): an EOF records no lapse, the
// reconnect at gen+1 keeps the liability and dedupes the resend; a lapsed frame
// on a stale gen is ignored, on the current gen it is evidence.
func TestSpoolSinkDisconnectIsNotAnExit(t *testing.T) {
	h := newSinkHarness(t)
	c := h.dial(h.key, inst)
	mustRead(t, c, proxy.FrameAccept)
	mustRead(t, c, proxy.FrameGrant)
	if a := h.record(c, 1, 1, true); a.Seq != 1 || a.Disposition != string(runnerspool.Appended) {
		t.Fatalf("ack %+v", a)
	}
	c.Close()
	h.waitEnded(1)
	c = h.dial(h.key, inst)
	acc := mustRead(t, c, proxy.FrameAccept)
	mustRead(t, c, proxy.FrameGrant)
	if acc.Gen != 2 || acc.Seq != 1 {
		t.Fatalf("reconnect accept %+v", acc)
	}
	if a := h.record(c, 2, 1, true); a.Disposition != string(runnerspool.Appended) {
		t.Fatalf("resend ack %+v", a)
	}
	if depth, _, _, _ := h.spool().Stats(t.Context()); depth != 1 {
		t.Fatalf("depth %d after a resend: duplicate", depth)
	}
	h.mu.Lock()
	ended, lapsed := h.ended, len(h.lapsed)
	h.mu.Unlock()
	if ended != 1 || lapsed != 0 {
		t.Fatalf("ended=%d lapsed=%d after an EOF", ended, lapsed)
	}
	if err := proxy.WriteSinkFrame(c, proxy.SinkFrame{T: proxy.FrameLapsed, Gen: 1, Reason: "deadline"}); err != nil {
		t.Fatal(err)
	}
	if _, err := read(t, c); err == nil { // stale gen closes the session
		t.Fatal("stale-gen frame kept the session")
	}
	c = h.dial(h.key, inst)
	acc = mustRead(t, c, proxy.FrameAccept)
	mustRead(t, c, proxy.FrameGrant)
	if err := proxy.WriteSinkFrame(c, proxy.SinkFrame{T: proxy.FrameLapsed, Gen: acc.Gen, Reason: "deadline"}); err != nil {
		t.Fatal(err)
	}
	h.record(c, acc.Gen, 2, false) // the ack orders the lapsed frame before it
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.lapsed) != 1 || h.lapsed[0] != "deadline" {
		t.Fatalf("lapsed evidence %v", h.lapsed)
	}
}

// TestSpoolSinkDrainExactLossAckedBeforeSeal (R6): drained with one held
// reservation gives an exact loss of 1, emitted as a durable row; Seal waits
// for the writer lock, then for the org's ack of the loss row.
func TestSpoolSinkDrainExactLossAckedBeforeSeal(t *testing.T) {
	ctx := t.Context()
	h := newSinkHarness(t)
	c := h.dial(h.key, inst)
	acc := mustRead(t, c, proxy.FrameAccept)
	mustRead(t, c, proxy.FrameGrant)
	h.record(c, acc.Gen, 1, true)
	go func() {
		f, err := read(t, c)
		if err == nil && f.T == proxy.FrameDrain {
			_ = proxy.WriteSinkFrame(c, proxy.SinkFrame{T: proxy.FrameDrained, Gen: acc.Gen, FinalSeq: 1, Consumed: 2})
		}
	}()
	d, err := h.sink.Drain(ctx, h.id)
	if err != nil || d.FinalSeq != 1 || d.Consumed != 2 {
		t.Fatalf("drain = %+v, %v", d, err)
	}
	s := h.spool()
	ackAndSeal := func() bool {
		t.Helper()
		if err := s.Replay(ctx, func(context.Context, []runnerspool.Record) error { return nil }); err != nil {
			t.Fatal(err)
		}
		r, _ := s.Receipt(ctx)
		ok, err := s.Seal(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if ackAndSeal() {
		t.Fatal("sealed while the sink holds the writer lock")
	}
	loss, err := h.sink.ProducerGone(ctx, h.id)
	if err != nil || loss.Count != 1 || loss.Basis != runnerspool.LossExact {
		t.Fatalf("loss = %+v, %v", loss, err)
	}
	r, _ := s.Receipt(ctx)
	if ok, _ := s.Seal(ctx, r); ok {
		t.Fatal("sealed before the loss row was acked")
	}
	var lost bool
	if err := s.Replay(ctx, func(_ context.Context, rows []runnerspool.Record) error {
		for _, row := range rows {
			lost = lost || row.Decision.RuleSource == "builtin:spool-lost"
		}
		return nil
	}); err != nil || !lost {
		t.Fatalf("loss row delivered=%v, %v", lost, err)
	}
	if !ackAndSeal() {
		t.Fatal("settled, acked spool did not seal")
	}
}

// TestSpoolSinkRestartKeepsGrantAndWriter (P3): after a runnerd restart the
// re-registered run holds the writer lock again and its outstanding grant is
// known; ProducerGone without a drained reply writes off the upper bound.
func TestSpoolSinkRestartKeepsGrantAndWriter(t *testing.T) {
	ctx := t.Context()
	h := newSinkHarness(t)
	c := h.dial(h.key, inst)
	acc := mustRead(t, c, proxy.FrameAccept)
	g := mustRead(t, c, proxy.FrameGrant)
	h.record(c, acc.Gen, 1, true)
	h.sink.Close()

	cert, _, err := loadOrCreateSinkCert(filepath.Join(h.dir, "sink.key"))
	if err != nil {
		t.Fatal(err)
	}
	sink := newSpoolSink(spoolSinkOptions{Cert: cert,
		SpoolDir:    func(id uuid.UUID) string { return filepath.Join(h.dir, "spools", id.String()) },
		ActivityDir: func(id uuid.UUID) string { return filepath.Join(h.dir, "spools", id.String()) }})
	t.Cleanup(func() { sink.Close() })
	if sink.SPKIPin() != h.pin {
		t.Fatal("pin changed across a restart")
	}
	hash := sha256.Sum256(must(hex.DecodeString(h.key)))
	if err := sink.Register(h.id, hash); err != nil {
		t.Fatal(err)
	}
	l, err := sink.Liability(ctx, h.id)
	if err != nil || l.Grant == nil || l.Grant.Through != g.Through || l.Producer.Seq != 1 {
		t.Fatalf("liability after restart = %+v, %v", l, err)
	}
	s := h.spool()
	if _, err := s.HoldWriter(); !errors.Is(err, runnerspool.ErrWriterHeld) {
		t.Fatalf("writer lock not re-held: %v", err)
	}
	loss, err := sink.ProducerGone(ctx, h.id)
	if err != nil || loss.Basis != runnerspool.LossUpperBound || loss.Count != uint64(g.Through-1) {
		t.Fatalf("loss = %+v, %v", loss, err)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// TestSpoolSinkRegrantsAtHalfByteCredit (H2): large records use half the byte
// credit long before half the record credit; runnerd regrants then, not only
// on the record count or Tick.
func TestSpoolSinkRegrantsAtHalfByteCredit(t *testing.T) {
	h := newSinkHarness(t)
	c := h.dial(h.key, inst)
	acc := mustRead(t, c, proxy.FrameAccept)
	g := mustRead(t, c, proxy.FrameGrant)
	if g.Bytes != runnerspool.GrantBytes {
		t.Fatalf("grant bytes %d, want %d", g.Bytes, runnerspool.GrantBytes)
	}
	path := strings.Repeat("a", 950_000)
	var sent int64
	for seq := int64(1); ; seq++ {
		body, _ := json.Marshal(proxy.RecordBody{Admitted: true, Decision: egress.DecisionLog{
			Request: egress.Request{RunID: h.id, Host: "example.test", Path: path}, Decision: egress.Allow, RuleSource: "policy:allowed"}})
		if err := proxy.WriteSinkFrame(c, proxy.SinkFrame{T: proxy.FrameRecord, Gen: acc.Gen, Seq: seq, Body: body}); err != nil {
			t.Fatal(err)
		}
		sent += int64(len(body))
		mustRead(t, c, proxy.FrameAck)
		if 2*sent >= g.Bytes {
			ng := mustRead(t, c, proxy.FrameGrant)
			if ng.Base != seq {
				t.Fatalf("regrant base %d, want %d", ng.Base, seq)
			}
			return
		}
	}
}

// TestSpoolSinkSucceedHandsTheRunToANewProxy (Docker revive): Succeed refuses
// while the old proxy's producer is live; after ProducerGone the run is handed
// to a new key, the old key no longer authenticates, and the new proxy's
// instance starts at sequence 1 beside the old proxy's records.
func TestSpoolSinkSucceedHandsTheRunToANewProxy(t *testing.T) {
	ctx := t.Context()
	h := newSinkHarness(t)
	c := h.dial(h.key, inst)
	acc := mustRead(t, c, proxy.FrameAccept)
	mustRead(t, c, proxy.FrameGrant)
	h.record(c, acc.Gen, 1, true)
	key, hash, err := newSinkKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.sink.Succeed(h.id, hash); err == nil {
		t.Fatal("succeed handed over a run whose producer is live")
	}
	// The driver drains the old proxy, removes it, then settles it: exact, so nothing is lost.
	go func() {
		f, err := read(t, c)
		if err == nil && f.T == proxy.FrameDrain {
			_ = proxy.WriteSinkFrame(c, proxy.SinkFrame{T: proxy.FrameDrained, Gen: acc.Gen, FinalSeq: 1, Consumed: 1})
		}
	}()
	if _, err := h.sink.Drain(ctx, h.id); err != nil {
		t.Fatal(err)
	}
	if loss, err := h.sink.ProducerGone(ctx, h.id); err != nil || loss.Count != 0 {
		t.Fatalf("settle = %+v, %v; want exact and nothing lost", loss, err)
	}
	if err := h.sink.Succeed(h.id, hash); err != nil {
		t.Fatal(err)
	}
	old := h.dial(h.key, inst)
	if f, err := read(t, old); err == nil {
		t.Fatalf("the old key still answered: %+v", f)
	}
	const next = "ffffffffffffffffffffffffffffffff"
	c = h.dial(key, next)
	acc = mustRead(t, c, proxy.FrameAccept)
	mustRead(t, c, proxy.FrameGrant)
	if acc.Seq != 0 {
		t.Fatalf("new proxy accept %+v, want seq 0", acc)
	}
	if a := h.record(c, acc.Gen, 1, true); a.Disposition != string(runnerspool.Appended) {
		t.Fatalf("new proxy ack %+v", a)
	}
	s := h.spool()
	if depth, _, _, _ := s.Stats(ctx); depth != 2 {
		t.Fatalf("depth %d, want both proxies' records and no loss row", depth)
	}
	if err := s.Replay(ctx, func(context.Context, []runnerspool.Record) error { return nil }); err != nil {
		t.Fatal(err)
	}
	r, _ := s.Receipt(ctx)
	if ok, err := s.Seal(ctx, r); ok || err != nil {
		t.Fatalf("seal = %v, %v while the new proxy holds the writer lock", ok, err)
	}
}

// slowLog holds each record back, so a goroutine that logs after its test has returned is caught.
type slowLog struct{ slog.Handler }

func (h slowLog) Handle(ctx context.Context, r slog.Record) error {
	time.Sleep(100 * time.Millisecond)
	return h.Handler.Handle(ctx, r)
}

// A harness leaves no session goroutine behind: the one a live proxy session leaves logs as it ends, into
// the process's default logger, which a later test may be reading.
func TestSinkHarnessCleanupWaitsForSessions(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slowLog{slog.NewTextHandler(&logs, nil)}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	t.Run("a live session", func(t *testing.T) {
		h := newSinkHarness(t)
		c := h.dial(h.key, inst)
		mustRead(t, c, proxy.FrameAccept)
		mustRead(t, c, proxy.FrameGrant) // the session is in its read loop once the grant is out
	})
	if !strings.Contains(logs.String(), "decision sink session disconnected") {
		t.Fatalf("the session ended after its test, not before it returned; log so far:\n%s", logs.String())
	}
}

// Tick forgets a run whose spool was erased, with no loss-row warning on every tick, and a settled run
// whose spool is sealed: neither keeps its handles or is read again.
func TestSpoolSinkTickForgetsErasedAndSealedRuns(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	registered := func(h *sinkHarness) bool {
		h.sink.mu.Lock()
		defer h.sink.mu.Unlock()
		return h.sink.runs[h.id] != nil
	}
	t.Run("erased", func(t *testing.T) {
		ctx := t.Context()
		h := newSinkHarness(t)
		if err := os.RemoveAll(filepath.Join(h.dir, "spools", h.id.String())); err != nil {
			t.Fatal(err)
		}
		h.sink.Tick(ctx)
		h.sink.Tick(ctx)
		if registered(h) || strings.Contains(logs.String(), "loss row not written") {
			t.Fatalf("after its spool was erased the run is still registered (%v), or Tick warned:\n%s", registered(h), logs.String())
		}
		if _, err := h.sink.ProducerGone(ctx, h.id); !errors.Is(err, errSinkRunUnknown) {
			t.Fatalf("settling a forgotten run = %v, want %v", err, errSinkRunUnknown)
		}
	})
	t.Run("sealed", func(t *testing.T) {
		ctx := t.Context()
		h := newSinkHarness(t)
		if _, err := h.sink.ProducerGone(ctx, h.id); err != nil {
			t.Fatal(err)
		}
		h.sink.Tick(ctx)
		if !registered(h) {
			t.Fatal("a settled run was forgotten before its spool was sealed")
		}
		s := h.spool()
		r, err := s.Receipt(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := s.Seal(ctx, r); !ok || err != nil {
			t.Fatalf("seal = %v, %v", ok, err)
		}
		h.sink.Tick(ctx)
		if registered(h) {
			t.Fatal("a settled run whose spool is sealed is still registered")
		}
	})
}
