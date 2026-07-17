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
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// capsCacheTTL bounds how long a substrate's ClassSupport is memoized. Host
// capabilities (installed runtimes) change only on daemon restart or a runtime
// install, but Capabilities()/substrateFor() were recomputing them on ~8 hot
// HTTP paths (incl. /healthz) — a full docker Info() round-trip each. A few
// seconds of staleness is harmless here and collapses the repeated round-trips
// to at most one per substrate per TTL. CreateSandbox keeps its own live
// provision-time Info() (driver-level), so nothing that needs fresh runtime
// data reads through this cache.
const capsCacheTTL = 5 * time.Second

// cachedClasses is a substrate's ClassSupport plus when it was fetched.
type cachedClasses struct {
	val substrate.ClassSupport
	at  time.Time
}

// RefStore is the durable ref->substrate mapping seam. Substrates are persisted
// by NAME (a substrate object is not serialisable); GetRef tolerates missing
// rows (found=false, nil error) so pre-migration/unknown refs are not errors.
// store.PG satisfies this interface.
type RefStore interface {
	PutRef(ctx context.Context, ref, substrateName string) error
	GetRef(ctx context.Context, ref string) (substrateName string, found bool, err error)
	DeleteRef(ctx context.Context, ref string) error
}

// Orchestrator implements runner.Runner over a set of Substrates.
type Orchestrator struct {
	substrates []substrate.Substrate

	// refStore, when non-nil, durably mirrors byRef so lifecycle routing (and
	// therefore the kill switch) survives a control-plane restart in
	// multi-substrate deployments. Nil = in-memory only (single-substrate
	// deployments don't need it: subForRef falls back to the sole substrate).
	refStore RefStore

	// mu guards byRef. The orchestrator is safe for concurrent use.
	mu sync.Mutex
	// byRef maps a sandbox ref to the substrate that created it, so lifecycle
	// ops (Exec/Wait/Attach/Status/Stop/Kill) route to the right substrate.
	byRef map[string]substrate.Substrate

	// capsMu guards classCache. It is SEPARATE from mu so a slow/wedged daemon
	// probe (held outside the lock, see classesFor) never blocks byRef routing or
	// the kill switch.
	capsMu     sync.Mutex
	classCache map[string]cachedClasses // substrate name -> memoized ClassSupport
	// now is the clock (overridable in tests to exercise TTL expiry).
	now func() time.Time
}

var _ runner.Runner = (*Orchestrator)(nil)

// New constructs an Orchestrator over the given substrates (at least one). With
// a single substrate it is a thin pass-through; with several it routes by class.
func New(substrates ...substrate.Substrate) *Orchestrator {
	return &Orchestrator{
		substrates: substrates,
		byRef:      make(map[string]substrate.Substrate),
		classCache: make(map[string]cachedClasses),
		now:        time.Now,
	}
}

// classesFor returns a substrate's ClassSupport, served from a short-TTL cache
// when fresh so repeated Capabilities()/substrateFor() calls collapse to at most
// one daemon round-trip per substrate per TTL. The daemon probe runs OUTSIDE
// capsMu (a wedged daemon must not block other cache readers or byRef routing),
// and errors are NEVER cached — an unhealthy substrate re-probes next call and
// fail-closed selection semantics are unchanged.
func (o *Orchestrator) classesFor(ctx context.Context, s substrate.Substrate) (substrate.ClassSupport, error) {
	name := s.Name()
	o.capsMu.Lock()
	if e, ok := o.classCache[name]; ok && o.now().Sub(e.at) < capsCacheTTL {
		o.capsMu.Unlock()
		return e.val, nil
	}
	o.capsMu.Unlock()

	cs, err := s.Classes(ctx)
	if err != nil {
		return substrate.ClassSupport{}, err
	}
	o.capsMu.Lock()
	o.classCache[name] = cachedClasses{val: cs, at: o.now()}
	o.capsMu.Unlock()
	return cs, nil
}

// WithRefStore wires a durable RefStore (chainable, call before serving
// traffic). Without it the orchestrator behaves exactly as before: in-memory
// routing plus the sole-substrate fallback.
func (o *Orchestrator) WithRefStore(rs RefStore) *Orchestrator {
	o.refStore = rs
	return o
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
		cs, err := o.classesFor(ctx, s)
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
	// Write-through BEFORE the sandbox is handed out: fail closed rather than
	// return a live sandbox the kill switch could not find after a restart.
	if o.refStore != nil {
		if perr := o.refStore.PutRef(ctx, sb.Ref, sub.Name()); perr != nil {
			o.forget(ctx, sb.Ref, false)
			_ = sub.KillSandbox(ctx, sb.Ref) // best-effort teardown of the untracked sandbox
			return runner.Sandbox{}, fmt.Errorf("orchestrator: persist ref %q -> %s: %w", sb.Ref, sub.Name(), perr)
		}
	}
	return sb, nil
}

// substrateFor returns the first substrate that advertises the class. Fails
// closed when none can enforce it (never silently downgrade).
func (o *Orchestrator) substrateFor(ctx context.Context, class types.ConfinementClass) (substrate.Substrate, error) {
	var probeErrs []error
	for _, s := range o.substrates {
		cs, err := o.classesFor(ctx, s)
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
// preserved). With several substrates an untracked ref is rehydrated from the
// RefStore by substrate name; only when that misses too (no store, no row, or a
// name no longer wired) does resolution fail.
func (o *Orchestrator) subForRef(ctx context.Context, ref string) (substrate.Substrate, error) {
	o.mu.Lock()
	if s, ok := o.byRef[ref]; ok {
		o.mu.Unlock()
		return s, nil
	}
	if len(o.substrates) == 1 {
		s := o.substrates[0]
		o.mu.Unlock()
		return s, nil
	}
	o.mu.Unlock()
	if o.refStore != nil {
		name, found, err := o.refStore.GetRef(ctx, ref)
		if err == nil && found {
			for _, s := range o.substrates {
				if s.Name() == name {
					o.mu.Lock()
					o.byRef[ref] = s
					o.mu.Unlock()
					return s, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("orchestrator: no substrate tracked for ref %q", ref)
}

func (o *Orchestrator) Exec(ctx context.Context, ref string, argv []string) (string, error) {
	s, err := o.subForRef(ctx, ref)
	if err != nil {
		return "", err
	}
	return s.Exec(ctx, ref, argv)
}

func (o *Orchestrator) Wait(ctx context.Context, ref string) (int, error) {
	s, err := o.subForRef(ctx, ref)
	if err != nil {
		return 0, err
	}
	return s.Wait(ctx, ref)
}

func (o *Orchestrator) Attach(ctx context.Context, ref string, opts runner.AttachOptions) (runner.Session, error) {
	s, err := o.subForRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	return s.Attach(ctx, ref, opts)
}

func (o *Orchestrator) Status(ctx context.Context, ref string) (runner.Status, error) {
	s, err := o.subForRef(ctx, ref)
	if err != nil {
		return runner.Status{}, err
	}
	return s.Status(ctx, ref)
}

func (o *Orchestrator) AgentStatus(ctx context.Context, ref, agentExecID string) (runner.Status, error) {
	s, err := o.subForRef(ctx, ref)
	if err != nil {
		return runner.Status{}, err
	}
	return s.AgentStatus(ctx, ref, agentExecID)
}

func (o *Orchestrator) StopSandbox(ctx context.Context, ref string) error {
	s, err := o.subForRef(ctx, ref)
	if err != nil {
		return err
	}
	err = s.StopSandbox(ctx, ref)
	o.forget(ctx, ref, err == nil)
	return err
}

func (o *Orchestrator) KillSandbox(ctx context.Context, ref string) error {
	s, err := o.subForRef(ctx, ref)
	if err != nil {
		return err
	}
	err = s.KillSandbox(ctx, ref)
	o.forget(ctx, ref, err == nil)
	return err
}

// forget drops the in-memory route and, when the sandbox was actually torn
// down, best-effort deletes the durable row so the table doesn't grow without
// bound. A delete error never fails a stop/kill: a stale row is garbage, not a
// routing hazard, and rehydrating it later is harmless.
func (o *Orchestrator) forget(ctx context.Context, ref string, tornDown bool) {
	o.mu.Lock()
	delete(o.byRef, ref)
	o.mu.Unlock()
	if tornDown && o.refStore != nil {
		_ = o.refStore.DeleteRef(ctx, ref)
	}
}
