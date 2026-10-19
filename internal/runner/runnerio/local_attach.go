// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

// Local attach: the person's CLI on the runner's own computer reaches a run's terminal over the
// runner's per-user socket (the listener is cmd/wardyn-runnerd's). The socket grants nothing by
// itself: ServeLocal hands its connection a nonce, and a terminal opens only when the organisation
// sends local_attach naming that nonce over the runner's authenticated stream, after the org has
// redeemed the person's attach ticket (design ATTACH §3.5.3).

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// Local socket frames are one type byte, a big-endian uint32 payload length, then the payload: JSON
// for control frames, raw terminal bytes for LocalData.
const (
	LocalHello   byte = 'h' // CLI, first frame: LocalHelloMsg
	LocalHelloOK byte = 'o' // runnerd: LocalHelloOKMsg
	LocalRefuse  byte = 'x' // runnerd, before LocalBound: LocalReasonMsg, then the connection closes
	LocalBound   byte = 'b' // runnerd: LocalBoundMsg, the organisation's grant arrived
	LocalData    byte = 'd' // either way: terminal bytes
	LocalResize  byte = 'r' // CLI: LocalSizeMsg
	LocalMode    byte = 'm' // runnerd: LocalModeMsg, the session became an observer or a writer
	LocalEnd     byte = 'e' // runnerd, after LocalBound: LocalReasonMsg, then the connection closes
)

// LocalProtocol is the version LocalHelloMsg carries; MaxLocalFrame bounds one frame's payload.
const (
	LocalProtocol = 1
	MaxLocalFrame = 64 << 10
)

// The reasons LocalRefuse and LocalEnd carry.
const (
	LocalReasonProtocol   = "protocol"
	LocalReasonNotHere    = "run_not_on_this_runner"
	LocalReasonBusy       = "too_many_pending"
	LocalReasonExpired    = "nonce_expired"
	LocalReasonOrgEnded   = "ended_by_organisation"
	LocalReasonLinkLost   = "runner_link_lost"
	LocalReasonTeeStalled = "attach_tee_stalled"
	LocalReasonModeFailed = "mode_not_applied"
	LocalReasonRunEnded   = "run_ended"
	LocalReasonExited     = "shell_exited"
	LocalReasonAttach     = "attach_failed"
	LocalReasonClosed     = "client_closed"
	// LocalReasonUnavailable: the org granted another run than the one this connection named; the
	// CLI goes through the organisation instead.
	LocalReasonUnavailable = "attach_local_unavailable"
)

type (
	LocalHelloMsg struct {
		V     int       `json:"v"`
		RunID uuid.UUID `json:"run_id"`
	}
	LocalHelloOKMsg struct {
		RunnerID string `json:"runner_id"`
		Nonce    string `json:"nonce"`
	}
	LocalBoundMsg struct {
		ReadOnly bool   `json:"read_only"`
		Cols     uint16 `json:"cols"`
		Rows     uint16 `json:"rows"`
	}
	LocalSizeMsg struct {
		Cols uint16 `json:"cols"`
		Rows uint16 `json:"rows"`
	}
	LocalModeMsg struct {
		ReadOnly bool `json:"read_only"`
	}
	LocalReasonMsg struct {
		Reason string `json:"reason"`
	}
)

// WriteLocalFrame writes one frame.
func WriteLocalFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > MaxLocalFrame {
		return fmt.Errorf("runnerio: local frame of %d bytes is over %d", len(payload), MaxLocalFrame)
	}
	frame := make([]byte, 5, 5+len(payload))
	frame[0] = typ
	binary.BigEndian.PutUint32(frame[1:], uint32(len(payload)))
	_, err := w.Write(append(frame, payload...))
	return err
}

// WriteLocalJSON writes one control frame.
func WriteLocalJSON(w io.Writer, typ byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteLocalFrame(w, typ, b)
}

// ReadLocalFrame reads one frame, refusing a payload over MaxLocalFrame.
func ReadLocalFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxLocalFrame {
		return 0, nil, fmt.Errorf("runnerio: local frame of %d bytes is over %d", n, MaxLocalFrame)
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return hdr[0], b, err
}

// localTimes are a Server's local attach bounds; tests shorten them.
type localTimes struct {
	nonceTTL   time.Duration // how long a nonce waits for the org's grant
	teeWindow  int64         // bytes shown past the org's last acknowledgement
	teeTimeout time.Duration // how long the oldest unacknowledged byte may wait for one
}

var defaultLocalTimes = localTimes{30 * time.Second, runnerwire.LocalAttachTeeWindowBytes, runnerwire.LocalAttachTeeWindowTimeout}

// localMaxPending bounds the nonces handed out and not yet granted or expired.
const localMaxPending = 8

type localFrame struct {
	typ byte
	b   []byte
}

// localClient is one socket connection. Its nonce maps to it alone, so a grant reaches only the
// connection that asked.
type localClient struct {
	conn  net.Conn
	ref   string            // the local sandbox ref the hello resolved
	in    chan localFrame   // frames the CLI sent after LocalHelloOK
	gone  chan struct{}     // closed when the CLI stops sending
	taken chan struct{}     // closed once local_attach claimed the nonce
	quit  chan struct{}     // closed by close
	wmu   sync.Mutex        // one frame on conn at a time
	once  sync.Once
	bound bool // guarded by wmu: LocalBound was sent, so the last frame is LocalEnd
}

func (c *localClient) write(typ byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if typ == LocalBound {
		c.bound = true
	}
	return WriteLocalFrame(c.conn, typ, payload)
}

func (c *localClient) writeJSON(typ byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.write(typ, b)
}

// close says why, best effort, and closes the connection. A CLI that stopped reading delays it by
// one second at most.
func (c *localClient) close(reason string) {
	c.once.Do(func() {
		close(c.quit)
		_ = c.conn.SetWriteDeadline(time.Now().Add(time.Second))
		c.wmu.Lock()
		typ := LocalRefuse
		if c.bound {
			typ = LocalEnd
		}
		b, _ := json.Marshal(LocalReasonMsg{Reason: reason})
		_ = WriteLocalFrame(c.conn, typ, b)
		c.wmu.Unlock()
		_ = c.conn.Close()
	})
}

// ServeLocal speaks the local socket protocol on one connection the listener has already proved is
// this user's. It returns once the connection is refused or expires, or a session owns it.
func (s *Server) ServeLocal(conn net.Conn) {
	c := &localClient{conn: conn, in: make(chan localFrame, 16), gone: make(chan struct{}), taken: make(chan struct{}), quit: make(chan struct{})}
	nonce, reason := s.helloLocal(c)
	if nonce == "" {
		c.close(reason)
		return
	}
	go func() {
		defer close(c.gone)
		defer close(c.in)
		for {
			typ, b, err := ReadLocalFrame(conn)
			if err != nil {
				return
			}
			select {
			case c.in <- localFrame{typ, b}:
			case <-c.quit:
				return
			}
		}
	}()
	if err := c.writeJSON(LocalHelloOK, LocalHelloOKMsg{RunnerID: s.runnerID, Nonce: nonce}); err != nil {
		reason = LocalReasonClosed
	} else {
		timer := time.NewTimer(s.times.nonceTTL)
		defer timer.Stop()
		select {
		case <-c.taken:
			return
		case <-timer.C:
			reason = LocalReasonExpired
		case <-c.gone:
			reason = LocalReasonClosed
		}
	}
	s.mu.Lock()
	mine := s.nonces[nonce] == c
	delete(s.nonces, nonce)
	s.mu.Unlock()
	if mine { // else local_attach claimed it meanwhile and owns the connection
		c.close(reason)
	}
}

// helloLocal reads the hello and hands out a nonce for a live run whose sandbox this runner holds,
// resolved through the runner's own run record (AttachRuns). "" refuses with the reason.
func (s *Server) helloLocal(c *localClient) (string, string) {
	_ = c.conn.SetReadDeadline(time.Now().Add(s.times.nonceTTL))
	typ, b, err := ReadLocalFrame(c.conn)
	_ = c.conn.SetReadDeadline(time.Time{})
	var hello LocalHelloMsg
	if err != nil || typ != LocalHello || json.Unmarshal(b, &hello) != nil || hello.V != LocalProtocol || hello.RunID == uuid.Nil {
		return "", LocalReasonProtocol
	}
	s.mu.Lock()
	resolve := s.attachRef
	s.mu.Unlock()
	if resolve != nil {
		c.ref = resolve(hello.RunID)
	}
	if c.ref == "" || s.held(c.ref) != nil {
		return "", LocalReasonNotHere
	}
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	nonce := base64.RawURLEncoding.EncodeToString(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.nonces) >= localMaxPending {
		return "", LocalReasonBusy
	}
	s.nonces[nonce] = c
	return nonce, ""
}

// localAttach is MethodLocalAttach: the organisation's grant for the connection holding args.Nonce.
// The nonce is spent by this call whatever it answers.
func (s *Server) localAttach(ctx context.Context, peer *runnerwire.Peer, id uint32, raw json.RawMessage) (any, error) {
	args, err := decode[runnerwire.LocalAttachArgs](raw)
	if err != nil {
		return nil, err
	}
	ref, err := s.local(args.Ref)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	c := s.nonces[args.Nonce]
	delete(s.nonces, args.Nonce)
	s.mu.Unlock()
	if c == nil {
		return nil, runnerwire.Refuse("unknown or expired local attach nonce")
	}
	close(c.taken)
	if c.ref != ref { // the run named on the socket must be the run the org's ticket is for
		c.close(LocalReasonUnavailable)
		return nil, runnerwire.Refuse("attach_local_unavailable: the run on the local socket is not the run the organisation granted")
	}
	if err := s.held(ref); err != nil {
		c.close(LocalReasonNotHere)
		return nil, err
	}
	life, cancel := context.WithCancel(s.life(peer))
	sess, err := s.sub.Attach(life, ref, args.Options)
	if err != nil {
		cancel()
		c.close(LocalReasonAttach)
		return nil, err
	}
	st, err := peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindPTYTee, Target: ref, Call: id})
	if err != nil {
		cancel()
		_ = sess.Close()
		c.close(LocalReasonAttach)
		return nil, err
	}
	l := &localSession{s: s, key: streamKey{peer, st.ID()}, ref: ref, c: c, tee: st, teeOn: args.Tee, life: life, cancel: cancel,
		done: make(chan struct{}), sess: sess, opened: args.Options, observer: args.Options.Observer, ackCh: make(chan struct{})}
	s.mu.Lock()
	if life.Err() != nil {
		s.mu.Unlock()
		l.end(LocalReasonLinkLost)
		return nil, runner.ErrRunnerOffline
	}
	s.locals[l.key] = l
	s.mu.Unlock()
	if err := c.writeJSON(LocalBound, LocalBoundMsg{ReadOnly: l.observer, Cols: args.Options.Cols, Rows: args.Options.Rows}); err != nil {
		l.end(LocalReasonClosed)
		return nil, runnerwire.Refuse("the local attach client went away")
	}
	go l.output()
	go l.input()
	go l.watchTee()
	if l.teeOn {
		go l.watchStall()
	}
	return runnerwire.StreamResult{Stream: st.ID()}, nil
}

// teeMark is the tee offset a chunk ends at and when it was handed to the tee.
type teeMark struct {
	end int64
	at  time.Time
}

// localSession is one granted local attach. With teeOn every output chunk goes to the org's tee
// before the CLI, and the CLI is shown at most teeWindow bytes past the org's last
// acknowledgement.
type localSession struct {
	s      *Server
	key    streamKey
	ref    string
	c      *localClient
	tee    *runnerwire.Stream
	teeOn  bool
	life   context.Context
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once

	imu      sync.Mutex // held across each input write, so a mode change waits for one in flight
	observer bool       // guarded by imu: input and resizes are dropped

	mu      sync.Mutex
	sess    runner.Session
	opened  runner.AttachOptions // the options sess was opened with
	sent    int64                // tee bytes handed to the org
	acked   int64                // the org's last acknowledgement
	shown   int64                // bytes written to the CLI
	unacked []teeMark            // chunks not wholly acknowledged, oldest first
	ackCh   chan struct{}        // closed and replaced on each acknowledgement that advances
}

func (l *localSession) current() runner.Session {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sess
}

func (l *localSession) output() {
	buf := make([]byte, 32<<10)
	for {
		sess := l.current()
		n, err := sess.Read(buf)
		if n > 0 {
			if reason := l.relay(buf[:n]); reason != "" {
				l.end(reason)
				return
			}
		}
		if err != nil {
			if l.current() != sess {
				continue // a promotion replaced the exec
			}
			l.end(LocalReasonExited)
			return
		}
	}
}

// relay tees b, then shows it within the window. It answers why the session ends, or "".
func (l *localSession) relay(b []byte) string {
	if l.teeOn {
		l.mu.Lock()
		l.sent += int64(len(b))
		l.unacked = append(l.unacked, teeMark{end: l.sent, at: time.Now()})
		l.mu.Unlock()
		if _, err := l.tee.Write(b); err != nil {
			return LocalReasonOrgEnded
		}
	}
	for len(b) > 0 {
		k := int64(len(b))
		l.mu.Lock()
		if l.teeOn {
			k = min(k, l.acked+l.s.times.teeWindow-l.shown)
		}
		ch := l.ackCh
		l.mu.Unlock()
		if k <= 0 { // the window is full: nothing more reaches the CLI until the org acknowledges
			select {
			case <-ch:
				continue
			case <-l.done:
				return LocalReasonClosed
			}
		}
		if err := l.c.write(LocalData, b[:k]); err != nil {
			return LocalReasonClosed
		}
		l.mu.Lock()
		l.shown += k
		l.mu.Unlock()
		b = b[k:]
	}
	return ""
}

// watchStall ends the session when the oldest unacknowledged tee byte was sent more than
// teeTimeout ago. A session with nothing unacknowledged never stalls.
func (l *localSession) watchStall() {
	t := time.NewTicker(l.s.times.teeTimeout / 8)
	defer t.Stop()
	for {
		select {
		case <-l.done:
			return
		case now := <-t.C:
			l.mu.Lock()
			stalled := len(l.unacked) > 0 && now.Sub(l.unacked[0].at) > l.s.times.teeTimeout
			l.mu.Unlock()
			if stalled {
				l.end(LocalReasonTeeStalled)
				return
			}
		}
	}
}

// ack is MethodLocalAttachTeeAck. Acknowledgements are cumulative: one at or below the last is a
// no-op, one past the bytes sent is refused.
func (l *localSession) ack(offset int64) error {
	if !l.teeOn {
		return runnerwire.Refuse("this local attach session has no tee")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if offset > l.sent {
		return runnerwire.Refuse("tee acknowledgement %d is past the %d bytes sent", offset, l.sent)
	}
	if offset <= l.acked {
		return nil
	}
	l.acked = offset
	i := 0
	for i < len(l.unacked) && l.unacked[i].end <= offset {
		i++
	}
	l.unacked = l.unacked[i:]
	close(l.ackCh)
	l.ackCh = make(chan struct{})
	return nil
}

func (l *localSession) input() {
	for f := range l.c.in {
		l.imu.Lock()
		if !l.observer {
			sess := l.current()
			switch f.typ {
			case LocalData:
				_, _ = sess.Write(f.b)
			case LocalResize:
				var size LocalSizeMsg
				if json.Unmarshal(f.b, &size) == nil {
					_ = sess.Resize(l.life, size.Cols, size.Rows)
				}
			}
		}
		l.imu.Unlock()
		if f.typ != LocalData && f.typ != LocalResize {
			l.end(LocalReasonProtocol)
			return
		}
	}
	l.end(LocalReasonClosed)
}

// mode is MethodLocalAttachMode: applied before it is acknowledged; one that cannot be applied ends
// the session. Promoting a session opened as an observer opens a writer's exec in its place.
func (l *localSession) mode(observer bool, cols, rows uint16) error {
	l.imu.Lock()
	defer l.imu.Unlock()
	l.mu.Lock()
	reopen := !observer && l.opened.Observer
	l.mu.Unlock()
	if reopen {
		opts := runner.AttachOptions{Cols: cols, Rows: rows}
		sess, err := l.s.sub.Attach(l.life, l.ref, opts)
		if err != nil {
			l.end(LocalReasonModeFailed)
			return err
		}
		l.mu.Lock()
		select {
		case <-l.done:
			l.mu.Unlock()
			_ = sess.Close()
			return runnerwire.Refuse("the local attach session ended")
		default:
		}
		old := l.sess
		l.sess, l.opened = sess, opts
		l.mu.Unlock()
		_ = old.Close()
	}
	l.observer = observer
	if err := l.c.writeJSON(LocalMode, LocalModeMsg{ReadOnly: observer}); err != nil {
		l.end(LocalReasonClosed)
		return runnerwire.Refuse("the local attach client went away")
	}
	return nil
}

// watchTee ends the session when the org resets or closes the tee, or the link carrying it drops.
func (l *localSession) watchTee() {
	_, _ = io.Copy(io.Discard, l.tee) // the org never writes the tee
	reason := LocalReasonOrgEnded
	select {
	case <-l.key.peer.Done():
		reason = LocalReasonLinkLost
	default:
	}
	l.end(reason)
}

func (l *localSession) end(reason string) {
	l.once.Do(func() {
		close(l.done)
		l.cancel()
		l.s.mu.Lock()
		if l.s.locals[l.key] == l {
			delete(l.s.locals, l.key)
		}
		l.s.mu.Unlock()
		_ = l.current().Close()
		_ = l.tee.Close()
		l.c.close(reason)
	})
}

func (s *Server) localSession(peer *runnerwire.Peer, stream uint32) (*localSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l := s.locals[streamKey{peer, stream}]; l != nil {
		return l, nil
	}
	return nil, runner.ErrSandboxGone
}

func (s *Server) localMode(peer *runnerwire.Peer, raw json.RawMessage) error {
	args, err := decode[runnerwire.LocalAttachModeArgs](raw)
	if err != nil {
		return err
	}
	l, err := s.localSession(peer, args.Stream)
	if err != nil {
		return err
	}
	return l.mode(args.Observer, args.Cols, args.Rows)
}

func (s *Server) localTeeAck(peer *runnerwire.Peer, raw json.RawMessage) error {
	args, err := decode[runnerwire.LocalAttachTeeAckArgs](raw)
	if err != nil {
		return err
	}
	l, err := s.localSession(peer, args.Stream)
	if err != nil {
		return err
	}
	return l.ack(args.Offset)
}

// EndLocalAttach ends every local attach session on the local sandbox ref; runnerd calls it before
// a kill or end of that sandbox.
func (s *Server) EndLocalAttach(ref string) {
	s.mu.Lock()
	var ends []*localSession
	for _, l := range s.locals {
		if l.ref == ref {
			ends = append(ends, l)
		}
	}
	s.mu.Unlock()
	for _, l := range ends {
		l.end(LocalReasonRunEnded)
	}
}
