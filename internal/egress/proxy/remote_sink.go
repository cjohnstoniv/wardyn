// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
)

// The decision sink wire (k8s runners): TLS 1.3 with ALPN SinkALPN, the proxy
// pinning runnerd's SPKI; frames are a 4-byte big-endian length and one JSON
// SinkFrame. The proxy's first frame is hello.
const (
	SinkALPN     = "wardyn-sink/1"
	SinkVersion  = 1
	MaxSinkFrame = 1<<20 + 8<<10
)

// Frame names. Proxy to runnerd: hello, record, refused, drained, lapsed.
// runnerd to proxy: accept, grant, ack, drain.
const (
	FrameHello   = "hello"
	FrameRecord  = "record"
	FrameRefused = "refused"
	FrameDrained = "drained"
	FrameLapsed  = "lapsed"
	FrameAccept  = "accept"
	FrameGrant   = "grant"
	FrameAck     = "ack"
	FrameDrain   = "drain"
)

// SinkFrame is every sink frame; T names it and only its own fields are set.
type SinkFrame struct {
	T string `json:"t"`
	// hello
	V        int    `json:"v,omitempty"`
	RunID    string `json:"run_id,omitempty"`
	Instance string `json:"instance,omitempty"`
	Key      string `json:"key,omitempty"`
	// every frame after hello
	Gen uint64 `json:"gen,omitempty"`
	// record, accept, ack
	Seq  int64           `json:"seq,omitempty"`
	Body json.RawMessage `json:"body,omitempty"`
	// ack
	Disposition string `json:"disposition,omitempty"`
	// refused
	Total uint64 `json:"total,omitempty"`
	// drained
	FinalSeq int64  `json:"final_seq,omitempty"`
	Consumed uint64 `json:"consumed,omitempty"`
	// lapsed
	Reason string `json:"reason,omitempty"`
	// grant
	Epoch   uuid.UUID `json:"epoch,omitempty"`
	Base    int64     `json:"base,omitempty"`
	Through int64     `json:"through,omitempty"`
	Bytes   int64     `json:"bytes,omitempty"`
}

// RecordBody is a record frame's body. Its hash is the SHA-256 of the body
// bytes as sent, and a resend carries the identical bytes.
type RecordBody struct {
	Admitted bool               `json:"admitted,omitempty"`
	Epoch    uuid.UUID          `json:"epoch"`
	Activity bool               `json:"activity,omitempty"`
	Decision egress.DecisionLog `json:"decision"`
}

// WriteSinkFrame writes one length-prefixed frame.
func WriteSinkFrame(w io.Writer, f SinkFrame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(b) > MaxSinkFrame {
		return errors.New("sink frame too large")
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err = w.Write(buf)
	return err
}

// ReadSinkFrame reads one length-prefixed frame, refusing an oversized one.
func ReadSinkFrame(r io.Reader) (SinkFrame, error) {
	var f SinkFrame
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return f, err
	}
	size := binary.BigEndian.Uint32(n[:])
	if size == 0 || size > MaxSinkFrame {
		return f, errors.New("sink frame size out of bounds")
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return f, err
	}
	err := json.Unmarshal(b, &f)
	return f, err
}

// ParseSinkURL returns the host:port of a tls://host:port sink URL.
func ParseSinkURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "tls" || u.Host == "" || u.Port() == "" || u.Path != "" || u.RawQuery != "" || u.User != nil {
		return "", errors.New("decision_sink.url must be tls://host:port")
	}
	return u.Host, nil
}

func validHex32(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == s
}

// validate is decision_sink's boot check.
func (c DecisionSinkConfig) validate() error {
	if _, err := ParseSinkURL(c.URL); err != nil {
		return err
	}
	if !validHex32(c.SPKISHA256) || !validHex32(c.Key) {
		return errors.New("decision_sink.spki_sha256 and decision_sink.key must be 64 lowercase hex")
	}
	return nil
}

// sinkPinConfig trusts only the server key whose SPKI SHA-256 is pin.
func sinkPinConfig(pin string) *tls.Config {
	want, _ := hex.DecodeString(pin)
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{SinkALPN},
		InsecureSkipVerify: true, //nolint:gosec // VerifyConnection pins the SPKI; no CA or name is trusted
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || cs.NegotiatedProtocol != SinkALPN {
				return errors.New("decision sink: no certificate or wrong protocol")
			}
			got := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if subtle.ConstantTimeCompare(got[:], want) != 1 {
				return errors.New("decision sink: certificate key does not match the pin")
			}
			return nil
		},
	}
}

type sinkRec struct {
	seq  int64
	body json.RawMessage
}

// remoteSink is the proxy's side of the decision sink (§3.2). It holds
// records in memory until runnerd acks them durable, within runnerd's
// write-ahead grant (at most GrantRecords / GrantBytes beyond the durable
// position): with no credit left a new request is refused, as a full local
// spool refuses, and counted in the cumulative refused total. A session that
// ends is reconnected and the unacked tail resent at the new generation.
type remoteSink struct {
	runID    uuid.UUID
	addr     string
	key      string
	tls      *tls.Config
	instance string
	dial     func(ctx context.Context) (net.Conn, error)
	retry    time.Duration
	drainFor time.Duration

	mu           sync.Mutex
	cond         *sync.Cond
	gen          uint64
	live         bool // a session is accepted and not yet ended
	haveGrant    bool
	epoch        uuid.UUID
	base         int64
	through      int64
	bytes        int64
	sinceBase    int64 // record-body bytes of seqs above base
	lastSeq      int64
	acked        int64
	buf          []sinkRec
	res          map[uuid.UUID]spoolReservation
	heldBytes    int64
	refused      uint64
	draining     bool
	drainStarted bool
	lapsePending bool
	tunnels      map[net.Conn]struct{}
	stopped      bool // drained replied: no further records
	drained      *SinkFrame
	lapsed       *SinkFrame
	closed       bool
	conn         net.Conn
}

func newRemoteSink(runID uuid.UUID, c DecisionSinkConfig) (*remoteSink, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	addr, _ := ParseSinkURL(c.URL)
	var inst [16]byte
	if _, err := rand.Read(inst[:]); err != nil {
		return nil, err
	}
	s := &remoteSink{runID: runID, addr: addr, key: c.Key, tls: sinkPinConfig(c.SPKISHA256), instance: hex.EncodeToString(inst[:]),
		retry: time.Second, drainFor: 3 * time.Second, res: map[uuid.UUID]spoolReservation{}}
	s.cond = sync.NewCond(&s.mu)
	s.dial = func(ctx context.Context) (net.Conn, error) {
		d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: s.tls}
		return d.DialContext(ctx, "tcp", s.addr)
	}
	go s.run()
	return s, nil
}

// fitsLocked reports whether one more credit unit carrying need bytes fits
// the held grant: sequences assigned plus reservations held stay within
// Through, and bytes since Base plus held bytes within Bytes. The grant
// outlives a session end (design Q5, §6.2): runnerd never re-issues it lower.
func (s *remoteSink) fitsLocked(need int64) bool {
	return s.haveGrant && s.lastSeq+int64(len(s.res)) < s.through && s.sinceBase+s.heldBytes+need <= s.bytes
}

// countRefusalLocked counts a want-of-credit refusal for Drops only when the
// spool is genuinely full: a grant is held and exhausted, during an outage too
// (§6.2: the write loop forwards the total on the reconnect). No grant yet, a
// drain, a stop or a close refuses uncounted.
func (s *remoteSink) countRefusalLocked() {
	if s.haveGrant && !s.draining && !s.stopped && !s.closed {
		s.refused++
		s.cond.Broadcast()
	}
}

// reserve holds credit for one admitted request, waiting until ctx ends for
// credit to return (a request finishing, or a regrant) rather than refusing
// on the first miss.
func (s *remoteSink) reserve(ctx context.Context, need int64) (uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { s.mu.Lock(); s.cond.Broadcast(); s.mu.Unlock() })
	defer stop()
	for !s.draining && !s.stopped && !s.closed && !s.fitsLocked(need) && ctx.Err() == nil {
		s.cond.Wait()
	}
	if s.draining || s.stopped || s.closed || !s.fitsLocked(need) {
		// A client that left refused nothing; a deadline that expired on an
		// exhausted grant is the full spool.
		if err := ctx.Err(); errors.Is(err, context.Canceled) {
			return uuid.Nil, err
		}
		if !s.draining && !s.stopped && !s.closed {
			s.countRefusalLocked()
		}
		return uuid.Nil, runnerspool.ErrFull
	}
	id := uuid.New()
	s.res[id] = spoolReservation{epoch: s.epoch, bytes: need}
	s.heldBytes += need
	return id, nil
}

func (s *remoteSink) dropLocked(id uuid.UUID) {
	s.heldBytes -= s.res[id].bytes
	delete(s.res, id)
	s.cond.Broadcast()
}

func (s *remoteSink) release(id uuid.UUID) {
	s.mu.Lock()
	s.dropLocked(id)
	s.mu.Unlock()
}

func (s *remoteSink) emit(log egress.DecisionLog) {
	b, err := json.Marshal(log)
	if err == nil {
		err = json.Unmarshal(maskDecisionBytes(b), &log)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := log.Request.AuditReservation
	held, admitted := s.res[id]
	admitted = admitted && log.Decision != egress.Pending
	epoch := s.epoch
	if admitted {
		s.dropLocked(id)
		epoch = held.epoch
	}
	body, merr := json.Marshal(RecordBody{Admitted: admitted, Epoch: epoch, Decision: log,
		Activity: admitted && log.RuleSource != ruleSourceApprovals && log.RuleSource != "credential:reauth-timeout"})
	switch {
	case err != nil || merr != nil || s.closed || s.stopped:
		// Never counted: an admitted record here is already in the drained
		// reply's consumed (the exact loss), and nothing else is a full spool.
		slog.Warn("wardyn-proxy: decision not sent to the runner sink", slog.Bool("admitted", admitted))
	case !admitted && !s.fitsLocked(int64(len(body))):
		s.countRefusalLocked()
	default:
		s.lastSeq++
		s.sinceBase += int64(len(body))
		s.buf = append(s.buf, sinkRec{seq: s.lastSeq, body: body})
	}
	s.cond.Broadcast()
}

// beginDrainLocked starts the one drain: stop reserving, close open tunnels so
// their final decisions are emitted, then answer drained (§3.2 item 5).
func (s *remoteSink) beginDrainLocked() {
	s.draining = true
	if !s.drainStarted {
		s.drainStarted = true
		go s.drain()
	}
	s.cond.Broadcast()
}

// lapse drains exactly as a drain frame does, then queues lapsed after the
// drained reply, so runnerd settles the exact loss rather than the upper bound.
func (s *remoteSink) lapse(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lapsePending = true
	s.beginDrainLocked()
	for s.drained == nil && !s.closed {
		s.cond.Wait()
	}
	s.lapsed = &SinkFrame{T: FrameLapsed, Reason: reason}
	s.lapsePending = false
	s.cond.Broadcast()
}

// track registers an open tunnel's client connection so a drain can close it;
// one opened after the drain began is closed at once. The returned func
// unregisters it once the tunnel's final decision has been emitted.
func (s *remoteSink) track(c net.Conn) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining || s.stopped || s.closed {
		_ = c.Close()
	}
	if s.tunnels == nil {
		s.tunnels = map[net.Conn]struct{}{}
	}
	s.tunnels[c] = struct{}{}
	return func() {
		s.mu.Lock()
		delete(s.tunnels, c)
		s.cond.Broadcast()
		s.mu.Unlock()
	}
}

// trackTunnel is the Proxy's hook at each hijack: a no-op unless decisions go
// to a runner sink (k8s runners).
func (p *Proxy) trackTunnel(c net.Conn) func() {
	if p.sink == nil || p.sink.remote == nil {
		return func() {}
	}
	return p.sink.remote.track(c)
}

// close flushes the unacked tail (and a queued lapsed frame) until ctx ends,
// then ends the session for good.
func (s *remoteSink) close(ctx context.Context) error {
	s.mu.Lock()
	s.draining = true
	stop := context.AfterFunc(ctx, func() { s.mu.Lock(); s.cond.Broadcast(); s.mu.Unlock() })
	for ctx.Err() == nil && (len(s.buf) > 0 || s.lapsePending || s.lapsed != nil) {
		s.cond.Wait()
	}
	stop()
	unacked := len(s.buf)
	s.closed = true
	if s.conn != nil {
		s.conn.Close()
	}
	s.haveGrant = false
	s.cond.Broadcast()
	s.mu.Unlock()
	if unacked > 0 {
		return fmt.Errorf("decision sink: %d decisions not acknowledged at shutdown", unacked)
	}
	return nil
}

func (s *remoteSink) run() {
	for {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return
		}
		if err := s.session(); err != nil {
			slog.Warn("wardyn-proxy: decision sink session ended", slog.Any("err", err))
		}
		s.mu.Lock()
		s.live, s.conn = false, nil // the grant outlives the session (H1)
		s.cond.Broadcast()
		s.mu.Unlock()
		time.Sleep(s.retry)
	}
}

// session dials, says hello, and pumps frames until the connection ends.
func (s *remoteSink) session() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	conn, err := s.dial(ctx)
	cancel()
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := WriteSinkFrame(conn, SinkFrame{T: FrameHello, V: SinkVersion, RunID: s.runID.String(), Instance: s.instance, Key: s.key}); err != nil {
		return err
	}
	acc, err := ReadSinkFrame(conn)
	if err != nil {
		return err
	}
	if acc.T != FrameAccept || acc.Gen == 0 {
		return errors.New("decision sink: no accept")
	}
	_ = conn.SetDeadline(time.Time{})
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.gen, s.live, s.conn = acc.Gen, true, conn
	s.ackLocked(acc.Seq)
	s.mu.Unlock()
	readErr := make(chan error, 1)
	go func() { readErr <- s.read(conn) }()
	werr := s.write(conn, acc.Gen)
	conn.Close()
	return errors.Join(werr, <-readErr)
}

func (s *remoteSink) ackLocked(seq int64) {
	if seq > s.acked {
		s.acked = seq
	}
	i := 0
	for i < len(s.buf) && s.buf[i].seq <= s.acked {
		i++
	}
	s.buf = s.buf[i:]
	s.cond.Broadcast()
}

func (s *remoteSink) read(conn net.Conn) error {
	defer func() { s.mu.Lock(); s.live = false; s.cond.Broadcast(); s.mu.Unlock() }()
	for {
		f, err := ReadSinkFrame(conn)
		if err != nil {
			return err
		}
		s.mu.Lock()
		if f.Gen != s.gen {
			s.mu.Unlock()
			return errors.New("decision sink: frame from another generation")
		}
		switch f.T {
		case FrameAck:
			s.ackLocked(f.Seq)
		case FrameGrant:
			// Every record at or below Base is acked before the grant
			// arrives, so the buffered tail is exactly the bytes above it.
			s.haveGrant, s.epoch, s.base, s.through, s.bytes = true, f.Epoch, f.Base, f.Through, f.Bytes
			s.sinceBase = 0
			for _, r := range s.buf {
				if r.seq > f.Base {
					s.sinceBase += int64(len(r.body))
				}
			}
			s.cond.Broadcast()
		case FrameDrain:
			s.beginDrainLocked()
		default:
			s.mu.Unlock()
			return fmt.Errorf("decision sink: unexpected frame %q", f.T)
		}
		s.mu.Unlock()
	}
}

// drain closes the open tunnels, gives in-flight requests and closing tunnels
// up to drainFor to emit their final decisions, then answers drained with the
// last sequence and the credit consumed; reservations still held are the
// exact loss.
func (s *remoteSink) drain() {
	s.mu.Lock()
	for c := range s.tunnels {
		_ = c.Close()
	}
	deadline := time.Now().Add(s.drainFor)
	timer := time.AfterFunc(s.drainFor, func() { s.mu.Lock(); s.cond.Broadcast(); s.mu.Unlock() })
	for (len(s.res) > 0 || len(s.tunnels) > 0) && time.Now().Before(deadline) && !s.closed {
		s.cond.Wait()
	}
	timer.Stop()
	s.stopped = true
	s.drained = &SinkFrame{T: FrameDrained, FinalSeq: s.lastSeq, Consumed: uint64(s.lastSeq) + uint64(len(s.res))}
	s.cond.Broadcast()
	s.mu.Unlock()
}

// write sends, in order, every unsent record, then the refused total, the
// drained reply and the lapsed frame. A new session resends from the ack.
func (s *remoteSink) write(conn net.Conn, gen uint64) error {
	sent := int64(-1)
	var refusedSent uint64
	drainedSent, lapsedSent := false, false
	for {
		s.mu.Lock()
		if sent < 0 {
			sent = s.acked
		}
		var out []SinkFrame
		for {
			if !s.live || s.closed || s.gen != gen {
				s.mu.Unlock()
				return nil
			}
			for _, r := range s.buf {
				if r.seq > sent {
					out = append(out, SinkFrame{T: FrameRecord, Gen: gen, Seq: r.seq, Body: r.body})
				}
			}
			if s.refused != refusedSent {
				out = append(out, SinkFrame{T: FrameRefused, Gen: gen, Total: s.refused})
			}
			if s.drained != nil && !drainedSent {
				d := *s.drained
				d.Gen = gen
				out = append(out, d)
			}
			if s.lapsed != nil && !lapsedSent {
				l := *s.lapsed
				l.Gen = gen
				out = append(out, l)
			}
			if len(out) > 0 {
				break
			}
			s.cond.Wait()
		}
		refusedTotal := s.refused
		s.mu.Unlock()
		for _, f := range out {
			if err := WriteSinkFrame(conn, f); err != nil {
				return err
			}
			switch f.T {
			case FrameRecord:
				sent = f.Seq
			case FrameRefused:
				refusedSent = refusedTotal
			case FrameDrained:
				drainedSent = true
			case FrameLapsed:
				lapsedSent = true
				s.mu.Lock()
				s.lapsed = nil
				s.cond.Broadcast()
				s.mu.Unlock()
			}
		}
	}
}
