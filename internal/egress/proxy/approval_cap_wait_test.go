// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestResolveWaitDoesNotRetryTheHostCap (B10-F4): past maxApprovalHosts, Resolve
// fails closed with no entry, no raise and no row. It used to report that as
// {apPending, uuid.Nil} — the SAME shape a raise still in flight has — so
// ResolveWait's concurrent-raise retry loop could not tell them apart and slept
// its whole budget (concurrentRaiseRetries * holdPollInterval = 5s) for every new
// host, holding a goroutine and a socket with no holdSem slot to bound it.
//
// That turns the fail-closed cap into a way for the sandbox to pin unbounded
// goroutines for 5 s each in a 256 MiB sidecar: the cap is reached by NAMING
// hosts, which the agent controls. A distinct state is the whole fix — evaluate's
// default arm already maps anything that is not approved/denied to Pending, so
// the wire answer is unchanged.
//
// Runs at the REAL holdPollInterval so "well under the retry budget" means the
// shipped 5 s, not a shrunken one.
func TestResolveWaitDoesNotRetryTheHostCap(t *testing.T) {
	// No CP stub: a capped Resolve must not make a network call either. A URL
	// nothing listens on makes that a failure rather than a silent pass.
	ap := newApprovalClient("http://127.0.0.1:1", newTokenSource("tok"), uuid.New(), nil)
	ap.configureHold(types.FirstUseWaitForReview, 30*time.Second, 16)

	// Saturate the per-run host table directly: the cap is a count, and 4096
	// real raises would test the CP stub rather than the cap.
	for i := 0; len(ap.hosts) < maxApprovalHosts; i++ {
		ap.hosts[fmt.Sprintf("h%d.test", i)] = &hostApproval{state: apApproved}
	}

	start := time.Now()
	res := ap.ResolveWait(context.Background(), "new.test")
	elapsed := time.Since(start)

	budget := concurrentRaiseRetries * holdPollInterval
	if elapsed >= holdPollInterval {
		t.Fatalf("ResolveWait over the host cap took %v (retry budget %v); the capped verdict is "+
			"indistinguishable from a raise in flight, so every new host parks a goroutine", elapsed, budget)
	}
	if res.State == apApproved || res.State == apDenied {
		t.Fatalf("capped ResolveWait = %v, want a fail-closed non-terminal state", res.State)
	}
}
