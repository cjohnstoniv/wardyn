// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

var (
	_ runner.ProxyFencer            = (*Orchestrator)(nil)
	_ runner.FencedStarter          = (*Orchestrator)(nil)
	_ runner.ProxyGenerationStopper = (*Orchestrator)(nil)
	_ runner.ProxyReviverAt         = (*Orchestrator)(nil)
	_ runner.LostProxyStopper       = (*Orchestrator)(nil)
)

// FenceProxy forwards a contained run's fence to ref's substrate (runner.ProxyFencer). A substrate that
// cannot cut an agent off its networks answers runner.ErrEndUnsupported.
func (o *Orchestrator) FenceProxy(ctx context.Context, ref string) error {
	s, err := o.subForRef(ctx, ref)
	if err != nil {
		return err
	}
	f, ok := s.(runner.ProxyFencer)
	if !ok {
		return runner.ErrEndUnsupported
	}
	return f.FenceProxy(ctx, ref)
}

// StartFenced forwards a fenced start to ref's substrate (runner.FencedStarter). A substrate that cannot start a
// stopped sandbox with no network answers runner.ErrEndUnsupported.
func (o *Orchestrator) StartFenced(ctx context.Context, ref string) error {
	s, err := o.subForRef(ctx, ref)
	if err != nil {
		return err
	}
	f, ok := s.(runner.FencedStarter)
	if !ok {
		return runner.ErrEndUnsupported
	}
	return f.StartFenced(ctx, ref)
}

// StopProxyAt forwards a stop that names a policy generation to ref's substrate
// (runner.ProxyGenerationStopper). A substrate without generations is sent the plain StopProxy.
func (o *Orchestrator) StopProxyAt(ctx context.Context, ref string, gen int) error {
	s, err := o.subForRef(ctx, ref)
	if err != nil {
		return err
	}
	if at, ok := s.(runner.ProxyGenerationStopper); ok {
		return at.StopProxyAt(ctx, ref, gen)
	}
	return o.StopProxy(ctx, ref)
}

// StopLostProxyAt forwards a lost run's proxy stop to ref's substrate (runner.LostProxyStopper). A substrate
// without it is sent StopProxy for gen <= 0 and StopProxyAt otherwise, as the lost-run path sent before.
func (o *Orchestrator) StopLostProxyAt(ctx context.Context, ref string, gen int) error {
	s, err := o.subForRef(ctx, ref)
	if err != nil {
		return err
	}
	if lp, ok := s.(runner.LostProxyStopper); ok {
		return lp.StopLostProxyAt(ctx, ref, gen)
	}
	if gen <= 0 {
		return o.StopProxy(ctx, ref)
	}
	return o.StopProxyAt(ctx, ref, gen)
}

// EndSandboxAt is StopProxyAt's twin for the lease end: the plain EndSandbox on a substrate without generations.
func (o *Orchestrator) EndSandboxAt(ctx context.Context, ref string, gen int) error {
	s, err := o.subForRef(ctx, ref)
	if err != nil {
		return err
	}
	if at, ok := s.(runner.ProxyGenerationStopper); ok {
		return at.EndSandboxAt(ctx, ref, gen)
	}
	return o.EndSandbox(ctx, ref)
}

// ReplaceProxyHashedAt forwards a hashed replacement that names a policy generation to ref's substrate
// (runner.ProxyReviverAt). A substrate without generations is sent the plain ReplaceProxyHashed.
func (o *Orchestrator) ReplaceProxyHashedAt(ctx context.Context, ref string, cfgJSON []byte, gen int) (string, error) {
	s, err := o.subForRef(ctx, ref)
	if err != nil {
		return "", err
	}
	if at, ok := s.(runner.ProxyReviverAt); ok {
		return at.ReplaceProxyHashedAt(ctx, ref, cfgJSON, gen)
	}
	return o.ReplaceProxyHashed(ctx, ref, cfgJSON)
}
