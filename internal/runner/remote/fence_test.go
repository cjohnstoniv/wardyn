// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// A policy generation goes on the wire only to a runner whose caps carry ProxyFence: an older runner is sent
// the plain verbs and never sees the field.
func TestRemote_GenerationSentOnlyWithProxyFence(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	sb, _ := create(t, rig, nil)
	ctx := ctxT(t)
	cfg := []byte(`{"run_id":"r"}`)

	if err := rig.Sub.StopProxyAt(ctx, sb.Ref, 4); err != nil {
		t.Fatalf("StopProxyAt: %v", err)
	}
	if err := rig.Sub.EndSandboxAt(ctx, sb.Ref, 5); err != nil {
		t.Fatalf("EndSandboxAt: %v", err)
	}
	if _, err := rig.Sub.ReplaceProxyHashedAt(ctx, sb.Ref, cfg, 6); err != nil {
		t.Fatalf("ReplaceProxyHashedAt: %v", err)
	}
	want := map[string]int{"stop_proxy": 4, "end": 5, "replace_proxy": 6}
	if len(rig.Fake.PolicyGens) != 3 {
		t.Fatalf("a runner with ProxyFence saw %v; want the three generations", rig.Fake.PolicyGens)
	}
	for _, c := range rig.Fake.PolicyGens {
		if want[c.Verb] != c.Gen {
			t.Errorf("%s reached the runner at generation %d; want %d", c.Verb, c.Gen, want[c.Verb])
		}
	}

	// The same runner as one that predates ProxyFence: its caps carry no flag.
	rig.Sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: []byte(`{"support":{"Classes":["CC1"]},"version":"0.10.0"}`)})
	if err := rig.Sub.StopProxyAt(ctx, sb.Ref, 7); err != nil {
		t.Fatalf("StopProxyAt on an old runner: %v", err)
	}
	if err := rig.Sub.EndSandboxAt(ctx, sb.Ref, 8); err != nil {
		t.Fatalf("EndSandboxAt on an old runner: %v", err)
	}
	if _, err := rig.Sub.ReplaceProxyHashedAt(ctx, sb.Ref, cfg, 9); err != nil {
		t.Fatalf("ReplaceProxyHashedAt on an old runner: %v", err)
	}
	if len(rig.Fake.PolicyGens) != 3 {
		t.Errorf("a runner without ProxyFence was sent a generation: %v", rig.Fake.PolicyGens)
	}
}

// A fence is an observation of the runner, not an action to replay: offline it is refused, and nothing is queued.
func TestRemote_FenceOfflineIsNotQueued(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	sb, _ := create(t, rig, nil)
	ctx := ctxT(t)
	if err := rig.Sub.FenceProxy(ctx, sb.Ref); err != nil {
		t.Fatalf("online FenceProxy = %v; want nil", err)
	}
	rig.Org.Drop()
	waitOffline(t, rig)
	if err := rig.Sub.FenceProxy(ctx, sb.Ref); !errors.Is(err, runner.ErrRunnerOffline) {
		t.Fatalf("offline FenceProxy = %v; want runner.ErrRunnerOffline", err)
	}
	if got := rig.Queued(); len(got) != 0 {
		t.Errorf("offline FenceProxy queued %v; want nothing", got)
	}
}
