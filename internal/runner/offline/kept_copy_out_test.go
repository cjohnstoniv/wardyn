// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/identity"
)

// TestLapsedProxyStopKeepsTheSandboxHeld: a run's lapse while its runner is offline stops its proxy and never
// retires its sandbox, so the owner's copy-out door (the server's keptCopyOutRefusal) has an agent to open: the
// runner still answers an attach or exec for a held sandbox.
func TestLapsedProxyStopKeepsTheSandboxHeld(t *testing.T) {
	m, sub, elapsed, org := managerFixture(t)
	id := createLocal(t, m, org)
	ctx := context.Background()
	m.Offline(ctx)
	*elapsed = identity.RunTokenLapseAfter
	for pass := 1; pass <= 2; pass++ {
		if err := m.CheckLapses(ctx); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if sub.stops < 1 || sub.kills != 0 {
			t.Fatalf("pass %d: stops=%d kills=%d, want the proxy stopped and nothing killed", pass, sub.stops, sub.kills)
		}
		if !m.HoldsSandbox(id.String()) {
			t.Fatalf("pass %d: the lapsed run's sandbox is no longer held", pass)
		}
	}
}
