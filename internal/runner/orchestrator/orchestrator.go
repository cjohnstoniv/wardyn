// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package orchestrator is the build-tag-free runner.Runner the control plane
// talks to. It multiplexes one or more confinement Substrates (the OCI/Docker
// substrate today; a non-OCI microVM VMM later) by Confinement Class: it routes
// each run to a substrate that can enforce its class, aggregates per-class
// capabilities, and presents a single runner.Runner surface. It carries ZERO
// target-specific code (the parity rule) — substrates are injected.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Orchestrator implements runner.Runner over a set of Substrates.
type Orchestrator struct {
	substrates []substrate.Substrate

	// mu guards byRef. The orchestrator is safe for concurrent use.
	mu sync.Mutex
	// byRef maps a sandbox ref to the substrate that created it, so lifecycle
	// ops (Exec/Wait/Attach/Status/Stop/Kill) route to the right substrate.
	byRef map[string]substrate.Substrate
}

var _ runner.Runner = (*Orchestrator)(nil)

// New constructs an Orchestrator over the given substrates (at least one). With
// a single substrate it is a thin pass-through; with several it routes by class.
func New(substrates ...substrate.Substrate) *Orchestrator {
	return &Orchestrator{substrates: substrates, byRef: make(map[string]substrate.Substrate)}
}

// Name reports the single substrate's name (so /healthz still shows "docker" for
// the OCI deployment), or "orchestrator" when several substrates are wired.
func (o *Orchestrator) Name() string {
	if len(o.substrates) == 1 {
		return o.substrates[0].Name()
	}
	return "orchestrator"
}

// Capabilities aggregates the substrates' ClassSupport into one Capabilities:
// the union of enforceable classes (strongest last) and the merged per-class
// substrate labels. A class is advertised only when SOME substrate enforces it.
func (o *Orchestrator) Capabilities(ctx context.Context) (runner.Capabilities, error) {
	caps := runner.Capabilities{
		Driver:   o.Name(),
		Resolved: map[types.ConfinementClass]string{},
	}
	seen := map[types.ConfinementClass]bool{}
	var classes []types.ConfinementClass
	for _, s := range o.substrates {
		cs, err := s.Classes(ctx)
		if err != nil {
			return runner.Capabilities{}, fmt.Errorf("orchestrator: %s classes: %w", s.Name(), err)
		}
		for _, c := range cs.Classes {
			if !seen[c] {
				seen[c] = true
				classes = append(classes, c)
			}
		}
		for k, v := range cs.Resolved {
			// First substrate to claim a class wins its label (deterministic).
			if _, ok := caps.Resolved[k]; !ok {
				caps.Resolved[k] = v
			}
		}
		caps.StructuralEgress = caps.StructuralEgress || cs.StructuralEgress
		caps.SessionRecording = caps.SessionRecording || cs.SessionRecording
	}
	// Strongest last regardless of substrate order.
	sort.Slice(classes, func(i, j int) bool { return classes[i].Rank() < classes[j].Rank() })
	caps.ConfinementClasses = classes
	return caps, nil
}

// CreateSandbox routes to a substrate that can enforce the requested class, then
// delegates and records ref->substrate for subsequent lifecycle ops.
func (o *Orchestrator) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	class := spec.ConfinementClass
	if class == "" {
		class = types.CC1
	}
	sub, err := o.substrateFor(ctx, class)
	if err != nil {
		return runner.Sandbox{}, err
	}
	sb, err := sub.CreateSandbox(ctx, spec)
	if err != nil {
		return runner.Sandbox{}, err
	}
	o.mu.Lock()
	o.byRef[sb.Ref] = sub
	o.mu.Unlock()
	return sb, nil
}

// substrateFor returns the first substrate that advertises the class. Fails
// closed when none can enforce it (never silently downgrade).
func (o *Orchestrator) substrateFor(ctx context.Context, class types.ConfinementClass) (substrate.Substrate, error) {
	var probeErrs []error
	for _, s := range o.substrates {
		cs, err := s.Classes(ctx)
		if err != nil {
			// A substrate that can't report capabilities can't be selected, but
			// RETAIN the cause so a real probe failure (e.g. "docker: info:
			// <daemon down>") is not masked by the generic message below. This is
			// diagnostic only — fail-closed selection semantics are unchanged.
			probeErrs = append(probeErrs, fmt.Errorf("%s: %w", s.Name(), err))
			continue
		}
		for _, c := range cs.Classes {
			if c == class {
				return s, nil
			}
		}
	}
	if len(probeErrs) > 0 {
		return nil, fmt.Errorf("orchestrator: no confinement substrate can enforce class %q on this host (substrate probe failed: %w)", class, errors.Join(probeErrs...))
	}
	return nil, fmt.Errorf("orchestrator: no confinement substrate can enforce class %q on this host", class)
}

// subForRef resolves the substrate that owns ref. After a control-plane restart
// the byRef map is empty; with a single substrate we fall back to it (its
// teardown reconstructs state from the run-id label, so crash recovery is
// preserved). With several substrates an untracked ref cannot be resolved.
func (o *Orchestrator) subForRef(ref string) (substrate.Substrate, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if s, ok := o.byRef[ref]; ok {
		return s, nil
	}
	if len(o.substrates) == 1 {
		return o.substrates[0], nil
	}
	return nil, fmt.Errorf("orchestrator: no substrate tracked for ref %q", ref)
}

func (o *Orchestrator) Exec(ctx context.Context, ref string, argv []string) (string, error) {
	s, err := o.subForRef(ref)
	if err != nil {
		return "", err
	}
	return s.Exec(ctx, ref, argv)
}

func (o *Orchestrator) Wait(ctx context.Context, ref string) (int, error) {
	s, err := o.subForRef(ref)
	if err != nil {
		return 0, err
	}
	return s.Wait(ctx, ref)
}

func (o *Orchestrator) Attach(ctx context.Context, ref string, opts runner.AttachOptions) (runner.Session, error) {
	s, err := o.subForRef(ref)
	if err != nil {
		return nil, err
	}
	return s.Attach(ctx, ref, opts)
}

func (o *Orchestrator) Status(ctx context.Context, ref string) (runner.Status, error) {
	s, err := o.subForRef(ref)
	if err != nil {
		return runner.Status{}, err
	}
	return s.Status(ctx, ref)
}

func (o *Orchestrator) AgentStatus(ctx context.Context, ref, agentExecID string) (runner.Status, error) {
	s, err := o.subForRef(ref)
	if err != nil {
		return runner.Status{}, err
	}
	return s.AgentStatus(ctx, ref, agentExecID)
}

func (o *Orchestrator) StopSandbox(ctx context.Context, ref string) error {
	s, err := o.subForRef(ref)
	if err != nil {
		return err
	}
	err = s.StopSandbox(ctx, ref)
	o.forget(ref)
	return err
}

func (o *Orchestrator) KillSandbox(ctx context.Context, ref string) error {
	s, err := o.subForRef(ref)
	if err != nil {
		return err
	}
	err = s.KillSandbox(ctx, ref)
	o.forget(ref)
	return err
}

func (o *Orchestrator) forget(ref string) {
	o.mu.Lock()
	delete(o.byRef, ref)
	o.mu.Unlock()
}
