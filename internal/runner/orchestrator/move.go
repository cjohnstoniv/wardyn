// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"
	"fmt"
	"io"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var (
	_ runner.StateExporter = (*Orchestrator)(nil)
	_ runner.StateImporter = (*Orchestrator)(nil)
	_ runner.MoveUnbounder = (*Orchestrator)(nil)
)

// ExportState forwards a move's export to ref's substrate (runner.StateExporter).
func (o *Orchestrator) ExportState(ctx context.Context, ref string, set runner.CarriedSet) (runner.StateExport, error) {
	s, err := o.subForRef(ctx, ref)
	if err != nil {
		return runner.StateExport{}, err
	}
	e, ok := s.(runner.StateExporter)
	if !ok {
		return runner.StateExport{}, fmt.Errorf("orchestrator: %s cannot export a run's files", s.Name())
	}
	return e.ExportState(ctx, ref, set)
}

// MoveUnbound forwards a move's notice to the substrate that held n.Ref (runner.MoveUnbounder). A
// substrate that takes no notice needs none: nothing on it reports run state to the organisation.
func (o *Orchestrator) MoveUnbound(ctx context.Context, n runner.MoveUnbound) error {
	s, err := o.subForRef(ctx, n.Ref)
	if err != nil {
		return err
	}
	if u, ok := s.(runner.MoveUnbounder); ok {
		return u.MoveUnbound(ctx, n)
	}
	return nil
}

// ImportSandbox routes a move's import as CreateSandbox routes a create: to spec.RunnerID's runner, or
// else to a substrate that can enforce the class.
func (o *Orchestrator) ImportSandbox(ctx context.Context, spec runner.SandboxSpec, set runner.CarriedSet, maxBytes int64, carried io.Reader) (runner.Sandbox, runner.CarriedManifest, error) {
	var sub substrate.Substrate
	var err error
	if spec.RunnerID != "" {
		sub, err = o.substrateForRunner(spec.RunnerID)
	} else {
		sub, err = o.substrateFor(ctx, spec.ConfinementClass)
	}
	if err != nil {
		return runner.Sandbox{}, runner.CarriedManifest{}, fmt.Errorf("%w: %w", runner.ErrMoveCreate, err)
	}
	return o.importOn(ctx, sub, spec, set, maxBytes, carried)
}

// ImportSandboxOn imports on the named executor of this deployment, as CreateSandboxOn creates there.
func (o *Orchestrator) ImportSandboxOn(ctx context.Context, executor string, spec runner.SandboxSpec, set runner.CarriedSet, maxBytes int64, carried io.Reader) (runner.Sandbox, runner.CarriedManifest, error) {
	sub, ok := o.local(executor)
	if !ok {
		return runner.Sandbox{}, runner.CarriedManifest{}, fmt.Errorf("%w: %w: %q is not an executor of this deployment", runner.ErrMoveCreate, runner.ErrExecutorUnavailable, executor)
	}
	class := spec.ConfinementClass
	if class == "" {
		class = types.CC1
	}
	cs, err := o.classesFor(ctx, sub)
	if err != nil {
		return runner.Sandbox{}, runner.CarriedManifest{}, fmt.Errorf("%w: %w: %s: %v", runner.ErrMoveCreate, runner.ErrExecutorUnavailable, executor, err)
	}
	if !slices.Contains(cs.Classes, class) {
		return runner.Sandbox{}, runner.CarriedManifest{}, fmt.Errorf("%w: %w: %s cannot enforce class %s", runner.ErrMoveCreate, runner.ErrExecutorUnavailable, executor, class)
	}
	return o.importOn(ctx, sub, spec, set, maxBytes, carried)
}

// importOn imports on sub and records ref->substrate, as createOn does for a create.
func (o *Orchestrator) importOn(ctx context.Context, sub substrate.Substrate, spec runner.SandboxSpec, set runner.CarriedSet, maxBytes int64, carried io.Reader) (runner.Sandbox, runner.CarriedManifest, error) {
	imp, ok := sub.(runner.StateImporter)
	if !ok {
		return runner.Sandbox{}, runner.CarriedManifest{}, fmt.Errorf("%w: %s cannot take a moved run", runner.ErrMoveCreate, sub.Name())
	}
	sb, m, err := imp.ImportSandbox(ctx, spec, set, maxBytes, carried)
	if err != nil {
		return runner.Sandbox{}, runner.CarriedManifest{}, err
	}
	o.mu.Lock()
	o.byRef[sb.Ref] = sub
	o.mu.Unlock()
	// Write-through before the sandbox is handed out, as at create.
	if o.refStore != nil {
		if perr := o.refStore.PutRef(ctx, sb.Ref, sub.Name()); perr != nil {
			o.forget(ctx, sb.Ref, false)
			_ = sub.KillSandbox(ctx, sb.Ref)
			return runner.Sandbox{}, runner.CarriedManifest{}, fmt.Errorf("%w: orchestrator: persist ref %q -> %s: %w", runner.ErrMoveFiles, sb.Ref, sub.Name(), perr)
		}
	}
	return sb, m, nil
}
