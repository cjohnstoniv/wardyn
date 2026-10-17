// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// The decision sink (design §3.2, D9): on k8s runners the proxy pod streams
// its decisions here, and runnerd is the only spool writer. Each record gets
// one durable disposition in the spool manifest before it is acked; credit is
// granted write-ahead, so after any crash the liability is known before any
// regrant; loss is settled only once the producer is proven gone.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runneractivity"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
)

const (
	sinkHelloTimeout = 10 * time.Second
	sinkDrainTimeout = 5 * time.Second
)

var errNoSinkSession = errors.New("decision sink: no live session for the run")

type spoolSinkOptions struct {
	Cert         tls.Certificate
	SpoolDir     func(runID uuid.UUID) string
	ActivityDir  func(runID uuid.UUID) string
	OnLapsed     func(runID uuid.UUID, reason string)
	OnSessionEnd func(runID uuid.UUID)
}

type spoolSink struct {
	opts spoolSinkOptions
	pin  string
	tls  *tls.Config

	mu     sync.Mutex
	runs   map[uuid.UUID]*sinkRun
	lns    []net.Listener
	closed bool
}

type sinkRun struct {
	id      uuid.UUID
	keyHash [32]byte
	spool   *runnerspool.Spool
	writer  *os.File // the held writer.lock; nil once the producer is settled
	sess    *sinkSession
	drained *runnerspool.Drained
}

type sinkSession struct {
	run      *sinkRun
	conn     net.Conn
	gen      uint64
	instance string
	gmu      sync.Mutex // grant and epoch: the pump and Tick both regrant
	grant    runnerspool.Grant
	epoch    uuid.UUID
	wmu      sync.Mutex
	drainCh  chan runnerspool.Drained
}

func (ss *sinkSession) send(f proxy.SinkFrame) error {
	ss.wmu.Lock()
	defer ss.wmu.Unlock()
	f.Gen = ss.gen
	_ = ss.conn.SetWriteDeadline(time.Now().Add(sinkHelloTimeout))
	return proxy.WriteSinkFrame(ss.conn, f)
}

// loadOrCreateSinkCert keeps the sink's key at path (0600) so its pin survives
// a runnerd restart; the self-signed certificate around it is rebuilt each
// start. It returns the pin: hex SHA-256 of the SubjectPublicKeyInfo.
func loadOrCreateSinkCert(path string) (tls.Certificate, string, error) {
	var key *ecdsa.PrivateKey
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		st, serr := os.Lstat(path)
		if serr != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 {
			return tls.Certificate{}, "", errors.New("decision sink key file must be a private regular file")
		}
		blk, _ := pem.Decode(b)
		if blk == nil {
			return tls.Certificate{}, "", errors.New("decision sink key file is not PEM")
		}
		if key, err = x509.ParseECPrivateKey(blk.Bytes); err != nil {
			return tls.Certificate{}, "", err
		}
	case errors.Is(err, os.ErrNotExist):
		if key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			return tls.Certificate{}, "", err
		}
		der, merr := x509.MarshalECPrivateKey(key)
		if merr != nil {
			return tls.Certificate{}, "", merr
		}
		f, oerr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if oerr != nil {
			return tls.Certificate{}, "", oerr
		}
		werr := pem.Encode(f, &pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
		if err := errors.Join(werr, f.Sync(), f.Close()); err != nil {
			return tls.Certificate{}, "", err
		}
	default:
		return tls.Certificate{}, "", err
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, hex.EncodeToString(sum[:]), nil
}

// newSinkKey makes a run's 32-byte key: the hex goes into the proxy config,
// only its hash is kept.
func newSinkKey() (string, [32]byte, error) {
	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		return "", [32]byte{}, err
	}
	return hex.EncodeToString(k[:]), sha256.Sum256(k[:]), nil
}

func newSpoolSink(opts spoolSinkOptions) *spoolSink {
	pin := ""
	if len(opts.Cert.Certificate) > 0 {
		if leaf, err := x509.ParseCertificate(opts.Cert.Certificate[0]); err == nil {
			sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
			pin = hex.EncodeToString(sum[:])
		}
	}
	return &spoolSink{opts: opts, pin: pin, runs: map[uuid.UUID]*sinkRun{}, tls: &tls.Config{
		MinVersion: tls.VersionTLS13, NextProtos: []string{proxy.SinkALPN}, Certificates: []tls.Certificate{opts.Cert},
	}}
}

// SPKIPin is decision_sink.spki_sha256 for every proxy config.
func (s *spoolSink) SPKIPin() string { return s.pin }

// Register opens the run's spool and holds its writer lock, so Seal defers,
// unless the producer is already settled. K2 calls it before creating the
// proxy pod and again for every non-retired run at startup, before Online.
func (s *spoolSink) Register(runID uuid.UUID, keyHash [32]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("decision sink closed")
	}
	if _, ok := s.runs[runID]; ok {
		return fmt.Errorf("decision sink: run %s already registered", runID)
	}
	sp, err := runnerspool.Open(s.opts.SpoolDir(runID), runID)
	if err != nil {
		return err
	}
	l, err := sp.Liability(context.Background())
	if err != nil {
		sp.Close()
		return err
	}
	r := &sinkRun{id: runID, keyHash: keyHash, spool: sp}
	if !l.Settled() {
		if r.writer, err = sp.HoldWriter(); err != nil {
			sp.Close()
			return err
		}
	}
	s.runs[runID] = r
	return nil
}

// Serve accepts proxy sessions on ln until Close.
func (s *spoolSink) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return net.ErrClosed
	}
	s.lns = append(s.lns, ln)
	s.mu.Unlock()
	for {
		c, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		go s.handle(tls.Server(c, s.tls))
	}
}

// hello authenticates the first frame: the run is registered and sha256(key)
// matches its stored hash, compared in constant time.
func (s *spoolSink) hello(c *tls.Conn) (*sinkRun, string, error) {
	_ = c.SetDeadline(time.Now().Add(sinkHelloTimeout))
	if err := c.Handshake(); err != nil {
		return nil, "", err
	}
	if c.ConnectionState().NegotiatedProtocol != proxy.SinkALPN {
		return nil, "", errors.New("decision sink: wrong protocol")
	}
	f, err := proxy.ReadSinkFrame(c)
	if err != nil {
		return nil, "", err
	}
	key, kerr := hex.DecodeString(f.Key)
	id, ierr := uuid.Parse(f.RunID)
	s.mu.Lock()
	r := s.runs[id]
	s.mu.Unlock()
	want := [32]byte{}
	if r != nil {
		want = r.keyHash
	}
	got := sha256.Sum256(key)
	match := subtle.ConstantTimeCompare(got[:], want[:]) == 1
	if f.T != proxy.FrameHello || f.V != proxy.SinkVersion || kerr != nil || len(key) != 32 || ierr != nil || r == nil || !match {
		return nil, "", errors.New("decision sink: hello refused")
	}
	return r, f.Instance, nil
}

func (s *spoolSink) handle(c *tls.Conn) {
	defer c.Close()
	r, instance, err := s.hello(c)
	if err != nil {
		return
	}
	ctx := context.Background()
	// Accept persists the new generation and the grant before the reply.
	acc, err := r.spool.Accept(ctx, instance)
	if err != nil {
		slog.Warn("runnerd: decision sink session refused", slog.String("run_id", r.id.String()), slog.Any("err", err))
		return
	}
	ss := &sinkSession{run: r, conn: c, gen: acc.Gen, instance: instance, grant: acc.Grant, drainCh: make(chan runnerspool.Drained, 1),
		epoch: runneractivity.Capture(s.opts.ActivityDir(r.id))}
	s.mu.Lock()
	old := r.sess
	if s.runs[r.id] != r {
		s.mu.Unlock()
		return
	}
	r.sess = ss
	s.mu.Unlock()
	if old != nil {
		old.conn.Close() // superseded: its gen is fenced in the manifest already
	}
	_ = c.SetDeadline(time.Time{})
	if ss.send(proxy.SinkFrame{T: proxy.FrameAccept, Seq: acc.Seq}) != nil || ss.send(grantFrame(acc.Grant, ss.epoch)) != nil {
		s.ended(ss)
		return
	}
	err = s.pump(ctx, ss)
	s.ended(ss)
	if err != nil {
		slog.Info("runnerd: decision sink session disconnected", slog.String("run_id", r.id.String()), slog.Any("err", err))
	}
}

func grantFrame(g runnerspool.Grant, epoch uuid.UUID) proxy.SinkFrame {
	return proxy.SinkFrame{T: proxy.FrameGrant, Epoch: epoch, Base: g.Base, Through: g.Through, Bytes: g.Bytes}
}

// ended marks a session disconnected (never an exit, R3-M1) and asks K2 for
// an immediate pod read.
func (s *spoolSink) ended(ss *sinkSession) {
	ss.conn.Close()
	s.mu.Lock()
	current := ss.run.sess == ss
	if current {
		ss.run.sess = nil
	}
	s.mu.Unlock()
	if current && s.opts.OnSessionEnd != nil {
		s.opts.OnSessionEnd(ss.run.id)
	}
}

func (s *spoolSink) pump(ctx context.Context, ss *sinkSession) error {
	r := ss.run
	for {
		f, err := proxy.ReadSinkFrame(ss.conn)
		if err != nil {
			return err
		}
		if f.Gen != ss.gen {
			return errors.New("frame from another generation")
		}
		switch f.T {
		case proxy.FrameRecord:
			var body proxy.RecordBody
			if err := json.Unmarshal(f.Body, &body); err != nil {
				return err
			}
			sum := sha256.Sum256(f.Body)
			disp, fresh, err := r.spool.Produce(ctx, runnerspool.Produced{Gen: ss.gen, Instance: ss.instance, Seq: f.Seq,
				Hash: hex.EncodeToString(sum[:]), Admitted: body.Admitted, Decision: body.Decision})
			if err != nil {
				return err
			}
			if fresh && disp == runnerspool.Appended && body.Activity {
				_ = runneractivity.Record(s.opts.ActivityDir(r.id), body.Epoch)
			}
			if err := ss.send(proxy.SinkFrame{T: proxy.FrameAck, Seq: f.Seq, Disposition: string(disp)}); err != nil {
				return err
			}
			if err := s.regrant(ctx, ss, f.Seq); err != nil {
				return err
			}
		case proxy.FrameRefused:
			if err := r.spool.ProducerRefused(ctx, ss.gen, ss.instance, f.Total); err != nil {
				return err
			}
		case proxy.FrameDrained:
			d := runnerspool.Drained{Gen: ss.gen, FinalSeq: f.FinalSeq, Consumed: f.Consumed}
			s.mu.Lock()
			r.drained = &d
			s.mu.Unlock()
			select {
			case ss.drainCh <- d:
			default:
			}
		case proxy.FrameLapsed:
			// Evidence only on an authenticated session at the current gen.
			s.mu.Lock()
			current := r.sess == ss
			s.mu.Unlock()
			if l, err := r.spool.Liability(ctx); err == nil && current && l.Gen == ss.gen && s.opts.OnLapsed != nil {
				s.opts.OnLapsed(r.id, f.Reason)
			}
		default:
			return fmt.Errorf("unexpected frame %q", f.T)
		}
	}
}

// regrant persists fresh credit (and the current activity epoch) before
// sending it: after a record (seq > 0) only once half the grant is used, from
// Tick (seq 0) whenever the credit or the epoch changed.
func (s *spoolSink) regrant(ctx context.Context, ss *sinkSession, seq int64) error {
	ss.gmu.Lock()
	defer ss.gmu.Unlock()
	if seq > 0 && ss.grant.Through-seq >= runnerspool.GrantRecords/2 {
		return nil
	}
	epoch := runneractivity.Capture(s.opts.ActivityDir(ss.run.id))
	g, changed, err := ss.run.spool.Regrant(ctx, ss.gen, ss.instance)
	if err != nil {
		return err
	}
	if !changed && epoch == ss.epoch {
		return nil
	}
	ss.grant, ss.epoch = g, epoch
	return ss.send(grantFrame(g, epoch))
}

// Drain asks the run's proxy to stop reserving and flush, waiting at most 5 s.
// Containment proceeds whatever it returns.
func (s *spoolSink) Drain(ctx context.Context, runID uuid.UUID) (runnerspool.Drained, error) {
	s.mu.Lock()
	r := s.runs[runID]
	var ss *sinkSession
	if r != nil {
		ss = r.sess
	}
	s.mu.Unlock()
	if ss == nil {
		return runnerspool.Drained{}, errNoSinkSession
	}
	if err := ss.send(proxy.SinkFrame{T: proxy.FrameDrain}); err != nil {
		return runnerspool.Drained{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, sinkDrainTimeout)
	defer cancel()
	select {
	case d := <-ss.drainCh:
		return d, nil
	case <-ctx.Done():
		return runnerspool.Drained{}, ctx.Err()
	}
}

// ProducerGone writes off the run's liability on completion evidence only
// (never a NotFound alone or termination unknown): exact after a drained reply
// at the current gen with every sequence committed, else the upper bound. It
// emits the loss row when the reserve is free (Tick retries otherwise) and
// releases the writer lock, so Seal waits only for the org's ack.
func (s *spoolSink) ProducerGone(ctx context.Context, runID uuid.UUID) (runnerspool.Loss, error) {
	s.mu.Lock()
	r := s.runs[runID]
	var ss *sinkSession
	var drained *runnerspool.Drained
	if r != nil {
		ss, r.sess, drained = r.sess, nil, r.drained
	}
	s.mu.Unlock()
	if r == nil {
		return runnerspool.Loss{}, fmt.Errorf("decision sink: run %s not registered", runID)
	}
	if ss != nil {
		ss.conn.Close()
	}
	loss, err := r.spool.SettleLoss(ctx, drained)
	if err != nil {
		return loss, err
	}
	if _, err := r.spool.EmitLoss(ctx); err != nil {
		return loss, err
	}
	s.mu.Lock()
	w := r.writer
	r.writer = nil
	s.mu.Unlock()
	if w != nil {
		err = w.Close()
	}
	return loss, err
}

// Liability reads the run's durable producer state.
func (s *spoolSink) Liability(ctx context.Context, runID uuid.UUID) (runnerspool.Liability, error) {
	s.mu.Lock()
	r := s.runs[runID]
	s.mu.Unlock()
	if r == nil {
		return runnerspool.Liability{}, fmt.Errorf("decision sink: run %s not registered", runID)
	}
	return r.spool.Liability(ctx)
}

// Tick regrants live sessions (fresh capacity after org acks, a new activity
// epoch) and retries loss rows waiting on the reserve.
func (s *spoolSink) Tick(ctx context.Context) {
	s.mu.Lock()
	runs := make([]*sinkRun, 0, len(s.runs))
	for _, r := range s.runs {
		runs = append(runs, r)
	}
	s.mu.Unlock()
	for _, r := range runs {
		if _, err := r.spool.EmitLoss(ctx); err != nil {
			slog.Warn("runnerd: decision sink loss row not written", slog.String("run_id", r.id.String()), slog.Any("err", err))
		}
		s.mu.Lock()
		ss := r.sess
		s.mu.Unlock()
		if ss != nil {
			if err := s.regrant(ctx, ss, 0); err != nil {
				ss.conn.Close()
			}
		}
	}
}

// Unregister forgets a settled or retired run.
func (s *spoolSink) Unregister(runID uuid.UUID) {
	s.mu.Lock()
	r := s.runs[runID]
	delete(s.runs, runID)
	var ss *sinkSession
	if r != nil {
		ss, r.sess = r.sess, nil
	}
	s.mu.Unlock()
	if r != nil {
		r.close(ss)
	}
}

func (r *sinkRun) close(ss *sinkSession) {
	if ss != nil {
		ss.conn.Close()
	}
	if r.writer != nil {
		r.writer.Close()
	}
	r.spool.Close()
}

// Close stops serving and releases every run's handles. The writer locks go
// with the process, as a crash would drop them; the manifest's grant check
// keeps Seal deferred regardless (P3).
func (s *spoolSink) Close() error {
	s.mu.Lock()
	s.closed = true
	lns, runs := s.lns, s.runs
	s.runs = map[uuid.UUID]*sinkRun{}
	sess := map[*sinkRun]*sinkSession{}
	for _, r := range runs {
		sess[r], r.sess = r.sess, nil
	}
	s.mu.Unlock()
	var err error
	for _, ln := range lns {
		err = errors.Join(err, ln.Close())
	}
	for _, r := range runs {
		r.close(sess[r])
	}
	return err
}
