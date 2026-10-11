// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"context"
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/google/uuid"
)

// OutputSettled reads producer completion and durable byte acknowledgement.
// It never closes a producer, advances its cursor or forgets retained output.
func (s *Server) OutputSettled(ctx context.Context, id uuid.UUID, ref string) (int64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	if id == uuid.Nil || ref == "" {
		return 0, false, errors.New("output settlement requires exact run/ref")
	}
	s.mu.Lock()
	b := s.outputs[id]
	s.mu.Unlock()
	if b == nil {
		return 0, false, runner.ErrOutputUnrecoverable
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state.Ref != ref {
		return 0, false, errors.New("output settlement reference changed")
	}
	if b.stopped != nil {
		return 0, false, b.stopped
	}
	return b.state.End, b.settled(), nil
}

// settled reports a completed, failure-free producer whose every byte the org
// acknowledged. The caller holds b.mu.
func (b *outputBuffer) settled() bool {
	return b.stopped == nil && b.state.Complete && !b.interrupted && b.state.Failure == "" && b.state.Ack == b.state.End
}
