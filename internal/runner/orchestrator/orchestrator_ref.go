// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
)

// errRefUntracked is subForRef's answer for a ref no substrate is tracked for, after every fallback missed.
var errRefUntracked = errors.New("orchestrator: no substrate tracked for ref")

// subForRef resolves the substrate that owns ref. After a control-plane restart
// the byRef map is empty; with a single local substrate we fall back to it (its
// teardown reconstructs state from the run-id label, so crash recovery is
// preserved). With several substrates an untracked ref is rehydrated from the
// RefStore by substrate name; only when that misses too (no store, no row, or a
// name no longer wired) does resolution fail. A runner's ref is routed by the
// runner id it carries, never by either fallback.
func (o *Orchestrator) subForRef(ctx context.Context, ref string) (substrate.Substrate, error) {
	o.mu.Lock()
	if s, ok := o.byRef[ref]; ok {
		o.mu.Unlock()
		return s, nil
	}
	o.mu.Unlock()
	// A runner's ref names its runner, so it never meets the single-substrate
	// fallback below: it routes to that runner or is offline.
	if name, ok := remoteNameOfRef(ref); ok {
		s, ok := o.runnerSubstrate(name)
		if !ok {
			return nil, fmt.Errorf("%w: no registered substrate %s for ref %q", runner.ErrRunnerOffline, name, ref)
		}
		o.mu.Lock()
		o.byRef[ref] = s
		o.mu.Unlock()
		return s, nil
	}
	if locals := o.locals(); len(locals) == 1 {
		return locals[0], nil
	}
	if o.refStore != nil {
		name, found, err := o.refStore.GetRef(ctx, ref)
		if err != nil {
			// The lookup failed: the owner is unknown, not absent, so this is not errRefUntracked.
			return nil, fmt.Errorf("orchestrator: no substrate tracked for ref %q: %w", ref, err)
		}
		if found {
			for _, s := range o.snapshot() {
				if s.Name() == name {
					o.mu.Lock()
					o.byRef[ref] = s
					o.mu.Unlock()
					return s, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("%w %q", errRefUntracked, ref)
}
