// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

var _ runner.ProxyNarrower = (*Orchestrator)(nil)

// ProxyNarrowState forwards a read of ref's proxy deny list to its substrate
// (runner.ProxyNarrower). A substrate that does not narrow in place answers
// ErrNarrowNotOffered with no call.
func (o *Orchestrator) ProxyNarrowState(ctx context.Context, ref string) (runner.ProxyNarrowState, error) {
	s, err := o.subForRef(ctx, ref)
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
// ErrNarrowNotOffered with no call.
func (o *Orchestrator) NarrowProxy(ctx context.Context, ref string, denies []string) (runner.ProxyNarrowState, error) {
	s, err := o.subForRef(ctx, ref)
	if err != nil {
		return runner.ProxyNarrowState{}, err
	}
	n, ok := s.(runner.ProxyNarrower)
	if !ok {
		return runner.ProxyNarrowState{}, runner.ErrNarrowNotOffered
	}
	return n.NarrowProxy(ctx, ref, denies)
}
