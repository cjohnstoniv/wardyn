// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

// A peer that ends while the runner waits for the org's PENDING says why. Its end also cancels the wait,
// and that cancellation must never stand in for the GOAWAY or REVOKED that ended it: Run acts on those.
func TestFreshReturnsThePeersReasonNotItsCancellation(t *testing.T) {
	for _, reason := range []error{fmt.Errorf("%w: runner_unclaimed", ErrGoaway), ErrRevoked} {
		for range 2000 {
			_, run := Loopback()
			peer := NewPeer(run, PeerConfig{})
			peer.fail(reason)
			var ready atomic.Bool
			if err := (&Client{}).fresh(t.Context(), peer, make(chan []PendingAction), &ready); !errors.Is(err, reason) {
				t.Fatalf("fresh after the peer ended with %v returned %v", reason, err)
			}
		}
	}
}
