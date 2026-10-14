// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"
)

// The SCIM adapter must reproduce production's conservative dual-clock cutoff,
// including when an app clock issues sessions ahead of or behind PostgreSQL.
func TestSCIMRevocationAdapterClockParity(t *testing.T) {
	for _, clock := range []struct {
		name string
		skew time.Duration
	}{
		{"aligned", 0}, {"ahead", time.Minute}, {"behind", -time.Minute},
	} {
		for _, cut := range []string{"revoke", "browser cut"} {
			t.Run(clock.name+"/"+cut, func(t *testing.T) {
				e := newSCIMEnv(t)
				origin := time.Now()
				start := origin.UTC().Add(clock.skew)
				appClock := func() time.Time { return start.Add(time.Since(origin)) }
				e.rev.now = appClock
				for _, alias := range []string{"subject", "email"} {
					sub, email, principal := "clock-sub-"+alias, "clock-"+alias+"@corp.example", "clock-sub-"+alias
					if alias == "email" {
						principal = email
					}
					cookie := scimCookieAt(t, sub, email, 0, appClock())
					if !e.cookieWorks(e.a, cookie) {
						t.Fatalf("%s control cookie refused without a cutoff", alias)
					}
					time.Sleep(25 * time.Millisecond)
					var err error
					if cut == "revoke" {
						err = e.rev.RevokeSub(context.Background(), principal)
					} else {
						err = e.rev.CutSessions(context.Background(), principal)
					}
					if err != nil {
						t.Fatal(err)
					}
					if e.cookieWorks(e.a, cookie) || e.cookieWorks(e.b, cookie) {
						t.Fatalf("%s cookie minted before cutoff survived %s", alias, cut)
					}
				}
				bystander := scimCookieAt(t, "clock-bystander", "clock-bystander@corp.example", 0, appClock())
				if !e.cookieWorks(e.a, bystander) || !e.cookieWorks(e.b, bystander) {
					t.Fatal("unrelated session was revoked")
				}
			})
		}
	}
}

// A genuinely later timestamp remains live in every clock direction. The prior
// cutoff is independent of the short-boundary rollback probes above.
func TestSCIMRevocationClockAllowsLaterSession(t *testing.T) {
	for _, skew := range []time.Duration{-time.Minute, 0, time.Minute} {
		t.Run(skew.String(), func(t *testing.T) {
			e := newSCIMEnv(t)
			origin := time.Now()
			start := origin.UTC().Add(skew)
			clock := func() time.Time { return start.Add(time.Since(origin)) }
			e.rev.now = clock
			for _, table := range []string{"oidc_session_revocations", "oidc_session_cuts"} {
				column := "revoked_at"
				if table == "oidc_session_cuts" {
					column = "cut_at"
				}
				sub := "later-" + table
				if _, err := e.pool.Exec(context.Background(), "INSERT INTO "+table+" (sub,"+column+") VALUES ($1,clock_timestamp()-interval '2 minutes')", sub); err != nil {
					t.Fatal(err)
				}
				cookie := scimCookieAt(t, sub, "later@corp.example", 0, clock())
				if !e.cookieWorks(e.a, cookie) || !e.cookieWorks(e.b, cookie) {
					t.Fatalf("true later session refused: skew=%s table=%s", skew, table)
				}
			}
		})
	}
}
