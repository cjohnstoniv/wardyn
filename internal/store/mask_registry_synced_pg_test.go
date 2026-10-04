// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// The mask_registry_shared /setup/status row asks Store.Synced. It must be nil
// while the replica's listener is up and its cursor has reached the committed
// generation, and an error as soon as the replica's Postgres connection is cut.

import (
	"context"
	"testing"
	"time"
)

func TestPG_MaskRegistry_SyncedFollowsTheListenerAndTheConnection(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	k := localKEK(t)
	writer := newRegReplica(t, pool, k)

	// B has its own pool, so closing it cuts only B's connection.
	bp := pgxpoolCopy(t, pool)
	b := newRegReplica(t, bp, k)

	// Before Start there is no listener: the replica cannot vouch for its list.
	if err := b.st.Synced(t.Context()); err == nil {
		t.Fatal("Synced was nil on a replica that is not listening")
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b.st.Start(ctx)
	waitFor(t, "B's listener to come up and B to be synced", func() bool { return b.st.Synced(ctx) == nil })

	// A commit A made a moment ago does not fail the check: Synced reads the
	// table itself before comparing, so it is "within one sync", not racing the
	// notification.
	if err := writer.reg.AddGlobal(regAlice, "fresh", time.Now(), []byte("a-value-committed-just-now")); err != nil {
		t.Fatal(err)
	}
	if err := b.st.Synced(ctx); err != nil {
		t.Fatalf("Synced failed right after another replica's commit: %v", err)
	}

	// Cut B's connection to Postgres: the check fails.
	bp.Close()
	if err := b.st.Synced(ctx); err == nil {
		t.Fatal("Synced was nil with the replica's Postgres connection cut")
	}
}
