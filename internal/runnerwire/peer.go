// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// ackEvery is how many sequenced frames a peer receives between ACKs.
const (
	ackEvery = 8
	ackBytes = 64 << 10
)

// MaxStreams bounds the byte streams a connection holds open, parked ones
// included: an OPEN past it is refused. A constant, not a knob.
const MaxStreams = 256

// maxStreamReplies bounds the RESET and OPEN_OK answers to OPENs that wait
// for the writer. A peer that opens faster than this side can answer fills it,
// and the session ends.
const maxStreamReplies = 2 * MaxStreams

// CallHandler serves one incoming CALL. A ctx cancelled by the caller's RESET
// means the caller stopped waiting. The result is marshalled into the REPLY; an
// error becomes the typed REPLY error through ErrorFor.
type CallHandler func(ctx context.Context, id uint32, method string, args json.RawMessage) (any, error)

// PeerConfig wires one side of an established session.
type PeerConfig struct {
	Org bool
	// OnCall serves incoming calls; nil refuses every call.
	OnCall CallHandler
	// OnEvent receives EVENT frames (org side), in order, on the read loop.
	OnEvent func(Event)
	// OnOpen accepts an incoming byte stream that belongs to no call; an error
	// refuses it. It must not block.
	OnOpen func(s *Stream, o Open) error
	// OnControl receives the session-level frames the peer does not act on
	// itself: PENDING, STATE, READY, LEASE, GOAWAY, REVOKED and CURRENT_ACTIVITY.
	OnControl func(Frame)
	// Checked handlers replace their unchecked counterpart when set. A failed
	// durable ingest must not advance the received/ACK cursor.
	OnEventChecked   func(Event) error
	OnControlChecked func(Frame) error
	// Replay and LastReceived carry a session's sequencing across a reconnect:
	// the buffer the previous connection used (frames still un-ACKed) and the
	// last sequenced frame received from the peer. Zero values start a fresh
	// session at sequence 1.
	Replay       *ReplayBuffer
	LastReceived uint64
	// NextID and AbandonedCalls retain the call-id space after a link drop.
	// Callers fail offline; their late, replayed replies are still legitimate.
	NextID         uint32
	AbandonedCalls []uint32
}

// Peer multiplexes calls, events and byte streams over one established Conn
// (the handshake has already run). It assigns sequence numbers, keeps the
// replay buffer and answers PING; resume across connections is the session's.
type Peer struct {
	conn Conn
	cfg  PeerConfig

	ctx    context.Context
	cancel context.CancelFunc

	wmu    sync.Mutex
	replay *ReplayBuffer

	connSend *Credit

	mu        sync.Mutex
	ids       *IDAllocator
	calls     map[uint32]*pendingCall
	inCalls   map[uint32]context.CancelFunc
	streams   map[uint32]*Stream
	parked    map[uint32]*Stream
	byCall    map[uint32][]uint32
	recvSeq   uint64
	recvAcks  int
	recvBytes int
	connPend  int64
	held      atomic.Int64 // received bytes not yet read by a stream's reader
	err       error
	done      chan struct{}
	stopped   chan struct{}
	lastPong  atomic.Pointer[time.Time]
	ackWake   chan struct{}
	// writing is when the write in progress began, nil between writes.
	writing atomic.Pointer[time.Time]
	// pongDue (the latest PING payload) and owed (dropped connection credit)
	// wait under mu for the one worker that answers them.
	pongDue   []byte
	owed      int64
	answers   []Frame // under mu: RESET and OPEN_OK for OPENs, in order
	replyWake chan struct{}
}

// NewPeer builds a peer; Run drives it.
func NewPeer(conn Conn, cfg PeerConfig) *Peer {
	ctx, cancel := context.WithCancel(context.Background())
	replay := cfg.Replay
	if replay == nil {
		replay = NewReplayBuffer()
	}
	p := &Peer{
		conn: conn, cfg: cfg, ctx: ctx, cancel: cancel, recvSeq: cfg.LastReceived,
		replay: replay, connSend: NewConnCredit(), ids: NewIDAllocator(cfg.Org),
		calls: map[uint32]*pendingCall{}, inCalls: map[uint32]context.CancelFunc{},
		streams: map[uint32]*Stream{}, parked: map[uint32]*Stream{}, byCall: map[uint32][]uint32{},
		done: make(chan struct{}), stopped: make(chan struct{}),
		ackWake: make(chan struct{}, 1), replyWake: make(chan struct{}, 1),
	}
	if cfg.NextID != 0 {
		p.ids.next = cfg.NextID
	}
	for _, id := range cfg.AbandonedCalls {
		p.calls[id] = &pendingCall{abandoned: true, reply: make(chan Reply, 1)}
	}
	return p
}

// StreamCount is the byte streams the connection holds, parked ones included.
func (p *Peer) StreamCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.streams) + len(p.parked)
}

// Done closes when the link is down; Err says why.
func (p *Peer) Done() <-chan struct{} { return p.done }

func (p *Peer) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// Online is true until the link is down.
func (p *Peer) Online() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// LastReceivedSeq is the highest sequenced frame received, for a resume request.
func (p *Peer) LastReceivedSeq() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.recvSeq
}

// Close ends the session.
func (p *Peer) Close() { p.fail(ErrLinkDown) }

func (p *Peer) offline() error {
	return fmt.Errorf("%w: %w", runner.ErrRunnerOffline, p.Err())
}

func (p *Peer) fail(err error) {
	p.mu.Lock()
	if p.err != nil {
		p.mu.Unlock()
		return
	}
	p.err = err
	streams := make([]*Stream, 0, len(p.streams)+len(p.parked))
	for _, s := range p.streams {
		streams = append(streams, s)
	}
	for _, s := range p.parked {
		streams = append(streams, s)
	}
	cancels := make([]context.CancelFunc, 0, len(p.inCalls))
	for _, c := range p.inCalls {
		cancels = append(cancels, c)
	}
	p.streams, p.parked, p.byCall = map[uint32]*Stream{}, map[uint32]*Stream{}, map[uint32][]uint32{}
	p.mu.Unlock()
	close(p.done)
	p.cancel()
	p.connSend.Close()
	for _, s := range streams {
		s.abort(ResetReconnect)
	}
	for _, c := range cancels {
		c()
	}
	_ = p.conn.Close()
}

// send writes one frame; a sequenced one is numbered and buffered under the
// write lock, so sequence order is wire order.
func (p *Peer) send(ctx context.Context, f Frame) error {
	_, err := p.sendFrame(ctx, f)
	return err
}

func (p *Peer) sendFrame(ctx context.Context, f Frame) (uint64, error) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.ctx, cancel)
	defer func() { stop(); cancel() }()
	if f.Type.Sequenced() {
		if err := p.replay.Reserve(ctx, len(f.Payload)); err != nil {
			return 0, err
		}
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if !p.Online() {
		if f.Type.Sequenced() {
			p.replay.unreserve(len(f.Payload))
		}
		return 0, p.offline()
	}
	if f.Type.Sequenced() {
		f = p.replay.Assign(f)
	}
	// The caller's ctx bounds only the wait for replay room. The frame is
	// written under the link's ctx: a write whose ctx ends closes the shared
	// connection, so a caller giving up would take every stream and call down.
	if err := p.write(p.ctx, f); err != nil {
		// A cancelled write can have sent any prefix of the frame. Resume is
		// required before another sequence is safe to send on this link.
		p.fail(err)
		return f.Seq, fmt.Errorf("%w: %w", runner.ErrRunnerOffline, err)
	}
	return f.Seq, nil
}

// write puts one frame on the socket; the caller holds the write lock. The
// start time lets keepalive fail a link whose write has stopped moving.
func (p *Peer) write(ctx context.Context, f Frame) error {
	start := time.Now()
	p.writing.Store(&start)
	defer p.writing.Store(nil)
	return p.conn.WriteFrame(ctx, f)
}

// Run reads frames until the link ends and returns the reason.
func (p *Peer) Run(ctx context.Context) error {
	defer close(p.stopped)
	go p.sendAcks()
	go p.sendReplies()
	go func() {
		select {
		case <-ctx.Done():
			p.fail(ctx.Err())
		case <-p.done:
		}
	}()
	for {
		f, err := p.conn.ReadFrame(p.ctx)
		if err != nil {
			p.fail(err)
			return p.Err()
		}
		if err := p.handle(f); err != nil {
			p.fail(err)
			return p.Err()
		}
	}
}

func (p *Peer) handle(f Frame) error {
	if !f.Type.AllowedFrom(!p.cfg.Org) {
		return fmt.Errorf("%w: %s not allowed from the peer", ErrBadFrame, f.Type)
	}
	if f.Type.Sequenced() {
		p.mu.Lock()
		if f.Seq <= p.recvSeq {
			// A replay after a lost ACK: the frame already ran. Drop it and say
			// where we are, so the sender trims its buffer.
			p.mu.Unlock()
			p.wakeAck()
			return nil
		}
		if f.Seq != p.recvSeq+1 {
			last := p.recvSeq
			p.mu.Unlock()
			return fmt.Errorf("%w: %s seq %d after %d", ErrBadFrame, f.Type, f.Seq, last)
		}
		p.mu.Unlock()
	}
	if err := p.handleFrame(f); err != nil {
		return err
	}
	if f.Type.Sequenced() {
		p.acceptSequence(f)
	}
	return nil
}

func (p *Peer) acceptSequence(f Frame) {
	p.mu.Lock()
	p.recvSeq = f.Seq
	p.recvAcks++
	p.recvBytes += len(f.Payload)
	ack := p.recvAcks%ackEvery == 0 || p.recvBytes >= ackBytes || (f.Type == TypeEvent && p.cfg.OnEventChecked != nil)
	if ack {
		p.recvBytes = 0
	}
	p.mu.Unlock()
	if ack {
		p.wakeAck()
	}
}

func (p *Peer) handleFrame(f Frame) error {
	switch f.Type {
	case TypeCall:
		return p.onCall(f)
	case TypeReply:
		return p.onReply(f)
	case TypeEvent:
		return p.onEvent(f)
	case TypeOpen:
		return p.onOpen(f)
	case TypeOpenOK, TypeData, TypeWindow, TypeClose, TypeReset:
		return p.handleStream(f)
	case TypeAck:
		seq, err := DecodeAck(f.Payload)
		if err != nil {
			return err
		}
		if err := p.replay.Ack(seq); err != nil {
			return fmt.Errorf("%w: %v", ErrBadFrame, err)
		}
	case TypePing:
		if _, err := DecodePing(f.Payload); err != nil {
			return err
		}
		p.mu.Lock()
		p.pongDue = f.Payload
		p.mu.Unlock()
		p.wakeReplies()
	case TypePong:
		if _, err := DecodePing(f.Payload); err != nil {
			return err
		}
		now := time.Now()
		p.lastPong.Store(&now)
	default:
		if p.cfg.OnControlChecked != nil {
			return p.cfg.OnControlChecked(f)
		}
		if p.cfg.OnControl != nil {
			p.cfg.OnControl(f)
		}
	}
	return nil
}

func (p *Peer) onEvent(f Frame) error {
	var ev Event
	if err := Unmarshal(f.Payload, &ev); err != nil {
		return err
	}
	if err := ev.Validate(); err != nil {
		return err
	}
	return p.dispatchEvent(ev)
}

// handleStream serves the frames that act on one byte stream.
func (p *Peer) handleStream(f Frame) error {
	switch f.Type {
	case TypeOpenOK:
		if s := p.stream(f.Stream); s != nil {
			select {
			case <-s.opened:
			default:
				close(s.opened)
			}
		}
	case TypeData:
		s := p.stream(f.Stream)
		if s == nil {
			// Sent before the peer saw this stream end: dropped, so the
			// bytes are the sender's again.
			p.returnCredit(int64(len(f.Payload)))
			return nil
		}
		if err := s.deliver(f.Payload); errors.Is(err, ErrBadFrame) {
			return err
		} else if err != nil {
			p.returnCredit(int64(len(f.Payload))) // refused, not held
			_ = s.Reset(ResetInternal)
		}
	case TypeWindow:
		n, err := DecodeWindow(f.Payload)
		if err != nil {
			return err
		}
		if f.Stream == 0 {
			p.connSend.Add(int64(n))
		} else if s := p.stream(f.Stream); s != nil {
			s.send.Add(int64(n))
		}
	case TypeClose:
		if s := p.stream(f.Stream); s != nil && s.remoteClose() {
			p.forget(f.Stream)
		}
	case TypeReset:
		code, err := DecodeReset(f.Payload)
		if err != nil {
			return err
		}
		p.onReset(f.Stream, code)
	}
	return nil
}

func (p *Peer) stream(id uint32) *Stream {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.streams[id]; ok {
		return s
	}
	return p.parked[id]
}

func (p *Peer) forget(id uint32) {
	p.mu.Lock()
	delete(p.streams, id)
	delete(p.parked, id)
	p.mu.Unlock()
}

func (p *Peer) dispatchEvent(ev Event) error {
	if p.cfg.OnEventChecked != nil {
		if err := p.cfg.OnEventChecked(ev); err != nil {
			return err
		}
	}
	if ev.Call != 0 {
		p.mu.Lock()
		pc := p.calls[ev.Call]
		p.mu.Unlock()
		if pc != nil {
			queued, err := pc.queue(ev)
			if err != nil || queued {
				return err
			}
		}
	}
	if p.cfg.OnEventChecked == nil && p.cfg.OnEvent != nil {
		p.cfg.OnEvent(ev)
	}
	return nil
}

func (p *Peer) onReset(id, code uint32) {
	p.mu.Lock()
	cancel := p.inCalls[id]
	s := p.streams[id]
	if s == nil {
		s = p.parked[id]
	}
	delete(p.streams, id)
	delete(p.parked, id)
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if s != nil {
		s.abort(code)
	}
}

func (p *Peer) wakeReplies() {
	select {
	case p.replyWake <- struct{}{}:
	default:
	}
}

// One worker answers OPENs (queued, bounded), PINGs and dropped credit (each
// coalesced: the latest PING, the sum of credit), so a peer that sends faster
// than this side can write cannot create a goroutine per frame.
func (p *Peer) sendReplies() {
	for {
		select {
		case <-p.replyWake:
		case <-p.done:
			return
		}
		p.mu.Lock()
		answers, pong, owed := p.answers, p.pongDue, p.owed
		p.answers, p.pongDue, p.owed = nil, nil, 0
		p.mu.Unlock()
		for _, f := range answers {
			if p.send(p.ctx, f) != nil {
				return
			}
		}
		if pong != nil && p.send(p.ctx, Frame{Type: TypePong, Payload: pong}) != nil {
			return
		}
		if owed > 0 && p.send(p.ctx, Frame{Type: TypeWindow, Payload: EncodeWindow(uint32(min(owed, math.MaxUint32)))}) != nil {
			return
		}
	}
}

// grant returns consumed bytes to the sender's stream window.
func (p *Peer) grant(s *Stream, n int64) {
	_ = p.send(p.ctx, Frame{Type: TypeWindow, Stream: s.id, Payload: EncodeWindow(uint32(n))})
}

// consumed returns bytes a stream's reader took to the sender's connection
// window in ConnWindow/4 batches. It counts every read on every stream, so
// however many streams are live, at most ConnWindow/4-1 read bytes are withheld.
func (p *Peer) consumed(n int64) {
	p.mu.Lock()
	p.connPend += n
	c := int64(0)
	if p.connPend >= ConnWindow/4 {
		c, p.connPend = p.connPend, 0
	}
	p.mu.Unlock()
	if c > 0 {
		_ = p.send(p.ctx, Frame{Type: TypeWindow, Payload: EncodeWindow(uint32(c))})
	}
}

// Event sends one EVENT (runner side).
func (p *Peer) Event(ctx context.Context, ev Event) error {
	b, err := Marshal(ev)
	if err != nil {
		return err
	}
	return p.send(ctx, Frame{Type: TypeEvent, Payload: b})
}

// Send writes a session-level frame (PENDING, STATE, READY, LEASE, GOAWAY).
func (p *Peer) Send(ctx context.Context, f Frame) error { return p.send(ctx, f) }

// ErrNoStream is TakeStream's answer for an id no call opened.
var ErrNoStream = errors.New("runnerwire: no such stream")

// TakeStream claims a stream the runner opened for an in-flight call, named by
// that call's reply. A stream whose call fails is reset, not parked.
func (p *Peer) TakeStream(id uint32) (*Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.parked[id]
	if !ok {
		return nil, ErrNoStream
	}
	delete(p.parked, id)
	p.streams[id] = s
	return s, nil
}

// Open starts a byte stream to the peer and waits for it to be accepted.
func (p *Peer) Open(ctx context.Context, o Open) (*Stream, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.err != nil {
		p.mu.Unlock()
		return nil, p.offline()
	}
	id := p.ids.Next()
	s := p.newStream(id, o.Kind)
	p.streams[id] = s
	p.mu.Unlock()
	b, err := Marshal(o)
	if err != nil {
		return nil, err
	}
	if err := p.send(ctx, Frame{Type: TypeOpen, Stream: id, Payload: b}); err != nil {
		p.forget(id)
		return nil, err
	}
	select {
	case <-s.opened:
		return s, nil
	case <-s.ctx.Done():
		return nil, s.writeErr(ErrLinkDown)
	case <-ctx.Done():
		_ = s.Reset(ResetCancelled)
		return nil, ctx.Err()
	}
}

func (p *Peer) onOpen(f Frame) error {
	var o Open
	if err := Unmarshal(f.Payload, &o); err != nil {
		return err
	}
	if err := o.Validate(); err != nil {
		return err
	}
	if org, _ := InitiatedByOrg(f.Stream); org == p.cfg.Org {
		return fmt.Errorf("%w: OPEN on stream %d from the wrong side", ErrBadFrame, f.Stream)
	}
	p.mu.Lock()
	if p.streams[f.Stream] != nil || p.parked[f.Stream] != nil {
		p.mu.Unlock()
		return fmt.Errorf("%w: stream %d already open", ErrBadFrame, f.Stream)
	}
	// A missing call is a permanent refusal; capacity can clear while this
	// authenticated connection remains live, so output may retry it.
	if o.Call != 0 && p.calls[o.Call] == nil {
		p.mu.Unlock()
		return p.answer(Frame{Type: TypeReset, Stream: f.Stream, Payload: EncodeReset(ResetRefused)})
	}
	if len(p.streams)+len(p.parked) >= MaxStreams {
		p.mu.Unlock()
		return p.answer(Frame{Type: TypeReset, Stream: f.Stream, Payload: EncodeReset(ResetCapacity)})
	}
	s := p.newStream(f.Stream, o.Kind)
	if o.Call != 0 {
		p.parked[f.Stream] = s
		p.byCall[o.Call] = append(p.byCall[o.Call], f.Stream)
	} else {
		p.streams[f.Stream] = s
	}
	p.mu.Unlock()
	if o.Call == 0 {
		if p.cfg.OnOpen == nil {
			_ = s.Reset(ResetRefused)
			return nil
		}
		if err := p.cfg.OnOpen(s, o); err != nil {
			_ = s.Reset(ResetRefused)
			return nil
		}
	}
	return p.answer(Frame{Type: TypeOpenOK, Stream: f.Stream})
}

// answer queues a reply to an OPEN for the reply worker. The read loop must
// not wait on a write: two peers that both did could stop each other reading.
func (p *Peer) answer(f Frame) error {
	p.mu.Lock()
	if len(p.answers) >= maxStreamReplies {
		p.mu.Unlock()
		return fmt.Errorf("%w: %d OPENs await an answer", ErrBadFrame, maxStreamReplies)
	}
	p.answers = append(p.answers, f)
	p.mu.Unlock()
	p.wakeReplies()
	return nil
}
