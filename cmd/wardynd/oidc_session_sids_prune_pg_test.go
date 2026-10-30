// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"
)

// The reaper tick prunes ended front-channel logout sid rows only with WARDYN_OIDC_FRONTCHANNEL_LOGOUT
// on: off, nothing is recorded and a split-role install may lack the 0229 grant, so the tick must
// not touch the table at all.
func TestReapTickPrunesOIDCSessionSIDsOnlyWithFrontchannelLogoutOn(t *testing.T) {
	pool := pgPool(t) // skips without WARDYN_TEST_PG; migrates
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	count := func() (n int) {
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM oidc_session_sids WHERE issuer = 'https://reap-tick.example'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	tick := func(on bool) {
		for ctx.Err() == nil {
			if release, ok := reapTickLock(pool, on)(ctx); ok {
				release()
				return
			}
			time.Sleep(50 * time.Millisecond) // another test holds the reaper key for a moment
		}
		t.Fatal("the reaper tick never won its lock")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO oidc_session_sids (issuer, sid, sub, expires_at) VALUES ('https://reap-tick.example', 'ended', 'p', now() - interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM oidc_session_sids WHERE issuer = 'https://reap-tick.example'`)
	})

	if tick(false); count() != 1 {
		t.Fatal("a tick with front-channel logout off pruned the sid table; want it untouched")
	}
	if tick(true); count() != 0 {
		t.Fatal("a tick with front-channel logout on left an ended sid row; want it pruned")
	}
}
