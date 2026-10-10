// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A pool wait must not move the translated issuance past an existing cutoff.
// The same fast app clock stamps the cookie and samples its age.
func TestPG_RevocationBeforeCookiePoolWaitDoesNotLoseCutoff(t *testing.T) {
	migrationPool := revocationPool(t)
	cfg, err := pgxpool.ParseConfig(os.Getenv("WARDYN_TEST_PG"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var armed atomic.Bool
	sampled := make(chan time.Time, 1)
	appOrigin := time.Now()
	appStart := appOrigin.UTC().Add(time.Minute)
	appClock := func() time.Time {
		at := appStart.Add(time.Since(appOrigin))
		if armed.Load() {
			select {
			case sampled <- at:
			default:
			}
		}
		return at
	}
	rev := &pgSessionRevocations{pool: pool, now: appClock}
	sub := "pool-delay-" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = migrationPool.Exec(context.Background(), `DELETE FROM oidc_session_revocations WHERE sub=$1`, sub)
	})
	issued := appClock()
	if status, err := rev.SessionStatus(ctx, sub, "", issued, 0); err != nil || status != oidc.SessionLive {
		t.Fatalf("pre-cut control status=%d err=%v", status, err)
	}
	time.Sleep(25 * time.Millisecond)
	if err := rev.RevokeSub(ctx, sub); err != nil {
		t.Fatal(err)
	}
	held, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	armed.Store(true)
	type response struct {
		status oidc.SessionStatus
		err    error
	}
	result := make(chan response, 1)
	go func() { status, err := rev.SessionStatus(ctx, sub, "", issued, 0); result <- response{status, err} }()
	// Old code samples before the wait; repaired code samples after Scan.
	// Release on the same bounded timer in either case; the assertion is unchanged.
	timer := time.NewTimer(200 * time.Millisecond)
	var sampledAt time.Time
	select {
	case sampledAt = <-sampled:
		<-timer.C
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	held.Release()
	var got response
	select {
	case got = <-result:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if sampledAt.IsZero() {
		select {
		case sampledAt = <-sampled:
		default:
			t.Fatal("app clock was not sampled after receipt")
		}
	}
	var cutoff, pgNow time.Time
	if err := pool.QueryRow(ctx, `SELECT revoked_at,clock_timestamp() FROM oidc_session_revocations WHERE sub=$1`, sub).Scan(&cutoff, &pgNow); err != nil {
		t.Fatal(err)
	}
	t.Logf("POOL issued=%s app_age_sample=%s cutoff=%s pg_now=%s held_connection=200ms acquires_without_idle=%d actual_status=%d", issued.Format(time.RFC3339Nano), sampledAt.Format(time.RFC3339Nano), cutoff.Format(time.RFC3339Nano), pgNow.Format(time.RFC3339Nano), pool.Stat().EmptyAcquireCount(), got.status)
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.status != oidc.SessionRevoked {
		t.Fatalf("cookie minted before revoke survived actual pool wait: status=%d", got.status)
	}
}
