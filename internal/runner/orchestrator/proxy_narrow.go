// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

var _ runner.ProxyNarrower = (*Orchestrator)(nil)

// ProxyNarrowState forwards a read of ref's proxy deny list to its substrate
// (runner.ProxyNarrower). A substrate that does not narrow in place answers
// ErrNarrowNotOffered with no call, and so does a ref no substrate is tracked
// for: none can be asked, so an edit takes the revive path rather than failing
// as an unreadable proxy. A failed ref lookup and an offline runner keep their
// own errors, which the edit answers as retryable.
func (o *Orchestrator) ProxyNarrowState(ctx context.Context, ref string) (runner.ProxyNarrowState, error) {
	s, err := o.subForRef(ctx, ref)
	if errors.Is(err, errRefUntracked) {
		return runner.ProxyNarrowState{}, fmt.Errorf("%w: %w", runner.ErrNarrowNotOffered, err)
	}
	if err != nil {
		return runner.ProxyNarrowState{}, err
	}
	n, ok := s.(runner.ProxyNarrower)
	if !ok {
		return runner.ProxyNarrowState{}, runner.ErrNarrowNotOffered
	}
	return n.ProxyNarrowState(ctx, ref)
}

// NarrowProxy forwards a narrowing of ref's proxy to its substrate
// (runner.ProxyNarrower). A substrate that does not narrow in place answers
// ErrNarrowNotOffered with no call, and a ref no substrate can be found for
// answers ErrNarrowNotSent.
func (o *Orchestrator) NarrowProxy(ctx context.Context, ref string, denies []string) (runner.ProxyNarrowState, error) {
	s, err := o.subForRef(ctx, ref)
	if err != nil {
		return runner.ProxyNarrowState{}, fmt.Errorf("%w: %w", runner.ErrNarrowNotSent, err)
	}
	n, ok := s.(runner.ProxyNarrower)
	if !ok {
		return runner.ProxyNarrowState{}, runner.ErrNarrowNotOffered
	}
	return n.NarrowProxy(ctx, ref, denies)
}
