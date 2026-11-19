// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"
)

// adoClockedStore is the minted-PAT fixture's store on a fake database clock: DatabaseNow answers at, or fails with err.
type adoClockedStore struct {
	*adoPATStore
	at  time.Time
	err error
}

func (s adoClockedStore) DatabaseNow(context.Context) (time.Time, error) { return s.at, s.err }

// A minted token's life is counted from the database's clock, the one that stamps the run's end that caps it, both at
// dispatch and at a forced re-mint. When that clock cannot be read the server's stands in: the life is a bound, not a
// decision. Catches: a token's life moved by the replica's skew against the run's end.
func TestMintedPAT_TokenLifeIsCountedOnTheDatabaseClock(t *testing.T) {
	db := adoTestNow.Add(-2 * time.Hour)
	for _, tc := range []struct {
		name string
		err  error
		base time.Time
	}{
		{"the database's clock", nil, db},
		{"the server's clock when the database's cannot be read", errClockRead, adoTestNow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newADOPATLane(t)
			fx.srv.cfg.Store = adoClockedStore{adoPATStore: fx.st, at: db, err: tc.err}
			if !fx.dispatch(t) {
				t.Fatalf("dispatch refused: %q", fx.st.hint)
			}
			first := fx.ok(t, "dev.azure.com", nil)
			fx.ok(t, "dev.azure.com", stale(first.JTI))
			want := tc.base.Add(8 * time.Hour).UTC().Truncate(time.Second)
			if n := fx.pats.createCount(); n != 2 || !fx.pats.creates[0].ValidTo.Equal(want) || !fx.pats.creates[1].ValidTo.Equal(want) {
				t.Fatalf("%d creates %+v; want 2, each valid to %v", n, fx.pats.creates, want)
			}
		})
	}
}
