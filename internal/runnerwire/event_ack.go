// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import "context"

// EventAck waits for durable acceptance by the receiving org before returning.
// A link drop fails the wait; the caller retains its durable record and replays
// it with its stable event identity. Sequence replay can also deliver it, so
// the durable receiver must deduplicate by that identity.
func (p *Peer) EventAck(ctx context.Context, ev Event) error {
	return p.EventsAck(ctx, []Event{ev})
}

// EventsAck sends every event in order, then waits once for durable acceptance
// of the last. The org ingests sequenced events in order before its cumulative
// ACK covers them, so that one ACK covers the whole batch.
func (p *Peer) EventsAck(ctx context.Context, evs []Event) error {
	var seq uint64
	for _, ev := range evs {
		b, err := Marshal(ev)
		if err != nil {
			return err
		}
		if seq, err = p.sendFrame(ctx, Frame{Type: TypeEvent, Payload: b}); err != nil {
			return err
		}
	}
	if seq == 0 {
		return nil
	}
	return p.replay.waitAck(ctx, p.done, seq)
}

func (b *ReplayBuffer) waitAck(ctx context.Context, down <-chan struct{}, seq uint64) error {
	for {
		b.mu.Lock()
		if b.acked >= seq {
			b.mu.Unlock()
			return nil
		}
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-down:
			return ErrLinkDown
		}
	}
}

func (p *Peer) wakeAck() {
	select {
	case p.ackWake <- struct{}{}:
	default:
	}
}

// One worker coalesces cumulative ACKs so a slow writer cannot create an
// unbounded goroutine per event or duplicate frame.
func (p *Peer) sendAcks() {
	for {
		select {
		case <-p.ackWake:
			if err := p.send(p.ctx, Frame{Type: TypeAck, Payload: EncodeAck(p.LastReceivedSeq())}); err != nil {
				return
			}
		case <-p.done:
			return
		}
	}
}
