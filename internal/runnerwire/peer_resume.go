// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"fmt"
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
	for _, f := range frames {
		if err := p.conn.WriteFrame(ctx, f); err != nil {
			p.fail(err)
			return err
		}
	}
	go p.keepalive(PingInterval)
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
	p.lastPong.Store(time.Now().UnixNano())
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case now := <-tick.C:
			if now.Sub(time.Unix(0, p.lastPong.Load())) >= MissedPings*every {
				p.fail(fmt.Errorf("%w: keepalive timeout", ErrLinkDown))
				return
			}
			ctx, cancel := context.WithTimeout(p.ctx, every)
			err := p.Send(ctx, Frame{Type: TypePing, Payload: EncodePing(now)})
			cancel()
			if err != nil {
				p.fail(err)
				return
			}
		case <-p.done:
			return
		}
	}
}
