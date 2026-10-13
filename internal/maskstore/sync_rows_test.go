// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package maskstore

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
)

// One lasting value held by more than one committed row, which is what a key
// generation move leaves: the row identity includes the generation, so the
// same bytes registered again are a second row. Each test here applies reads
// as a replica would and asks the two maskers the consumers use.

// rowsValue has a quote in it, so its JSON rendering differs from its bytes.
const rowsValue = `lasting-"refresh"-token`

type rowsFixture struct {
	*w4CovFixture
	t    *testing.T
	key2 []byte
}

func newRowsFixture(t *testing.T) *rowsFixture {
	f := w4CovNewFixture(t)
	return &rowsFixture{w4CovFixture: f, t: t, key2: f.keys.put(w4CovOwner, 2, 7)}
}

// row is a live credential row of rowsValue under key generation version:
// current, or retired at retired.
func (f *rowsFixture) row(id uuid.UUID, version int, name string, gen int64, until, retired *time.Time) row {
	f.t.Helper()
	key := f.key
	if version == 2 {
		key = f.key2
	}
	blob, err := kek.Seal(key, []byte(rowsValue), aad(bucketGlobal, id, w4CovOwner, name, version))
	if err != nil {
		f.t.Fatalf("seal: %v", err)
	}
	return row{id: id, bucket: bucketGlobal, owner: w4CovOwner, name: name, version: &version, sealed: blob, until: until, retiredAt: retired, gen: gen}
}

func rowsTomb(id uuid.UUID, gen int64, retired *time.Time) row {
	return row{id: id, bucket: bucketGlobal, tombstone: true, retiredAt: retired, gen: gen}
}

func (f *rowsFixture) read(full bool, rows ...row) {
	f.t.Helper()
	if err := f.s.apply(context.Background(), rows, 100, full); err != nil {
		f.t.Fatalf("apply: %v", err)
	}
}

// wantMasked checks the plain masker (terminal output, recordings) and the
// JSON one (audit event data) together.
func (f *rowsFixture) wantMasked(when string, want bool) {
	f.t.Helper()
	plain := f.reg.Masker(w4CovRun).Mask([]byte("out " + rowsValue + " end"))
	quoted, _ := json.Marshal(rowsValue)
	inJSON := f.reg.JSONVariantMasker(w4CovRun).Mask(quoted)
	if got := !bytes.Contains(plain, []byte(rowsValue)); got != want {
		f.t.Errorf("%s: masked in plain output = %v, want %v", when, got, want)
	}
	if got := !bytes.Contains(inJSON, quoted[1:len(quoted)-1]); got != want {
		f.t.Errorf("%s: masked in JSON data = %v, want %v", when, got, want)
	}
}

// wantGone checks that the store let go of row id: no ref, and the bytes the
// ref held cleared.
func (f *rowsFixture) wantGone(when string, id uuid.UUID, held []byte) {
	f.t.Helper()
	if f.ref(id) != nil {
		f.t.Errorf("%s: the row's ref is still tracked", when)
	}
	if !bytes.Equal(held, make([]byte, len(held))) {
		f.t.Errorf("%s: the dropped ref's value bytes were not cleared", when)
	}
}

func rowsOrders(a, b row) map[string][]row {
	return map[string][]row{"new row first": {a, b}, "old row first": {b, a}}
}

// The generation move itself: the old row is retired in the commit that writes
// the new one, both at one generation, so a read returns them in either order.
// Sweeping the old row must not take the value the new row still holds, and the
// registration check a later refresh makes must still find it.
func TestRows_AGenerationMoveKeepsTheUnchangedValueMasked(t *testing.T) {
	t0 := w4CovT0
	for name, order := range rowsOrders(rowsNew(nil), rowsOld(nil, &t0)) {
		t.Run(name, func(t *testing.T) {
			f := newRowsFixture(t)
			f.read(false, f.row(w4CovRowA, 1, "cred", 1, nil, nil))
			held := f.ref(w4CovRowA).value
			f.read(false, f.resealed(order)...)
			f.wantMasked("after the move", true)

			f.reg.SweepGlobals(t0.Add(time.Second))
			f.wantMasked("after a replica's own sweep past the old row's grace", true)
			f.read(false, rowsTomb(w4CovRowA, 3, nil))
			f.wantMasked("after the old row's tombstone", true)
			f.wantGone("the swept old row", w4CovRowA, held)
			if f.ref(w4CovRowB) == nil {
				t.Error("the new row's ref was dropped with the old row's")
			}
			if !f.reg.HasGlobalValues(w4CovOwner, "cred", []secretmask.GlobalPut{{Value: []byte(rowsValue)}}) {
				t.Error("the value's next unchanged registration would be refused as not available for masking")
			}
		})
	}
}

// Both rows retired, at different times: the value is let go at the later
// retirement's grace, not the earlier one's, and then nothing of it is kept.
func TestRows_TheLaterRetirementDecidesWhenTheValueIsLetGo(t *testing.T) {
	t0, t1 := w4CovT0, w4CovT0.Add(30*time.Minute)
	for name, order := range rowsOrders(rowsNew(&t1), rowsOld(nil, &t0)) {
		t.Run(name, func(t *testing.T) {
			f := newRowsFixture(t)
			f.read(false, f.resealed(order)...)
			oldHeld, newHeld := f.ref(w4CovRowA).value, f.ref(w4CovRowB).value

			f.reg.SweepGlobals(t0.Add(time.Second))
			f.read(false, rowsTomb(w4CovRowA, 3, nil))
			f.wantMasked("past the earlier retirement's grace", true)
			f.wantGone("the earlier row", w4CovRowA, oldHeld)

			f.reg.SweepGlobals(t1.Add(time.Second))
			f.read(false, rowsTomb(w4CovRowB, 4, nil))
			f.wantMasked("past the later retirement's grace", false)
			f.wantGone("the later row", w4CovRowB, newHeld)
		})
	}
}

// A row with an expiry beside a row with none: the value does not expire.
func TestRows_ARowWithNoExpiryOutlastsOneThatExpires(t *testing.T) {
	expiry := w4CovT0.Add(10 * time.Minute)
	for name, order := range rowsOrders(rowsNew(nil), rowsOld(&expiry, nil)) {
		t.Run(name, func(t *testing.T) {
			f := newRowsFixture(t)
			f.read(false, f.resealed(order)...)
			f.reg.SweepGlobals(expiry.Add(time.Second))
			f.wantMasked("after a replica's own sweep past the expiry", true)
			f.read(false, rowsTomb(w4CovRowA, 3, nil))
			f.wantMasked("after the expired row's tombstone", true)
		})
	}
}

// A third row under another credential name, and an eviction: each row ends on
// its own, and an evicted row's grace outlasts a later drop of the same bytes.
func TestRows_AThirdRegistrationAndAnEvictionEachEndOnTheirOwn(t *testing.T) {
	f := newRowsFixture(t)
	t0, t1 := w4CovT0, w4CovT0.Add(30*time.Minute)
	f.read(false, f.row(w4CovRowA, 1, "cred", 1, nil, &t0), f.row(w4CovRowB, 2, "cred", 1, nil, nil), f.row(w4CovRowC, 1, "other", 2, nil, nil))
	evictedHeld := f.ref(w4CovRowB).value

	f.read(false, rowsTomb(w4CovRowA, 3, nil))
	f.wantMasked("after the retired row is swept", true)
	f.read(false, rowsTomb(w4CovRowB, 4, &t1))
	f.wantMasked("after the credential is evicted", true)
	f.wantGone("the evicted row", w4CovRowB, evictedHeld)
	f.read(false, rowsTomb(w4CovRowC, 5, nil))
	f.wantMasked("inside the evicted row's grace, after the other credential's row is dropped", true)
	f.reg.SweepGlobals(t1.Add(time.Second))
	f.wantMasked("past the evicted row's grace", false)
}

// A full reload drops the rows the table no longer has, one row at a time.
func TestRows_AFullReloadDropsOnlyTheRowsThatAreGone(t *testing.T) {
	f := newRowsFixture(t)
	t0 := w4CovT0
	f.read(false, f.row(w4CovRowA, 1, "cred", 1, nil, &t0), f.row(w4CovRowB, 2, "cred", 1, nil, nil))
	oldHeld, newHeld := f.ref(w4CovRowA).value, f.ref(w4CovRowB).value

	f.read(true, f.row(w4CovRowB, 2, "cred", 1, nil, nil))
	f.wantMasked("after a reload that no longer has the old row", true)
	f.wantGone("the old row", w4CovRowA, oldHeld)
	f.read(true)
	f.wantMasked("after a reload that has neither row", false)
	f.wantGone("the new row", w4CovRowB, newHeld)
}

// rowsOld and rowsNew describe the generation-1 row (w4CovRowA) and the
// generation-2 row (w4CovRowB) of credential "cred"; resealed seals them for a
// fixture.
func rowsOld(until, retired *time.Time) row {
	return row{id: w4CovRowA, until: until, retiredAt: retired}
}

func rowsNew(retired *time.Time) row { return row{id: w4CovRowB, retiredAt: retired} }

func (f *rowsFixture) resealed(order []row) []row {
	out := make([]row, 0, len(order))
	for _, r := range order {
		version := 1
		if r.id == w4CovRowB {
			version = 2
		}
		out = append(out, f.row(r.id, version, "cred", 2, r.until, r.retiredAt))
	}
	return out
}
