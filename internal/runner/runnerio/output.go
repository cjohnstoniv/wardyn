// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

func (s *Server) sendOutput(peer *runnerwire.Peer, id uuid.UUID, b *outputBuffer) {
	ctx := s.life(peer)
	b.mu.Lock()
	offset := b.state.Ack
	b.mu.Unlock()
	go func() {
		delay := 50 * time.Millisecond
		for {
			st, err := peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindOutput, Target: id.String(), Offset: offset})
			if err == nil {
				s.copyOutput(ctx, st, b, offset)
				return
			}
			var reset *runnerwire.ResetError
			if !errors.As(err, &reset) || reset.Code != runnerwire.ResetCapacity {
				return
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			delay = min(2*delay, time.Second)
		}
	}()
}

func (s *Server) copyOutput(ctx context.Context, st *runnerwire.Stream, b *outputBuffer, offset int64) {
	life, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-st.Done():
			cancel()
		case <-life.Done():
		}
	}()
	data := make([]byte, outputChunk)
	for {
		n, err := b.read(life, offset, data)
		if n > 0 {
			wrote, writeErr := st.Write(data[:n])
			offset += int64(wrote)
			if writeErr != nil {
				_ = st.Reset(runnerwire.ResetInternal)
				return
			}
		}
		if errors.Is(err, io.EOF) {
			_ = st.CloseWrite()
			return
		}
		if err != nil {
			_ = st.Reset(runnerwire.ResetInternal)
			return
		}
	}
}

func (s *Server) outputAck(raw json.RawMessage) error {
	args, err := decode[runnerwire.OutputAckArgs](raw)
	if err != nil {
		return err
	}
	if args.RunID == uuid.Nil {
		return runnerwire.Refuse("output run id required")
	}
	s.mu.Lock()
	b := s.outputs[args.RunID]
	s.mu.Unlock()
	if b == nil {
		return runner.ErrOutputUnrecoverable
	}
	if err := b.ack(args.Offset); err != nil {
		return runnerwire.Refuse("output acknowledgment failed: %v", err)
	}
	return nil
}

func (s *Server) recoverOutput(ctx context.Context, peer *runnerwire.Peer, id uint32, raw json.RawMessage) (any, error) {
	args, err := decode[runnerwire.RefArgs](raw)
	if err != nil {
		return nil, err
	}
	ref, err := s.local(args.Ref)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	var found *outputBuffer
	for _, b := range s.outputs {
		b.mu.Lock()
		match := b.state.Ref == ref
		b.mu.Unlock()
		if match {
			found = b
			break
		}
	}
	s.mu.Unlock()
	if found == nil {
		return runnerwire.RecoverResult{Unrecoverable: true}, nil
	}
	found.mu.Lock()
	lost := found.start() != 0 || found.interrupted || found.stopped != nil || found.state.Failure != ""
	found.mu.Unlock()
	if lost {
		return runnerwire.RecoverResult{Unrecoverable: true}, nil
	}
	st, err := peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindOutput, Target: args.Ref, Call: id})
	if err != nil {
		return nil, err
	}
	go s.copyOutput(s.life(peer), st, found, 0)
	return runnerwire.RecoverResult{Stream: st.ID()}, nil
}
