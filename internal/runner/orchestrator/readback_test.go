// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// hashingSubstrate is a revivingSubstrate that answers the hash of each config it is handed.
type hashingSubstrate struct{ *revivingSubstrate }

func (h *hashingSubstrate) ReplaceProxyHashed(ctx context.Context, ref string, cfg []byte) (string, error) {
	return "sha-of-" + string(cfg), h.ReplaceProxy(ctx, ref, cfg)
}

// The orchestrator is a ProxyReviverHashed: a substrate that hashes answers its hash through it,
// and one that cannot is refused before anything changes, never answered with no hash.
func TestOrchestrator_ProxyReviverHashed(t *testing.T) {
	ctx := context.Background()
	hashing := &hashingSubstrate{&revivingSubstrate{fakeSubstrate: &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}}}}
	var r runner.Runner = New(hashing)
	rv, ok := r.(runner.ProxyReviverHashed)
	if !ok {
		t.Fatal("the orchestrator is not a runner.ProxyReviverHashed")
	}
	if sum, err := rv.ReplaceProxyHashed(ctx, "wardyn-agent-x", []byte("cfg")); err != nil || sum != "sha-of-cfg" || len(hashing.replaced) != 1 {
		t.Fatalf("ReplaceProxyHashed = %q, %v; replaced %v", sum, err, hashing.replaced)
	}

	plain := &revivingSubstrate{fakeSubstrate: &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}}}
	if sum, err := New(plain).ReplaceProxyHashed(ctx, "wardyn-agent-x", []byte("cfg")); !errors.Is(err, runner.ErrReviveUnsupported) || sum != "" || len(plain.replaced) != 0 {
		t.Fatalf("a substrate that cannot hash: %q, %v, replaced %v; want ErrReviveUnsupported and nothing replaced", sum, err, plain.replaced)
	}
}
