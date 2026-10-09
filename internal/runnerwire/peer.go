// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

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
	// itself: PENDING, STATE, READY, LEASE, GOAWAY and REVOKED.
	OnControl func(Frame)
	// Replay and LastReceived carry a session's sequencing across a reconnect:
	// the buffer the previous connection used (frames still un-ACKed) and the
	// last sequenced frame received from the peer. Zero values start a fresh
	// session at sequence 1.
	Replay       *ReplayBuffer
	LastReceived uint64
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
}

// NewPeer builds a peer; Run drives it.
func NewPeer(conn Conn, cfg PeerConfig) *Peer {
	ctx, cancel := context.WithCancel(context.Background())
	replay := cfg.Replay
	if replay == nil {
		replay = NewReplayBuffer()
	}
	return &Peer{
		conn: conn, cfg: cfg, ctx: ctx, cancel: cancel, recvSeq: cfg.LastReceived,
		replay: replay, connSend: NewConnCredit(), ids: NewIDAllocator(cfg.Org),
		calls: map[uint32]*pendingCall{}, inCalls: map[uint32]context.CancelFunc{},
		streams: map[uint32]*Stream{}, parked: map[uint32]*Stream{}, byCall: map[uint32][]uint32{},
		done: make(chan struct{}),
	}
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
	if f.Type.Sequenced() {
		if err := p.replay.Reserve(ctx, len(f.Payload)); err != nil {
			return err
		}
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if !p.Online() {
		return p.offline()
	}
	if f.Type.Sequenced() {
		f = p.replay.Assign(f)
	}
	if err := p.conn.WriteFrame(ctx, f); err != nil {
		if ctx.Err() == nil {
			p.fail(err)
		}
		return fmt.Errorf("%w: %w", runner.ErrRunnerOffline, err)
	}
	return nil
}

// Run reads frames until the link ends and returns the reason.
func (p *Peer) Run(ctx context.Context) error {
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
			last := p.recvSeq
			p.mu.Unlock()
			go p.send(p.ctx, Frame{Type: TypeAck, Payload: EncodeAck(last)}) //nolint:errcheck // a lost ACK is re-sent on the next replay
			return nil
		}
		if f.Seq != p.recvSeq+1 {
			last := p.recvSeq
			p.mu.Unlock()
			return fmt.Errorf("%w: %s seq %d after %d", ErrBadFrame, f.Type, f.Seq, last)
		}
		p.recvSeq = f.Seq
		p.recvAcks++
		p.recvBytes += len(f.Payload)
		ack := p.recvAcks%ackEvery == 0 || p.recvBytes >= ackBytes
		if ack {
			p.recvBytes = 0
		}
		p.mu.Unlock()
		if ack {
			go p.send(p.ctx, Frame{Type: TypeAck, Payload: EncodeAck(f.Seq)}) //nolint:errcheck // a lost ACK only delays buffer trimming
		}
	}
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
		go p.send(p.ctx, Frame{Type: TypePong, Payload: f.Payload}) //nolint:errcheck // keepalive; a failed pong is the link going down
	case TypePong:
	default:
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
	p.dispatchEvent(ev)
	return nil
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
		if s := p.stream(f.Stream); s != nil {
			if err := s.deliver(f.Payload); errors.Is(err, ErrBadFrame) {
				return err
			} else if err != nil {
				_ = s.Reset(ResetInternal)
			}
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

func (p *Peer) dispatchEvent(ev Event) {
	if ev.Call != 0 {
		p.mu.Lock()
		pc := p.calls[ev.Call]
		p.mu.Unlock()
		if pc != nil && pc.queue(ev) {
			return
		}
	}
	if p.cfg.OnEvent != nil {
		p.cfg.OnEvent(ev)
	}
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

// grant returns consumed bytes to the sender as WINDOW frames: the stream's and the connection's.
func (p *Peer) grant(s *Stream, n int64) {
	_ = p.send(p.ctx, Frame{Type: TypeWindow, Stream: s.id, Payload: EncodeWindow(uint32(n))})
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
	// A stream parked for a call that is not in flight would never be claimed,
	// and the connection holds only MaxStreams: refuse both.
	if (o.Call != 0 && p.calls[o.Call] == nil) || len(p.streams)+len(p.parked) >= MaxStreams {
		p.mu.Unlock()
		go p.send(p.ctx, Frame{Type: TypeReset, Stream: f.Stream, Payload: EncodeReset(ResetRefused)}) //nolint:errcheck // a failed RESET is the link going down
		return nil
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
	go p.send(p.ctx, Frame{Type: TypeOpenOK, Stream: f.Stream}) //nolint:errcheck // a failed OPEN_OK is the link going down
	return nil
}
