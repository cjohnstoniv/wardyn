// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

type pendingCall struct {
	abandoned bool // the caller stopped waiting; guarded by Peer.mu
	reply     chan Reply
	notify    chan struct{}
	onEvent   func(Event)

	mu     sync.Mutex
	events []Event
}

// queue holds an event for the caller's goroutine; false when the call takes none.
func (c *pendingCall) queue(ev Event) bool {
	if c.onEvent == nil {
		return false
	}
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
	return true
}

func (c *pendingCall) drain() {
	c.mu.Lock()
	evs := c.events
	c.events = nil
	c.mu.Unlock()
	for _, ev := range evs {
		c.onEvent(ev)
	}
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
	pc := &pendingCall{reply: make(chan Reply, 1), notify: make(chan struct{}, 1)}
	for _, o := range opts {
		o(pc)
	}
	p.mu.Lock()
	if p.err != nil {
		p.mu.Unlock()
		return p.offline()
	}
	id := p.ids.Next()
	p.calls[id] = pc
	p.mu.Unlock()
	// The entry stays until the REPLY removes it (onReply), also for a call
	// whose caller gave up: the runner still answers, and a REPLY for an id
	// that is not pending is a protocol error.
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
	if err := p.send(ctx, Frame{Type: TypeCall, Stream: id, Payload: body}); err != nil {
		p.mu.Lock()
		delete(p.calls, id)
		p.mu.Unlock()
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
			pc.abandoned = true
			p.mu.Unlock()
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
	if rep.Error != nil && !rep.Error.Code.Valid() {
		return fmt.Errorf("%w: reply error code %q is not in the closed list", ErrBadFrame, rep.Error.Code)
	}
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
			_ = s.Reset(ResetCancelled)
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
			res, err = p.cfg.OnCall(ctx, f.Stream, c.Method, c.Args)
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
		if merr != nil {
			return
		}
		_ = p.send(p.ctx, Frame{Type: TypeReply, Stream: f.Stream, Payload: b})
	}()
	return nil
}
