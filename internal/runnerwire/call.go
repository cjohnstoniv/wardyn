// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// MaxCalls bounds concurrent RPC handlers and retained reply tombstones. Byte
// streams have their separate MaxStreams bound. An authenticated peer still
// must not allocate an unbounded number of goroutines or callback events.
const MaxCalls = 256

type pendingCall struct {
	method    string // the call's method, which a runner-opened stream's kind may depend on
	abandoned bool   // the caller stopped waiting; guarded by Peer.mu
	reply     chan Reply
	notify    chan struct{}
	onEvent   func(Event)

	mu         sync.Mutex
	stopped    bool // the caller returned: nobody drains events any more
	events     []Event
	eventBytes int
}

// queue holds an event for the caller's goroutine; false when the call takes none. An event for a
// caller that already returned is dropped (reported taken), never queued for nobody.
func (c *pendingCall) queue(ev Event) (bool, error) {
	if c.onEvent == nil {
		return false, nil
	}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return true, nil
	}
	if c.eventBytes+len(ev.Data)+64 > MaxControlFrame {
		c.mu.Unlock()
		return false, fmt.Errorf("%w: call event queue exceeds its bound", ErrBadFrame)
	}
	c.events = append(c.events, ev)
	c.eventBytes += len(ev.Data) + 64
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
	return true, nil
}

func (c *pendingCall) drain() {
	c.mu.Lock()
	evs := c.events
	c.events = nil
	c.eventBytes = 0
	c.mu.Unlock()
	for _, ev := range evs {
		c.onEvent(ev)
	}
}

// stop drops the queued events once the caller has returned, and every later one.
func (c *pendingCall) stop() {
	c.mu.Lock()
	c.stopped, c.events, c.eventBytes = true, nil, 0
	c.mu.Unlock()
}

// CallOption tunes one Call.
type CallOption func(*pendingCall)

// WithEvents delivers the EVENTs that name this call to fn, in order, on the
// caller's goroutine and always before Call returns.
func WithEvents(fn func(Event)) CallOption { return func(c *pendingCall) { c.onEvent = fn } }

// Call sends one CALL and waits for its REPLY, decoding the result into result
// (nil to ignore it). A typed REPLY error comes back as *Error, so errors.Is
// works on the sentinels; a link that drops mid-call is runner.ErrRunnerOffline.
func (p *Peer) Call(ctx context.Context, method string, args, result any, opts ...CallOption) (err error) {
	var raw json.RawMessage
	if args != nil {
		if raw, err = json.Marshal(args); err != nil {
			return fmt.Errorf("runnerwire: marshal %s args: %w", method, err)
		}
	}
	body, err := Marshal(Call{Method: method, Args: raw})
	if err != nil {
		return err
	}
	pc := &pendingCall{method: method, reply: make(chan Reply, 1), notify: make(chan struct{}, 1)}
	for _, o := range opts {
		o(pc)
	}
	p.mu.Lock()
	if p.err != nil {
		p.mu.Unlock()
		return p.offline()
	}
	if len(p.calls) >= MaxCalls {
		p.mu.Unlock()
		return Refuse("runner connection has too many pending calls; reconnect to clear abandoned calls")
	}
	id := p.ids.Next()
	p.calls[id] = pc
	p.mu.Unlock()
	// The entry stays until the REPLY removes it (onReply), also for a call
	// whose caller gave up: the runner still answers, and a REPLY for an id
	// that is not pending is a protocol error. Across a resume, the entries
	// the peer can no longer answer are retired (retireUnanswerable).
	defer func() {
		p.mu.Lock()
		var orphans []uint32
		if !pc.abandoned {
			orphans = p.byCall[id]
			delete(p.byCall, id)
		}
		p.mu.Unlock()
		if err != nil {
			p.resetParked(orphans)
		}
	}()
	if seq, err := p.sendFrame(ctx, Frame{Type: TypeCall, Stream: id, Payload: body}); err != nil {
		p.mu.Lock()
		if seq == 0 {
			// Never sequenced, so never in the replay buffer: nothing can answer it.
			delete(p.calls, id)
		} else {
			// A failed write may already be in the replay buffer. Preserve the id
			// so its eventual reply is discarded after resume, never misdelivered.
			pc.abandoned = true
		}
		p.mu.Unlock()
		pc.stop()
		return err
	}
	finish := func(rep Reply) error {
		if pc.onEvent != nil {
			pc.drain()
		}
		if rep.Error != nil {
			return rep.Error.Err()
		}
		if result != nil && len(rep.Result) > 0 {
			return json.Unmarshal(rep.Result, result)
		}
		return nil
	}
	for {
		select {
		case <-pc.notify:
			pc.drain()
		case rep := <-pc.reply:
			return finish(rep)
		case <-ctx.Done():
			p.mu.Lock()
			// The tombstone: this entry stays in p.calls until the runner's REPLY
			// removes it; the runner answers every CALL it received on a live link.
			// A resume carries it, and the resumed peer retires it once that REPLY
			// can no longer come (retireUnanswerable). Its events are dropped.
			pc.abandoned = true
			p.mu.Unlock()
			pc.stop()
			_ = p.send(p.ctx, Frame{Type: TypeReset, Stream: id, Payload: EncodeReset(ResetCancelled)})
			return ctx.Err()
		case <-p.done:
			select {
			case rep := <-pc.reply:
				return finish(rep)
			default:
			}
			return p.offline()
		}
	}
}

func (p *Peer) onReply(f Frame) error {
	var rep Reply
	if err := Unmarshal(f.Payload, &rep); err != nil {
		return err
	}
	// A code outside the closed list is not a bad frame: a newer peer may know a code this one does not,
	// and (*Error).Err degrades it to internal with the message kept.
	p.mu.Lock()
	pc := p.calls[f.Stream]
	if pc == nil {
		p.mu.Unlock()
		return fmt.Errorf("%w: REPLY for call %d, which is not pending", ErrBadFrame, f.Stream)
	}
	delete(p.calls, f.Stream)
	var orphans []uint32
	if pc.abandoned {
		orphans = p.byCall[f.Stream]
		delete(p.byCall, f.Stream)
	}
	p.mu.Unlock()
	pc.reply <- rep // one slot, and the entry is gone: only the first REPLY gets here
	p.resetParked(orphans)
	return nil
}

// resetParked aborts streams a call opened that nobody will claim.
func (p *Peer) resetParked(ids []uint32) {
	for _, sid := range ids {
		if s, err := p.TakeStream(sid); err == nil {
			_ = s.resetQueued(ResetCancelled)
		}
	}
}

func (p *Peer) onCall(f Frame) error {
	var c Call
	if err := Unmarshal(f.Payload, &c); err != nil {
		return err
	}
	if org, _ := InitiatedByOrg(f.Stream); org == p.cfg.Org {
		return fmt.Errorf("%w: CALL on stream %d from the wrong side", ErrBadFrame, f.Stream)
	}
	ctx, cancel := context.WithCancel(p.ctx)
	p.mu.Lock()
	if _, dup := p.inCalls[f.Stream]; dup {
		p.mu.Unlock()
		cancel()
		return fmt.Errorf("%w: call %d already in flight", ErrBadFrame, f.Stream)
	}
	if len(p.inCalls) >= MaxCalls {
		p.mu.Unlock()
		cancel()
		return fmt.Errorf("%w: too many incoming calls", ErrBadFrame)
	}
	p.inCalls[f.Stream] = cancel
	p.mu.Unlock()
	go func() {
		defer func() {
			p.mu.Lock()
			delete(p.inCalls, f.Stream)
			p.mu.Unlock()
			cancel()
		}()
		var res any
		var err error
		if p.cfg.OnCall == nil {
			err = fmt.Errorf("no call handler for %q", c.Method)
		} else {
			err = guarded(func() (herr error) {
				res, herr = p.cfg.OnCall(ctx, f.Stream, c.Method, c.Args)
				return herr
			})
		}
		var rep Reply
		if err != nil {
			rep.Error = ErrorFor(err)
		} else if res != nil {
			if rep.Result, err = json.Marshal(res); err != nil {
				rep.Error = ErrorFor(err)
			}
		}
		b, merr := Marshal(rep)
		if merr == nil && len(b) > MaxControlFrame {
			merr = fmt.Errorf("%d bytes, over the %d-byte frame limit", len(b), MaxControlFrame)
		}
		if merr != nil {
			// A REPLY that cannot be sent still answers the call, so the caller fails now
			// instead of at its deadline and its tombstone clears.
			b, _ = Marshal(Reply{Error: &Error{Code: CodeInternal, Message: "the runner's reply could not be sent: " + merr.Error()}})
		}
		_ = p.send(p.ctx, Frame{Type: TypeReply, Stream: f.Stream, Payload: b})
	}()
	return nil
}
