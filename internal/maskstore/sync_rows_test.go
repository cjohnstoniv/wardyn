// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package maskstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
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

// An eviction tombstone for a row this replica already holds as retired leaves
// the row the grace it has: the value is let go at the row's own retirement.
func TestRows_AnEvictionTombstoneLeavesARetiredRowItsOwnGrace(t *testing.T) {
	f := newRowsFixture(t)
	t0, t1 := w4CovT0, w4CovT0.Add(30*time.Minute)
	f.read(false, f.row(w4CovRowA, 1, "cred", 1, nil, &t0))
	held := f.ref(w4CovRowA).value
	f.read(false, rowsTomb(w4CovRowA, 2, &t1))
	f.wantMasked("after the eviction tombstone", true)
	f.wantGone("the evicted row", w4CovRowA, held)
	f.reg.SweepGlobals(t0.Add(time.Second))
	f.wantMasked("past the row's own retirement", false)
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

// modelRow is what one replica should hold of one row it applied and has not
// yet let go.
type modelRow struct {
	hasRef    bool // the store tracks a ref
	inReg     bool // the registry holds the row
	until     time.Time
	retiredAt time.Time
}

// Seeded histories of three rows of one value (two key generations of "cred",
// one of "other"): delta reads, full reloads in any row order, tombstones,
// eviction tombstones and the replica's own sweeps, on a simulated clock. After
// every step the value is masked exactly while the model says a row holds it.
func TestRows_RandomRowHistoriesMatchAPerRowModel(t *testing.T) {
	ids := []uuid.UUID{w4CovRowA, w4CovRowB, w4CovRowC}
	version := map[uuid.UUID]int{w4CovRowA: 1, w4CovRowB: 2, w4CovRowC: 1}
	name := map[uuid.UUID]string{w4CovRowA: "cred", w4CovRowB: "cred", w4CovRowC: "other"}
	for seed := int64(1); seed <= 400; seed++ {
		rng := rand.New(rand.NewSource(seed))
		f := newRowsFixture(t)
		model := map[uuid.UUID]*modelRow{}
		var trace []string
		clock := w4CovT0
		gen := int64(1)
		check := func(step string) {
			want := false
			for _, m := range model {
				want = want || m.inReg
			}
			trace = append(trace, step)
			before := t.Failed()
			f.wantMasked(fmt.Sprintf("seed %d after %v", seed, trace), want)
			for id, m := range model {
				if (f.ref(id) != nil) != m.hasRef {
					t.Errorf("seed %d after %v: ref tracked for %s = %v, model says %v", seed, trace, id, f.ref(id) != nil, m.hasRef)
				}
			}
			if !before && t.Failed() {
				t.FailNow()
			}
		}
		live := func(id uuid.UUID, until, retired time.Time) row {
			var u, r *time.Time
			if !until.IsZero() {
				u = &until
			}
			if !retired.IsZero() {
				r = &retired
			}
			gen++
			return f.row(id, version[id], name[id], gen, u, r)
		}
		for step := 0; step < 14; step++ {
			clock = clock.Add(time.Duration(1+rng.Intn(20)) * time.Minute)
			id := ids[rng.Intn(3)]
			m := model[id]
			switch op := rng.Intn(8); {
			case op <= 1: // the row is (re)written current, with or without an expiry
				var until time.Time
				if rng.Intn(3) == 0 {
					until = clock.Add(time.Duration(rng.Intn(40)) * time.Minute)
				}
				f.read(false, live(id, until, time.Time{}))
				model[id] = &modelRow{hasRef: true, inReg: true, until: until}
				check(fmt.Sprintf("current(%s)", id.String()[35:]))
			case op == 2: // the row is retired in place
				if m == nil || !m.hasRef {
					continue
				}
				f.read(false, live(id, m.until, clock))
				model[id] = &modelRow{hasRef: true, inReg: true, until: m.until, retiredAt: clock}
				check(fmt.Sprintf("retired(%s)", id.String()[35:]))
			case op == 3: // a sweep or prune tombstone
				gen++
				f.read(false, rowsTomb(id, gen, nil))
				if m != nil && m.hasRef {
					m.hasRef, m.inReg = false, false
				}
				check(fmt.Sprintf("tomb(%s)", id.String()[35:]))
			case op == 4: // an eviction tombstone
				gen++
				at := clock
				f.read(false, rowsTomb(id, gen, &at))
				if m != nil && m.hasRef {
					m.hasRef = false // the registry keeps it through its grace
					if m.retiredAt.IsZero() {
						m.retiredAt = at
					}
				}
				check(fmt.Sprintf("evict(%s)", id.String()[35:]))
			case op == 5: // the replica's own sweep
				cutoff := clock.Add(-time.Duration(rng.Intn(30)) * time.Minute)
				f.reg.SweepGlobals(cutoff)
				for _, x := range model {
					at := x.retiredAt
					if at.IsZero() {
						at = x.until
					}
					if x.inReg && !at.IsZero() && at.Before(cutoff) {
						x.inReg = false
					}
				}
				check("sweep")
			default: // a full reload that returns a random subset of the rows the replica tracks, as they are
				var rows []row
				kept := map[uuid.UUID]bool{}
				for _, rid := range ids {
					if x := model[rid]; x != nil && x.hasRef && rng.Intn(2) == 0 {
						rows = append(rows, live(rid, x.until, x.retiredAt))
						kept[rid] = true
					}
				}
				rng.Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
				f.read(true, rows...)
				for rid, x := range model {
					switch {
					case kept[rid]:
						x.inReg = true // re-applied as it is in the table
					case x.hasRef:
						x.hasRef, x.inReg = false, false
					}
				}
				check("reload")
			}
		}
	}
}
