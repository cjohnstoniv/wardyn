// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

type outputSink struct {
	mu     sync.Mutex
	writer io.Writer
	offset int64
	end    func(error)
	ended  bool
}

func (s *outputSink) accept(offset int64, data []byte) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if offset < 0 || offset > s.offset {
		return s.offset, runner.ErrOutputUnrecoverable
	}
	skip := min(int64(len(data)), s.offset-offset)
	data = data[skip:]
	if len(data) == 0 {
		return s.offset, nil
	}
	if s.ended {
		return s.offset, io.ErrClosedPipe
	}
	if s.end == nil {
		s.end = runner.BeginOutputDrain(s.writer)
	}
	n, err := s.writer.Write(data)
	if n < 0 || n > len(data) {
		return s.offset, errors.New("remote: invalid output writer count")
	}
	s.offset += int64(n)
	if n < len(data) && err == nil {
		err = io.ErrShortWrite
	}
	return s.offset, err
}

func (s *outputSink) finish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	s.ended = true
	if s.end == nil {
		s.end = runner.BeginOutputDrain(s.writer)
	}
	s.end(err)
}

// HandleOpen resumes at a raw byte offset. Duplicate prefixes are discarded;
// a forward gap is an error. ACK follows successful writer acceptance.
func (s *Substrate) HandleOpen(st *runnerwire.Stream, o runnerwire.Open) error {
	if o.Kind != runnerwire.KindOutput || o.Offset < 0 {
		return ErrUnsupportedStream
	}
	id, err := uuid.Parse(o.Target)
	if err != nil || id == uuid.Nil {
		return ErrUnsupportedStream
	}
	s.mu.Lock()
	sink := s.outputs[id]
	s.mu.Unlock()
	if sink == nil {
		return ErrUnsupportedStream
	}
	go s.receiveOutput(id, sink, st, o.Offset)
	return nil
}

func (s *Substrate) outputACK(ctx context.Context, id uuid.UUID, offset int64) error {
	delay := 50 * time.Millisecond
	for {
		attempt, cancel := context.WithTimeout(ctx, callTimeout)
		err := s.call(attempt, runnerwire.MethodOutputAck, runnerwire.OutputAckArgs{RunID: id, Offset: offset}, nil)
		cancel()
		if err == nil || ctx.Err() != nil {
			return err
		}
		// Only transport ambiguity is retryable. A runner refusal or disk error
		// must finish the recording with failure, never spin on a stopped buffer.
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, runner.ErrRunnerOffline) {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay = min(2*delay, time.Second)
	}
}

func (s *Substrate) receiveOutput(id uuid.UUID, sink *outputSink, st *runnerwire.Stream, offset int64) {
	// Output has no stdin. Half-close it so an EOF can release both wire ends
	// without RESET, which would discard bytes still waiting in a reader.
	_ = st.CloseWrite()
	data := make([]byte, 64<<10)
	for {
		n, readErr := st.Read(data)
		accepted, writeErr := sink.accept(offset, data[:n])
		offset += int64(n)
		// Even a short write has accepted its prefix; only those bytes are ACKed.
		ackErr := s.outputACK(st.Context(), id, accepted)
		if writeErr != nil {
			sink.finish(writeErr)
			_ = st.Close()
			return
		}
		if ackErr != nil {
			if st.Context().Err() == nil {
				sink.finish(ackErr)
			}
			_ = st.Close()
			return
		} // transport loss retains writer and offset for reconnect
		if errors.Is(readErr, io.EOF) {
			sink.finish(nil)
			s.mu.Lock()
			if s.outputs[id] == sink {
				delete(s.outputs, id)
			}
			s.mu.Unlock()
			return
		}
		if readErr != nil {
			return
		} // transport loss preserves accepted offset and drain
	}
}
