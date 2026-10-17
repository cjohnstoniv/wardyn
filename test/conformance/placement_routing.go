// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RoutingOptions configures RunPlacementRouting.
type RoutingOptions struct {
	// Timeout bounds each subtest's calls; zero means 30 seconds.
	Timeout time.Duration
	// Image is the OCI image a honoured placement creates.
	Image string
	// Offline, when set, is a registered runner substrate whose link is down.
	Offline substrate.Substrate
}

// countingSubstrate counts the creates that reach the substrate it wraps, so a
// refused placement is proved by zero calls and not by an error alone.
type countingSubstrate struct {
	substrate.Substrate
	creates atomic.Int32
}

func (c *countingSubstrate) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	c.creates.Add(1)
	return c.Substrate.CreateSandbox(ctx, spec)
}

// RunPlacementRouting proves, against a real substrate, that a placement is
// honoured or refused and never substituted: asked for a runner that is not
// there or a class nobody advertises, the orchestrator refuses naming what was
// asked and local sees no create.
func RunPlacementRouting(t *testing.T, local substrate.Substrate, opts RoutingOptions) {
	t.Helper()
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	counting := &countingSubstrate{Substrate: local}
	orch := orchestrator.New(counting)
	classes := func(t *testing.T) []types.ConfinementClass {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		cs, err := local.Classes(ctx)
		if err != nil {
			t.Fatalf("Classes: %v", err)
		}
		return cs.Classes
	}
	create := func(t *testing.T, class types.ConfinementClass, runnerID string) (runner.Sandbox, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		spec := minimalSpec(opts.Image)
		spec.ConfinementClass = class
		spec.RunnerID = runnerID
		return orch.CreateSandbox(ctx, spec)
	}

	t.Run("UnregisteredRunnerIsRefusedNeverSubstituted", func(t *testing.T) {
		before := counting.creates.Load()
		id := uuid.NewString()
		_, err := create(t, types.CC1, id)
		if !errors.Is(err, runner.ErrRunnerOffline) {
			t.Fatalf("create for an unregistered runner = %v, want ErrRunnerOffline", err)
		}
		if !strings.Contains(err.Error(), id) {
			t.Errorf("refusal %q does not name runner %s", err, id)
		}
		if n := counting.creates.Load() - before; n != 0 {
			t.Fatalf("%s saw %d create(s) for a runner that is not registered, want 0", local.Name(), n)
		}
	})

	t.Run("UnadvertisedClassIsRefusedNeverDowngraded", func(t *testing.T) {
		have := classes(t)
		var missing types.ConfinementClass
		for _, c := range []types.ConfinementClass{types.CC3, types.CC2, types.CC1} {
			if !slices.Contains(have, c) {
				missing = c
				break
			}
		}
		if missing == "" {
			t.Skipf("%s advertises every class (%v); nothing unadvertised to ask for", local.Name(), have)
		}
		before := counting.creates.Load()
		_, err := create(t, missing, "")
		if err == nil {
			t.Fatalf("create at unadvertised class %s succeeded on %s", missing, local.Name())
		}
		if !strings.Contains(err.Error(), string(missing)) {
			t.Errorf("refusal %q does not name class %s", err, missing)
		}
		if n := counting.creates.Load() - before; n != 0 {
			t.Fatalf("%s saw %d create(s) for class %s it does not advertise, want 0", local.Name(), n, missing)
		}
	})

	t.Run("AskedPlacementIsHonoured", func(t *testing.T) {
		have := classes(t)
		if len(have) == 0 {
			t.Skipf("%s advertises no class", local.Name())
		}
		want := have[len(have)-1]
		before := counting.creates.Load()
		sb, err := create(t, want, "")
		if err != nil {
			t.Fatalf("create at advertised class %s: %v", want, err)
		}
		t.Cleanup(func() { _ = orch.KillSandbox(context.Background(), sb.Ref) })
		if n := counting.creates.Load() - before; n != 1 {
			t.Errorf("%s saw %d create(s) for a placement it can serve, want 1", local.Name(), n)
		}
		if sb.EnforcedClass.Rank() < want.Rank() {
			t.Errorf("asked %s, enforced %s: downgraded", want, sb.EnforcedClass)
		}
	})

	t.Run("OfflineRunnerIsRefusedNotRoutedLocally", func(t *testing.T) {
		if opts.Offline == nil {
			t.Skip("no offline runner substrate supplied")
		}
		orch.Add(opts.Offline)
		before := counting.creates.Load()
		_, err := create(t, types.CC1, strings.TrimPrefix(opts.Offline.Name(), substrate.RemotePrefix))
		if !errors.Is(err, runner.ErrRunnerOffline) {
			t.Fatalf("create for an offline runner = %v, want ErrRunnerOffline", err)
		}
		if n := counting.creates.Load() - before; n != 0 {
			t.Fatalf("%s saw %d create(s) for an offline runner's placement, want 0", local.Name(), n)
		}
	})
}
