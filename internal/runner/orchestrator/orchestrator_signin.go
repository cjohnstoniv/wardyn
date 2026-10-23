// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var (
	_ runner.SigninExecer   = (*Orchestrator)(nil)
	_ runner.SigninTargeter = (*Orchestrator)(nil)
)

// SigninsFor reports whether the substrate a create of class routes to
// (substrateFor, the same first match) implements runner.SigninExecer.
func (o *Orchestrator) SigninsFor(ctx context.Context, class types.ConfinementClass) bool {
	s, err := o.substrateFor(ctx, class)
	if err != nil {
		return false
	}
	_, ok := s.(runner.SigninExecer)
	return ok
}

// ExecStreamAs forwards to ref's substrate when it implements
// runner.SigninExecer; any other refuses with runner.ErrSigninUnsupported.
func (o *Orchestrator) ExecStreamAs(ctx context.Context, ref, user string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	s, err := o.subForRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	se, ok := s.(runner.SigninExecer)
	if !ok {
		return nil, runner.ErrSigninUnsupported
	}
	return se.ExecStreamAs(ctx, ref, user, spec)
}

// SignalMain forwards like ExecStreamAs.
func (o *Orchestrator) SignalMain(ctx context.Context, ref, signal string) error {
	s, err := o.subForRef(ctx, ref)
	if err != nil {
		return err
	}
	se, ok := s.(runner.SigninExecer)
	if !ok {
		return runner.ErrSigninUnsupported
	}
	return se.SignalMain(ctx, ref, signal)
}
