// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// residentClock sets the fixture server's database clock to the fixture's own plus off, or failing with err.
func residentClock(f *residentFix, off time.Duration, err error) {
	pg := f.srv.cfg.Store.(store.PG)
	pg.DBNow = func() (time.Time, error) { return f.clock().Add(off), err }
	f.srv.cfg.Store = pg
}

// A runner_resident delivery counts its value's bound on the database's clock, the one that stamps the run's cap and
// end: the one-minute floor takes a run whose end is more than a minute away on that clock and refuses one less than
// a minute away, whatever this server's clock says, and the 24-hour ceiling runs from the database's now. When that
// clock cannot be read this server's stands in: a bound, never a refusal. Catches: the floor or the ceiling counted on
// the server's clock, which a skewed replica moves by its skew.
func TestPG_ResidentDelivery_TheBoundIsTheDatabasesClock(t *testing.T) {
	for _, c := range []struct {
		name      string
		dbOff     time.Duration // the database's clock against the fixture server's
		clockErr  error
		endsIn    time.Duration // the run's end from the fixture server's clock; 0 is no end
		delivered bool
	}{
		{"an end 30s away on the server is 2m30s away on a database 2m behind", -2 * time.Minute, nil, 30 * time.Second, true},
		{"an end 2m30s away on the server is 30s away on a database 2m ahead", 2 * time.Minute, nil, 150 * time.Second, false},
		{"no end: the ceiling runs from the database's now", -2 * time.Hour, nil, 0, true},
		{"no end, the clock failing: the ceiling runs from the server's now", 0, errClockRead, 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newResidentFix(t, nil, residentEnvPolicy)
			residentClock(f, c.dbOff, c.clockErr)
			run := f.startingRun(t, func(r *types.AgentRun) {
				if c.endsIn > 0 {
					end := f.clock().Add(c.endsIn)
					r.EndsAt = &end
				}
			})
			got := f.srv.deliverResident(t.Context(), run, types.RunPolicySpec{}, []residentValue{f.value(t, run, "v")})
			delivered, _ := f.side.calls()
			if got != c.delivered || (len(delivered) == 1) != c.delivered {
				t.Fatalf("delivered = %v with %d values, want %v", got, len(delivered), c.delivered)
			}
			if !c.delivered {
				return
			}
			want := f.clock().Add(c.dbOff).Add(residentReadCeiling)
			if run.EndsAt != nil {
				want = *run.EndsAt
			}
			if !delivered[0].NotAfter.Equal(want) {
				t.Fatalf("not_after = %v, want %v", delivered[0].NotAfter, want)
			}
		})
	}
}

// A resident git_pat's proxy mint expires where its delivery's not_after will, counted from the database's now: with
// that clock 2 hours behind this server's and no cap or end, 24 hours after the database's now. Catches: the mint
// counted on the server's clock, which leaves the proxy answering past the runner's own bound on a replica ahead of
// the database.
func TestPG_ResidentMint_TheExpiryIsTheDatabasesClock(t *testing.T) {
	var c residentClassCase
	for _, rc := range residentClassCases {
		if rc.kind == types.GrantGitPAT {
			c = rc
		}
	}
	f := newResidentFix(t, nil, residentAllClassesPolicy)
	f.srv.cfg.DisableGitPATBroker = true
	f.reportCaps(t, true)
	if err := f.cfg.Secrets.(*memSecrets).Put(t.Context(), "op-secret", []byte("resident value\n")); err != nil {
		t.Fatal(err)
	}
	residentClock(f, -2*time.Hour, nil)
	run := f.startingRun(t, nil)
	g := f.classGrant(t, run, c, "op-secret")
	spec, _, ok := f.authorClass(t, run, c, false)
	want := f.clock().Add(-2 * time.Hour).Add(residentReadCeiling)
	if !ok || len(spec.ProxyConfig.ResidentMints) != 1 || spec.ProxyConfig.ResidentMints[0].GrantID != g.ID ||
		!spec.ProxyConfig.ResidentMints[0].ExpiresAt.Equal(want) {
		t.Fatalf("authored ok=%v mints %+v, want one for the grant expiring at %v", ok, spec.ProxyConfig.ResidentMints, want)
	}
}
