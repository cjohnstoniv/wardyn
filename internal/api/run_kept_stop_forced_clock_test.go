// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The forced end's hold-off is counted on the database's clock (the forced claim's own statement), so this
// server's clock, ahead of it or behind it, changes no decision: a kept run 1 minute inside the hold-off is
// not forced and one 1 minute past it is, whatever the server's clock says. Before, the server's own clock
// decided: +2 minutes forced the run inside the hold-off, and -2 minutes left the one past it unforced.
func TestStopKeptRun_TheHoldOffIsTheDatabaseClocksNotTheServers(t *testing.T) {
	cases := []struct {
		name   string
		after  time.Duration // the database's time past the run's due end
		forced bool
	}{
		{"1 minute inside the hold-off", keptEndHoldOffLimit - time.Minute, false},
		{"1 minute past the hold-off", keptEndHoldOffLimit + time.Minute, true},
	}
	for _, skew := range []time.Duration{0, 2 * time.Minute, -2 * time.Minute} {
		for _, c := range cases {
			t.Run(fmt.Sprintf("%s, server clock %+v off", c.name, skew), func(t *testing.T) {
				f := newForced(t)
				f.withoutOwedPass()
				f.s.cfg.Now = func() time.Time { return f.clock.now.Add(skew) } // the database's clock is f.clock
				f.clock.now = f.due.Add(c.after)
				f.end(f.s)
				if got := f.state() == types.RunStopped; got != c.forced {
					t.Errorf("forced = %v (state %s), want %v", got, f.state(), c.forced)
				}
				if n := len(forcedRows(t, f.audit, "run.ended.expired")); (n == 1) != c.forced || n > 1 {
					t.Errorf("%d forced rows, want forced = %v", n, c.forced)
				}
			})
		}
	}
}
