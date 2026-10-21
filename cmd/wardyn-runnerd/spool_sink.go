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
	"io/fs"
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

// sinkAcceptBound bounds Accept, which runs under the sink's lock: a spool whose file lock never frees
// (a hung disk) refuses that one session instead of stalling every run's hello, Tick and Drain.
var sinkAcceptBound = sinkHelloTimeout

var (
	errNoSinkSession = errors.New("decision sink: no live session for the run")
	// errSinkRunUnknown: the sink holds no such run, never registered or forgotten (sealed, or its spool
	// erased).
	errSinkRunUnknown = errors.New("decision sink: run not registered")
)

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
	// endVM is set only when Linux VM sandboxes are on (EndVMsOnFullSpool).
	endVM func(ctx context.Context, runID uuid.UUID) (isVM bool, err error)

	handlers sync.WaitGroup // the session goroutines Serve started
}

type sinkRun struct {
	id      uuid.UUID
	keyHash [32]byte
	spool   *runnerspool.Spool
	writer  *os.File // the held writer.lock; nil once the producer is settled
	sess    *sinkSession
	drained *runnerspool.Drained
	// fullHandled: the run's full spool was seen and acted on (EndVMsWithFullSpools); guarded by spoolSink.mu.
	fullHandled bool
	// endWarned is when a failed end of the run's VM was last logged; guarded by spoolSink.mu.
	endWarned time.Time
}

type sinkSession struct {
	run      *sinkRun
	conn     net.Conn
	gen      uint64
	instance string
	gmu      sync.Mutex // grant, epoch, bytesSince: the pump and Tick both regrant
	grant    runnerspool.Grant
	// bytesSince is the record-body bytes committed above grant.Base, the
	// same bytes the proxy counts against grant.Bytes.
	bytesSince int64
	epoch      uuid.UUID
	wmu        sync.Mutex
	drainCh    chan runnerspool.Drained
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

// Succeed hands a run to a new proxy under a new key (Docker runners): a
// revive starts a new proxy process, a new producer. A run not registered yet
// is registered. A registered run's producer must be settled (ProducerGone)
// first, so the new producer never overlaps the old one; the old key stops
// authenticating in the same step.
func (s *spoolSink) Succeed(runID uuid.UUID, keyHash [32]byte) error {
	s.mu.Lock()
	r := s.runs[runID]
	if r == nil {
		s.mu.Unlock()
		return s.Register(runID, keyHash)
	}
	defer s.mu.Unlock()
	if r.writer != nil || r.sess != nil {
		return fmt.Errorf("decision sink: run %s still has a live producer", runID)
	}
	w, err := r.spool.HoldWriter()
	if err != nil {
		return err
	}
	if err := r.spool.Succeed(context.Background()); err != nil {
		return errors.Join(err, w.Close())
	}
	r.keyHash, r.writer, r.drained = keyHash, w, nil
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
		s.handlers.Add(1)
		go func() {
			defer s.handlers.Done()
			s.handle(tls.Server(c, s.tls))
		}()
	}
}

// hello authenticates the first frame: the run is registered and sha256(key)
// matches its stored hash, compared in constant time. It returns the hash it
// matched, which handle checks again at Accept (Succeed may replace it).
func (s *spoolSink) hello(c *tls.Conn) (*sinkRun, [32]byte, string, error) {
	_ = c.SetDeadline(time.Now().Add(sinkHelloTimeout))
	if err := c.Handshake(); err != nil {
		return nil, [32]byte{}, "", err
	}
	if c.ConnectionState().NegotiatedProtocol != proxy.SinkALPN {
		return nil, [32]byte{}, "", errors.New("decision sink: wrong protocol")
	}
	f, err := proxy.ReadSinkFrame(c)
	if err != nil {
		return nil, [32]byte{}, "", err
	}
	key, kerr := hex.DecodeString(f.Key)
	id, ierr := uuid.Parse(f.RunID)
	s.mu.Lock()
	r := s.runs[id]
	want := [32]byte{}
	if r != nil {
		want = r.keyHash
	}
	s.mu.Unlock()
	got := sha256.Sum256(key)
	match := subtle.ConstantTimeCompare(got[:], want[:]) == 1
	if f.T != proxy.FrameHello || f.V != proxy.SinkVersion || kerr != nil || len(key) != 32 || ierr != nil || r == nil || !match {
		return nil, [32]byte{}, "", errors.New("decision sink: hello refused")
	}
	return r, want, f.Instance, nil
}

func (s *spoolSink) handle(c *tls.Conn) {
	defer c.Close()
	r, keyHash, instance, err := s.hello(c)
	if err != nil {
		return
	}
	ctx := context.Background()
	epoch := runneractivity.Capture(s.opts.ActivityDir(r.id))
	// Invariant: a session is accepted and published in one section under the sink's lock, the one
	// ProducerGone settles under. Accept persists the new generation and the grant before the reply, with
	// the key checked again, so a hello that passed with a key Succeed has since replaced never claims the
	// new producer's spool; and a settlement either takes this session or comes first, when Accept refuses
	// the settled producer.
	s.mu.Lock()
	if s.runs[r.id] != r || r.keyHash != keyHash {
		s.mu.Unlock()
		return
	}
	actx, cancel := context.WithTimeout(ctx, sinkAcceptBound)
	acc, err := r.spool.Accept(actx, instance)
	cancel()
	if err != nil {
		s.mu.Unlock()
		slog.Warn("runnerd: decision sink session refused", slog.String("run_id", r.id.String()), slog.Any("err", err))
		return
	}
	ss := &sinkSession{run: r, conn: c, gen: acc.Gen, instance: instance, grant: acc.Grant, drainCh: make(chan runnerspool.Drained, 1), epoch: epoch}
	old := r.sess
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
			var added int64
			if fresh {
				added = int64(len(f.Body))
			}
			if err := s.regrant(ctx, ss, f.Seq, added); err != nil {
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
// sending it: after a record (seq > 0, carrying body bytes) once half the
// grant's records or half its bytes are used, from Tick (seq 0) whenever the
// credit or the epoch changed.
func (s *spoolSink) regrant(ctx context.Context, ss *sinkSession, seq, body int64) error {
	ss.gmu.Lock()
	defer ss.gmu.Unlock()
	ss.bytesSince += body
	if seq > 0 && ss.grant.Through-seq >= runnerspool.GrantRecords/2 && 2*ss.bytesSince < ss.grant.Bytes {
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
	if changed {
		ss.bytesSince = 0 // the new grant's Base is the committed producer seq
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
// releases the writer lock, so Seal waits only for the org's ack. The settlement
// runs under the sink's lock, as handle's Accept does (the invariant there). A
// record the closing session still sends is refused by the settled grant.
func (s *spoolSink) ProducerGone(ctx context.Context, runID uuid.UUID) (runnerspool.Loss, error) {
	s.mu.Lock()
	r := s.runs[runID]
	if r == nil {
		s.mu.Unlock()
		return runnerspool.Loss{}, fmt.Errorf("%w: %s", errSinkRunUnknown, runID)
	}
	ss := r.sess
	r.sess = nil
	loss, err := r.spool.SettleLoss(ctx, r.drained)
	if err == nil {
		_, err = r.spool.EmitLoss(ctx)
	}
	var w *os.File
	if err == nil {
		w, r.writer = r.writer, nil
	}
	s.mu.Unlock()
	if ss != nil {
		ss.conn.Close()
	}
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
// epoch) and retries loss rows waiting on the reserve. It forgets a run whose
// spool directory is gone (its evidence erased) and a settled run whose spool
// is sealed (retired): neither has anything left for the sink, and each would
// otherwise keep its handles and be read every tick for the process's life.
func (s *spoolSink) Tick(ctx context.Context) {
	s.mu.Lock()
	runs := make([]*sinkRun, 0, len(s.runs))
	for _, r := range s.runs {
		runs = append(runs, r)
	}
	s.mu.Unlock()
	for _, r := range runs {
		if _, err := r.spool.EmitLoss(ctx); err != nil {
			if _, serr := os.Stat(s.opts.SpoolDir(r.id)); errors.Is(serr, fs.ErrNotExist) {
				s.forget(r)
				continue
			}
			slog.Warn("runnerd: decision sink loss row not written", slog.String("run_id", r.id.String()), slog.Any("err", err))
		}
		s.mu.Lock()
		ss, settled := r.sess, r.writer == nil && r.sess == nil
		s.mu.Unlock()
		if settled {
			if l, err := r.spool.Liability(ctx); err == nil && l.Sealed {
				s.forget(r)
				continue
			}
		}
		if ss != nil {
			if err := s.regrant(ctx, ss, 0, 0); err != nil {
				ss.conn.Close()
			}
		}
	}
}

// endWarnEvery spaces the log lines of a VM end that keeps failing.
var endWarnEvery = time.Minute

// EndVMsOnFullSpool turns on the full-spool rule for Linux VM sandboxes: end is asked to end a run's
// VM and reports false for a run that is not one. Without it a full spool only stops the run's proxy
// admitting (the proxy holds a request, then answers a retryable 503).
func (s *spoolSink) EndVMsOnFullSpool(end func(ctx context.Context, runID uuid.UUID) (bool, error)) {
	s.mu.Lock()
	s.endVM = end
	s.mu.Unlock()
}

// EndVMsWithFullSpools ends the Linux VM of each run whose spool has no room left (Spool.Full). The
// spool is capped, and a VM that keeps running past the cap would act with its decisions unrecorded.
// Called from the containment tick; a run is acted on once, and an end that failed is tried again.
func (s *spoolSink) EndVMsWithFullSpools(ctx context.Context) {
	s.mu.Lock()
	end := s.endVM
	var runs []*sinkRun
	for _, r := range s.runs {
		if !r.fullHandled {
			runs = append(runs, r)
		}
	}
	s.mu.Unlock()
	if end == nil {
		return
	}
	for _, r := range runs {
		if full, err := r.spool.Full(ctx); err != nil || !full {
			continue
		}
		isVM, err := end(ctx, r.id)
		if err != nil {
			// Retried every tick: log the first failure, then once per endWarnEvery.
			s.mu.Lock()
			due := r.endWarned.IsZero() || time.Since(r.endWarned) >= endWarnEvery
			if due {
				r.endWarned = time.Now()
			}
			s.mu.Unlock()
			if due {
				slog.Warn("runnerd: ending a Linux VM whose audit spool is full will retry", slog.String("run_id", r.id.String()), slog.Any("err", err))
			}
			continue
		}
		s.mu.Lock()
		r.fullHandled = true
		s.mu.Unlock()
		if isVM {
			slog.Warn("runnerd: a Linux VM's audit spool is full, so its VM was ended", slog.String("run_id", r.id.String()))
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

// forget drops r and releases its handles, unless another registration has taken its id since.
func (s *spoolSink) forget(r *sinkRun) {
	s.mu.Lock()
	if s.runs[r.id] != r {
		s.mu.Unlock()
		return
	}
	delete(s.runs, r.id)
	ss := r.sess
	r.sess = nil
	s.mu.Unlock()
	r.close(ss)
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
