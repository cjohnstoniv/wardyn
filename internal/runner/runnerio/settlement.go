// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/google/uuid"
)

// OutputSettled reads producer completion and durable byte acknowledgement.
// It never closes a producer, advances its cursor or forgets retained output.
// ErrOutputUnrecoverable means no buffer: never captured, or already gone.
// The caller tells the two apart from its own record of the create (the
// offline manager's OutputCapture) and the org's no_output receipt.
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

// errOutputErased stops readers and writers of an output deleted under the
// retention ceiling for an org-erased run.
var errOutputErased = errors.New("runnerio: output erased")

// EraseOutput deletes a run's retained output, for an org-erased run whose
// quarantined evidence reached its retention ceiling. It is idempotent.
func (s *Server) EraseOutput(id uuid.UUID) error {
	if id == uuid.Nil {
		return errors.New("runnerio: missing output run id")
	}
	s.mu.Lock()
	b := s.outputs[id]
	delete(s.outputs, id)
	s.mu.Unlock()
	unlock := func() {}
	if b != nil {
		b.mu.Lock()
		unlock = b.mu.Unlock
		b.stopped = errOutputErased
		b.notify()
	}
	tomb, err := s.entombOutputDir(filepath.Join(s.dir, id.String()))
	unlock()
	if err != nil || tomb == "" {
		return err
	}
	return removeAll(tomb)
}
