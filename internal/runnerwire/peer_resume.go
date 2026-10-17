// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// Start runs a peer and writes the resume replay before any normal writes.
// Reading runs concurrently so two full replay buffers cannot deadlock each
// other. A fresh session passes zero against its new, empty replay buffer.
func (p *Peer) Start(ctx context.Context, lastSeq uint64) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	frames, err := p.replay.Since(lastSeq)
	if err != nil {
		return err
	}
	// The authenticated resume cursor is itself an acknowledgement. Leaving
	// these bytes reserved can deadlock a full buffer whose last ACK was lost.
	if err := p.replay.Ack(lastSeq); err != nil {
		return err
	}
	go func() { _ = p.Run(ctx) }()
	// Keepalive runs before the replay, so a replay write that stops moving
	// fails the link like any other.
	go p.keepalive(PingInterval)
	for _, f := range frames {
		if err := p.write(ctx, f); err != nil {
			p.fail(err)
			return err
		}
	}
	return nil
}

// ResumeConfig snapshots an ended connection after its read loop stops. Byte
// streams have been reset; live callers already received ErrRunnerOffline.
func (p *Peer) ResumeConfig(ctx context.Context) (PeerConfig, error) {
	select {
	case <-p.stopped:
	case <-ctx.Done():
		return PeerConfig{}, ctx.Err()
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) >= MaxCalls {
		return PeerConfig{}, ErrResumeGap
	}
	cfg := p.cfg
	cfg.Replay, cfg.LastReceived, cfg.NextID = p.replay, p.recvSeq, p.ids.next
	cfg.AbandonedCalls = make([]uint32, 0, len(p.calls))
	for id := range p.calls {
		cfg.AbandonedCalls = append(cfg.AbandonedCalls, id)
	}
	return cfg, nil
}

func (p *Peer) keepalive(every time.Duration) {
	start := time.Now()
	p.lastPong.Store(&start)
	tick := time.NewTicker(every)
	defer tick.Stop()
	// The PING is sent off the ticker, one at a time: a write stalled on the
	// socket must not stop the missed-PONG check, which is what fails the link
	// and so closes the socket under the stalled write.
	var pinging atomic.Bool
	for {
		select {
		case now := <-tick.C:
			if p.pongOverdue(now, every) {
				p.fail(fmt.Errorf("%w: keepalive timeout", ErrLinkDown))
				return
			}
			if p.writeStalled(now, every) {
				p.fail(fmt.Errorf("%w: keepalive: write stalled", ErrLinkDown))
				return
			}
			if !pinging.CompareAndSwap(false, true) {
				continue
			}
			go func() {
				defer pinging.Store(false)
				// A PING reserves no replay room and its write runs under the link's own ctx, so no
				// caller deadline would bound it: the stall check above fails a stuck write.
				if err := p.Send(p.ctx, Frame{Type: TypePing, Payload: EncodePing(now)}); err != nil {
					p.fail(err)
				}
			}()
		case <-p.done:
			return
		}
	}
}

// pongOverdue reports whether MissedPings intervals have passed since the last
// PONG. Both times keep their monotonic reading, so a wall-clock step neither
// fails a healthy link nor delays failing a silent one.
func (p *Peer) pongOverdue(now time.Time, every time.Duration) bool {
	return now.Sub(*p.lastPong.Load()) >= MissedPings*every
}

// writeStalled reports whether one write has been in progress for MissedPings
// intervals. A PONG says only that the peer sends, not that it reads, so a
// stalled write is timed on the write itself.
func (p *Peer) writeStalled(now time.Time, every time.Duration) bool {
	w := p.writing.Load()
	return w != nil && now.Sub(*w) >= MissedPings*every
}
