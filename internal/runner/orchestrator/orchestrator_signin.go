// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

var _ runner.SigninExecer = (*Orchestrator)(nil)

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
