// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// PauseAgent forwards to ref's substrate when it implements runner.AgentPauser.
func (o *Orchestrator) PauseAgent(ctx context.Context, ref string, gen int64) (bool, error) {
	p, err := o.agentPauser(ctx, ref)
	if err != nil {
		return false, err
	}
	return p.PauseAgent(ctx, ref, gen)
}

// ResumeAgent forwards to ref's substrate when it implements runner.AgentPauser.
func (o *Orchestrator) ResumeAgent(ctx context.Context, ref string, gen int64) (bool, error) {
	p, err := o.agentPauser(ctx, ref)
	if err != nil {
		return false, err
	}
	return p.ResumeAgent(ctx, ref, gen)
}

func (o *Orchestrator) agentPauser(ctx context.Context, ref string) (runner.AgentPauser, error) {
	s, err := o.subForRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	p, ok := s.(runner.AgentPauser)
	if !ok {
		return nil, runner.ErrAgentPauseUnsupported
	}
	return p, nil
}
