// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var (
	_ runner.MCPSandboxer    = (*Orchestrator)(nil)
	_ runner.MCPProxyStopper = (*Orchestrator)(nil)
)

// intersectMCPKinds folds one executor's MCP sandbox kinds into the kinds of the executors before it (i of
// them). Unlike most capability flags it is an intersection, so a deployment with one executor that cannot run
// MCP sandboxes advertises none: the run could be routed there.
func intersectMCPKinds(i int, have, next []types.MachineKind) []types.MachineKind {
	if i == 0 {
		return slices.Clone(next)
	}
	return slices.DeleteFunc(have, func(k types.MachineKind) bool { return !slices.Contains(next, k) })
}

// mcpSandboxer is the substrate an MCP sandbox of class runs on: the one substrateFor routes the class to.
func (o *Orchestrator) mcpSandboxer(ctx context.Context, class types.ConfinementClass) (runner.MCPSandboxer, error) {
	s, err := o.substrateFor(ctx, class)
	if err != nil {
		return nil, err
	}
	m, ok := s.(runner.MCPSandboxer)
	if !ok {
		return nil, fmt.Errorf("orchestrator: %s runs no MCP sandboxes", s.Name())
	}
	return m, nil
}

// CreateMCPSandbox forwards to the substrate of the sandbox's class.
func (o *Orchestrator) CreateMCPSandbox(ctx context.Context, spec runner.MCPSandboxSpec) (runner.MCPSandbox, error) {
	m, err := o.mcpSandboxer(ctx, spec.ID.Class)
	if err != nil {
		return runner.MCPSandbox{}, err
	}
	return m.CreateMCPSandbox(ctx, spec)
}

// StartMCPSandbox forwards to the substrate of the sandbox's class.
func (o *Orchestrator) StartMCPSandbox(ctx context.Context, id runner.MCPSandboxID) error {
	m, err := o.mcpSandboxer(ctx, id.Class)
	if err != nil {
		return err
	}
	return m.StartMCPSandbox(ctx, id)
}

// KillMCPSandbox forwards to the substrate of the sandbox's class.
func (o *Orchestrator) KillMCPSandbox(ctx context.Context, id runner.MCPSandboxID) error {
	m, err := o.mcpSandboxer(ctx, id.Class)
	if err != nil {
		return err
	}
	return m.KillMCPSandbox(ctx, id)
}

// MCPSandboxStatus forwards to the substrate of the sandbox's class.
func (o *Orchestrator) MCPSandboxStatus(ctx context.Context, id runner.MCPSandboxID) (runner.MCPSandbox, error) {
	m, err := o.mcpSandboxer(ctx, id.Class)
	if err != nil {
		return runner.MCPSandbox{}, err
	}
	return m.MCPSandboxStatus(ctx, id)
}

// KillRunMCPSandboxes asks every executor that can hold MCP sandboxes to kill the run's: with the rows unread,
// which one holds them is not known.
func (o *Orchestrator) KillRunMCPSandboxes(ctx context.Context, runID uuid.UUID) error {
	var errs []error
	for _, s := range o.locals() {
		if m, ok := s.(runner.MCPSandboxer); ok {
			if err := m.KillRunMCPSandboxes(ctx, runID); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", s.Name(), err))
			}
		}
	}
	return errors.Join(errs...)
}

// StopMCPProxies forwards to ref's substrate, as StopProxy is. A substrate that cannot hold MCP sandboxes has no
// MCP proxy to stop.
func (o *Orchestrator) StopMCPProxies(ctx context.Context, ref string) error {
	s, err := o.subForRef(ctx, ref)
	if err != nil {
		return err
	}
	if m, ok := s.(runner.MCPProxyStopper); ok {
		return m.StopMCPProxies(ctx, ref)
	}
	return nil
}
