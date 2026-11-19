// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A read that fails in the tail owner's give-up reads is logged as the forced stop retry's when the forced retry made
// it (its give-up past the keep, under forcedCtx), and as the tail owner's otherwise: inside the keep the row is
// skipped for the tick, past it the record is given up unread. Catches: a log line that sends an operator to the tail
// owner for a read the forced retry made.
func TestTailOwner_AFailedReadIsLoggedAsTheForcedRetrysWhenItMadeTheRead(t *testing.T) {
	const (
		forcedSkip = "the forced stop retry could not read an ended run's folder state"
		tailSkip   = "the tail owner could not read an ended run's folder state"
		forcedPast = "the forced stop retry could not read an ended run's folder record past its keep"
		tailPast   = "the tail owner could not read an ended run's folder record past its keep"
	)
	for _, keep := range []struct {
		name         string
		past         bool
		forced, tail string
	}{
		{"inside the keep", false, forcedSkip, tailSkip},
		{"past the keep", true, forcedPast, tailPast},
	} {
		for _, by := range []struct {
			name   string
			forced bool
		}{
			{"the forced retry", true},
			{"the tail owner", false},
		} {
			t.Run(keep.name+"/"+by.name, func(t *testing.T) {
				g := newTailRig(t)
				ended := time.Hour
				if keep.past {
					ended = g.s.pendingCap() + time.Hour
				}
				row := g.owedRow(g.clock.now.Add(-ended))
				g.lab.statusErr = errors.New("the record does not decode")
				ctx := context.Background()
				if by.forced {
					ctx = forcedCtx(ctx)
				}
				logs := tailTestLogs(t)
				g.s.tailGiveUpOwed(ctx, row.run.ID)
				want, not := keep.tail, keep.forced
				if by.forced {
					want, not = keep.forced, keep.tail
				}
				if out := logs.String(); !strings.Contains(out, want) || strings.Contains(out, not) {
					t.Errorf("logs %q; want %q and not %q", out, want, not)
				}
			})
		}
	}
}
