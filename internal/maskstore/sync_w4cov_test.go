// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package maskstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
)

var (
	w4CovRun   = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	w4CovRowA  = uuid.MustParse("bbbbbbbb-0000-0000-0000-00000000000a")
	w4CovRowB  = uuid.MustParse("bbbbbbbb-0000-0000-0000-00000000000b")
	w4CovRowC  = uuid.MustParse("bbbbbbbb-0000-0000-0000-00000000000c")
	w4CovT0    = time.Now().UTC().Truncate(time.Second)
	w4CovOwner = "alice"
)

// w4CovFixture is a pool-less Store whose owner has key version 1.
type w4CovFixture struct {
	s    *Store
	reg  *secretmask.Registry
	keys *w4CovKeys
	key  []byte
}

func w4CovNewFixture(t *testing.T) *w4CovFixture {
	t.Helper()
	keys := w4CovNewKeys()
	key := keys.put(w4CovOwner, 1, 9)
	reg := secretmask.NewRegistry()
	return &w4CovFixture{s: New(nil, keys, reg), reg: reg, keys: keys, key: key}
}

// sealedRow is a live row as a read would return it, sealed under the owner's
// version-1 key with the aad the writer used.
func (f *w4CovFixture) sealedRow(t *testing.T, id uuid.UUID, bucket, name string, runID *uuid.UUID, value string, gen int64) row {
	t.Helper()
	scope := name
	if bucket == bucketRun {
		scope = runID.String()
	}
	blob, err := kek.Seal(f.key, []byte(value), aad(bucket, id, w4CovOwner, scope, 1))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	v := 1
	return row{id: id, bucket: bucket, owner: w4CovOwner, runID: runID, name: name, version: &v, sealed: blob, gen: gen}
}

func (f *w4CovFixture) snapshot() [][]byte { return f.reg.Snapshot(w4CovRun) }

func (f *w4CovFixture) ref(id uuid.UUID) *ref {
	f.s.sync.mu.Lock()
	defer f.s.sync.mu.Unlock()
	return f.s.sync.rows[id]
}

func TestW4CovApplyOpensAnUnknownRowIntoTheRegistry(t *testing.T) {
	f := w4CovNewFixture(t)
	run := w4CovRun
	until := w4CovT0.Add(time.Hour)
	runRow := f.sealedRow(t, w4CovRowA, bucketRun, "", &run, "per-run-secret-value", 5)
	globalRow := f.sealedRow(t, w4CovRowB, bucketGlobal, "cred", nil, "global-secret-value", 6)
	globalRow.until = &until

	if err := f.s.apply(context.Background(), []row{runRow, globalRow}, false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	snap := f.snapshot()
	for _, want := range []string{"per-run-secret-value", "global-secret-value"} {
		if !w4CovContains(snap, want) {
			t.Errorf("%q is not masked after apply", want)
		}
	}
	if rf := f.ref(w4CovRowA); rf == nil || rf.gen != 5 || rf.bucket != bucketRun || rf.runID != run || !rf.current {
		t.Errorf("run row ref = %+v", rf)
	}
	rf := f.ref(w4CovRowB)
	if rf == nil || rf.gen != 6 || !rf.current || !rf.until.Equal(until) || !rf.retiredAt.IsZero() {
		t.Errorf("global row ref = %+v, want current, gen 6, until %v", rf, until)
	}
	if len(f.keys.lookups) != 1 {
		t.Errorf("one owner and version looked up the key %d times, want 1", len(f.keys.lookups))
	}
	for i, k := range f.keys.handed {
		if !bytes.Equal(k, make([]byte, len(k))) {
			t.Errorf("key handed out #%d was not cleared after apply", i)
		}
	}
}

func TestW4CovApplyKeepsARetiredGlobalMaskedButNotCurrent(t *testing.T) {
	f := w4CovNewFixture(t)
	r := f.sealedRow(t, w4CovRowB, bucketGlobal, "cred", nil, "retired-secret-value", 3)
	retired := w4CovT0
	r.retiredAt = &retired
	if err := f.s.apply(context.Background(), []row{r}, false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !w4CovContains(f.snapshot(), "retired-secret-value") {
		t.Fatal("a retired value must stay masked until swept")
	}
	rf := f.ref(w4CovRowB)
	if rf == nil || rf.current || !rf.retiredAt.Equal(retired) {
		t.Fatalf("ref = %+v, want not current, retired at %v", rf, retired)
	}
	if n := f.reg.SweepGlobals(retired.Add(time.Second)); n != 1 {
		t.Errorf("sweeping past the retirement dropped %d values, want 1", n)
	}
	if w4CovContains(f.snapshot(), "retired-secret-value") {
		t.Error("the retired value is still masked after the sweep")
	}
}

func TestW4CovApplyUpdatesAKnownRowWithoutOpeningIt(t *testing.T) {
	f := w4CovNewFixture(t)
	f.s.note(&ref{id: w4CovRowB, gen: 2, bucket: bucketGlobal, owner: w4CovOwner, name: "cred", value: []byte("known-secret-value"), current: true})
	until := w4CovT0.Add(2 * time.Hour)
	// No sealed blob and no version: opening it would skip the row, so an
	// update that still lands proves the known ref was used.
	update := row{id: w4CovRowB, bucket: bucketGlobal, owner: w4CovOwner, name: "cred", until: &until, gen: 9}
	if err := f.s.apply(context.Background(), []row{update}, false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	rf := f.ref(w4CovRowB)
	if rf == nil || rf.gen != 9 || !rf.until.Equal(until) || !rf.current {
		t.Fatalf("ref = %+v, want gen 9, until %v, current", rf, until)
	}
	if !w4CovContains(f.snapshot(), "known-secret-value") {
		t.Error("the known value was not applied to the registry")
	}
	if len(f.keys.lookups) != 0 {
		t.Errorf("a known row asked for the owner's key: %v", f.keys.lookups)
	}
}

func TestW4CovApplyTombstoneOfAKnownRow(t *testing.T) {
	retired := w4CovT0
	cases := []struct {
		name         string
		ref          ref
		retiredAt    *time.Time
		wantMasked   bool
		wantSweepOne bool
	}{
		{"a current global with a retire time stays masked as retired",
			ref{bucket: bucketGlobal, current: true}, &retired, true, true},
		{"a global with no retire time is dropped",
			ref{bucket: bucketGlobal, current: true}, nil, false, false},
		{"an already-retired global is dropped",
			ref{bucket: bucketGlobal, current: false}, &retired, false, false},
		{"a per-run value is dropped",
			ref{bucket: bucketRun, current: true}, &retired, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := w4CovNewFixture(t)
			rf := c.ref
			rf.id, rf.owner, rf.name, rf.runID = w4CovRowC, w4CovOwner, "cred", w4CovRun
			rf.value = []byte("tombstoned-secret-value")
			if rf.bucket == bucketRun {
				f.reg.AddLocal(w4CovRun, rf.value)
			} else {
				f.reg.ApplyGlobal(w4CovOwner, "cred", rf.value, time.Time{}, time.Time{})
			}
			f.s.note(&rf)
			if !w4CovContains(f.snapshot(), "tombstoned-secret-value") {
				t.Fatal("the fixture value is not masked before the tombstone")
			}

			tomb := row{id: w4CovRowC, bucket: rf.bucket, tombstone: true, retiredAt: c.retiredAt, gen: 12}
			if err := f.s.apply(context.Background(), []row{tomb}, false); err != nil {
				t.Fatalf("apply: %v", err)
			}
			if f.ref(w4CovRowC) != nil {
				t.Error("the tombstoned ref is still tracked")
			}
			if got := w4CovContains(f.snapshot(), "tombstoned-secret-value"); got != c.wantMasked {
				t.Errorf("masked after the tombstone = %v, want %v", got, c.wantMasked)
			}
			swept := f.reg.SweepGlobals(retired.Add(time.Second))
			if (swept == 1) != c.wantSweepOne {
				t.Errorf("sweep dropped %d, want a retired value to exist = %v", swept, c.wantSweepOne)
			}
		})
	}
}

func TestW4CovApplyTombstoneOfAnUnknownRowIsANoOp(t *testing.T) {
	f := w4CovNewFixture(t)
	tomb := row{id: w4CovRowC, bucket: bucketGlobal, tombstone: true, gen: 3}
	if err := f.s.apply(context.Background(), []row{tomb}, false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(f.keys.lookups) != 0 || len(f.snapshot()) != 0 || f.ref(w4CovRowC) != nil {
		t.Errorf("a tombstone for a row never seen changed state: lookups=%v", f.keys.lookups)
	}
}

// w4CovRetired marks a row retired; its value is never read.
var w4CovRetired = time.Now()

func TestW4CovApplySkipsWhatCanNeverOpen(t *testing.T) {
	run := w4CovRun
	cases := []struct {
		name   string
		mutate func(f *w4CovFixture, r *row)
		bucket string
	}{
		{"no key version", func(_ *w4CovFixture, r *row) { r.version = nil }, bucketGlobal},
		{"no sealed blob", func(_ *w4CovFixture, r *row) { r.sealed = nil }, bucketGlobal},
		{"a per-run row with no run", func(_ *w4CovFixture, r *row) { r.runID = nil }, bucketRun},
		// The next three rows are retired: a live row that cannot open fences the runs it
		// masks in Postgres (TestPG_MaskStore_UnopenableLiveRowFencesRun), so only a retired
		// one is skipped with no database.
		{"a destroyed owner key", func(f *w4CovFixture, r *row) {
			f.keys.errs[w4CovOwner] = fmt.Errorf("gone: %w", subjectkey.ErrDataLoss)
			r.retiredAt = &w4CovRetired
		}, bucketGlobal},
		{"a blob that does not authenticate", func(_ *w4CovFixture, r *row) {
			r.sealed = append([]byte(nil), r.sealed...)
			r.sealed[len(r.sealed)-1] ^= 1
			r.retiredAt = &w4CovRetired
		}, bucketGlobal},
		{"a blob sealed for another row", func(_ *w4CovFixture, r *row) { r.name, r.retiredAt = "other", &w4CovRetired }, bucketGlobal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := w4CovNewFixture(t)
			r := f.sealedRow(t, w4CovRowA, c.bucket, "cred", &run, "unopenable-secret-value", 4)
			c.mutate(f, &r)
			if err := f.s.apply(context.Background(), []row{r}, false); err != nil {
				t.Fatalf("an unopenable row must be skipped, not fail the read: %v", err)
			}
			if f.ref(w4CovRowA) != nil || len(f.snapshot()) != 0 {
				t.Errorf("a skipped row reached the cache: ref=%+v snapshot=%d", f.ref(w4CovRowA), len(f.snapshot()))
			}
		})
	}
}

func TestW4CovApplyAbortsOnATransientKeyFailureAndAppliesNothingAfter(t *testing.T) {
	f := w4CovNewFixture(t)
	injected := fmt.Errorf("key service down: %w", secretstore.ErrUnavailable)
	f.keys.errs[w4CovOwner] = injected
	first := f.sealedRow(t, w4CovRowA, bucketGlobal, "cred", nil, "first-secret-value", 1)
	second := f.sealedRow(t, w4CovRowB, bucketGlobal, "cred", nil, "second-secret-value", 2)

	err := f.s.apply(context.Background(), []row{first, second}, true)
	if !errors.Is(err, injected) {
		t.Fatalf("apply = %v, want the injected key error wrapped", err)
	}
	if len(f.snapshot()) != 0 || f.ref(w4CovRowA) != nil || f.ref(w4CovRowB) != nil {
		t.Error("a failed read applied rows: the cursor would move past values never masked")
	}
	if len(f.keys.lookups) != 1 {
		t.Errorf("the read went on after the failure: %d key lookups", len(f.keys.lookups))
	}
}

// A key that does not unwrap never heals by retrying: the row is handled like a destroyed key
// (here retired, so no database is needed) and the read goes on to the rows after it.
func TestW4CovApplySkipsARowWhoseKeyDoesNotUnwrap(t *testing.T) {
	f := w4CovNewFixture(t)
	f.keys.errs[w4CovOwner] = errors.New("subjectkey: generation 1 does not unwrap (its key version retired)")
	bad := f.sealedRow(t, w4CovRowA, bucketGlobal, "cred", nil, "first-secret-value", 1)
	bad.retiredAt = &w4CovRetired
	if err := f.s.apply(context.Background(), []row{bad}, true); err != nil {
		t.Fatalf("a permanent key failure must not abort the read: %v", err)
	}
	if f.ref(w4CovRowA) != nil || len(f.snapshot()) != 0 {
		t.Error("a row whose key does not unwrap reached the cache")
	}
}

func TestW4CovFullApplyDropsWhatTheTableNoLongerHas(t *testing.T) {
	for _, full := range []bool{true, false} {
		t.Run(fmt.Sprintf("full=%v", full), func(t *testing.T) {
			f := w4CovNewFixture(t)
			stale := []byte("stale-secret-value")
			f.reg.ApplyGlobal(w4CovOwner, "cred", stale, time.Time{}, time.Time{})
			f.s.note(&ref{id: w4CovRowC, gen: 1, bucket: bucketGlobal, owner: w4CovOwner, name: "cred", value: stale, current: true})
			live := f.sealedRow(t, w4CovRowA, bucketGlobal, "cred", nil, "live-secret-value", 2)

			if err := f.s.apply(context.Background(), []row{live}, full); err != nil {
				t.Fatalf("apply: %v", err)
			}
			if !w4CovContains(f.snapshot(), "live-secret-value") {
				t.Error("the row in the read was not applied")
			}
			gotStale := w4CovContains(f.snapshot(), "stale-secret-value")
			if gotStale == full {
				t.Errorf("stale value masked = %v after a read with full=%v", gotStale, full)
			}
			if (f.ref(w4CovRowC) == nil) != full {
				t.Errorf("stale ref tracked = %v after a read with full=%v", f.ref(w4CovRowC) != nil, full)
			}
			if full && !bytes.Equal(stale, make([]byte, len(stale))) {
				t.Error("a dropped ref's value bytes were not cleared")
			}
		})
	}
}

func TestW4CovNoteKeepsTheNewestGenerationsValue(t *testing.T) {
	f := w4CovNewFixture(t)
	f.s.note(&ref{id: w4CovRowA, gen: 5, value: []byte("v5")})
	f.s.note(&ref{id: w4CovRowA, gen: 3, value: []byte("v3")})
	if rf := f.ref(w4CovRowA); string(rf.value) != "v5" || rf.gen != 5 {
		t.Errorf("an older generation replaced a newer one: %+v", rf)
	}
	f.s.note(&ref{id: w4CovRowA, gen: 5, value: []byte("again")})
	if rf := f.ref(w4CovRowA); string(rf.value) != "v5" {
		t.Errorf("an equal generation replaced the held value: %+v", rf)
	}
	f.s.note(&ref{id: w4CovRowA, gen: 6, value: []byte("v6")})
	if rf := f.ref(w4CovRowA); string(rf.value) != "v6" || rf.gen != 6 {
		t.Errorf("a newer generation did not replace: %+v", rf)
	}
}

func TestW4CovFreshAndSyncedDecisionsThatNeedNoRead(t *testing.T) {
	arrived := w4CovT0
	t.Run("a read that began after the bytes arrived is enough", func(t *testing.T) {
		f := w4CovNewFixture(t)
		f.s.sync.okStart = arrived.Add(time.Second)
		if err := f.s.Fresh(context.Background(), arrived); err != nil {
			t.Fatalf("Fresh = %v", err)
		}
	})
	t.Run("a running read is waited for, and a cancelled wait returns the context error", func(t *testing.T) {
		f := w4CovNewFixture(t)
		f.s.sync.running, f.s.sync.done = true, make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := f.s.Fresh(ctx, arrived); !errors.Is(err, context.Canceled) {
			t.Fatalf("Fresh = %v, want context.Canceled", err)
		}
	})
	t.Run("a running read that finishes fresh releases the waiter", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := w4CovNewFixture(t)
			arrived := time.Now()
			done := make(chan struct{})
			f.s.sync.running, f.s.sync.done = true, done
			result := make(chan error, 1)
			go func() { result <- f.s.Fresh(context.Background(), arrived) }()

			synctest.Wait() // the waiter is now blocked on done
			if len(result) != 0 {
				t.Fatalf("Fresh returned while a read was still running: %v", <-result)
			}
			f.s.sync.mu.Lock()
			f.s.sync.okStart, f.s.sync.running = arrived.Add(time.Second), false
			close(done)
			f.s.sync.mu.Unlock()
			synctest.Wait()
			select {
			case err := <-result:
				if err != nil {
					t.Fatalf("Fresh = %v", err)
				}
			default:
				t.Fatal("Fresh did not return after the running read finished")
			}
		})
	})
	t.Run("reads are spaced: a cancelled wait for the gap returns the context error", func(t *testing.T) {
		f := w4CovNewFixture(t)
		f.s.sync.lastStart = time.Now().Add(time.Hour)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := f.s.Fresh(ctx, arrived); !errors.Is(err, context.Canceled) {
			t.Fatalf("Fresh = %v, want context.Canceled", err)
		}
	})
	t.Run("a replica whose listener is down is not synced", func(t *testing.T) {
		f := w4CovNewFixture(t)
		err := f.s.Synced(context.Background())
		if err == nil || err.Error() != "the connection that listens for secret-masking changes is down" {
			t.Fatalf("Synced = %v", err)
		}
	})
}

func TestW4CovPokeIsNonBlockingAndCoalesces(t *testing.T) {
	f := w4CovNewFixture(t)
	f.s.poke() // no reader yet: must not block on a nil channel
	f.s.sync.kick = make(chan struct{}, 1)
	f.s.poke()
	f.s.poke()
	if n := len(f.s.sync.kick); n != 1 {
		t.Errorf("two pokes queued %d kicks, want 1", n)
	}
}

func TestW4CovStartWithAnEndedContextSetsUpTheKick(t *testing.T) {
	f := w4CovNewFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.s.Start(ctx)
	if cap(f.s.sync.kick) != 1 {
		t.Fatalf("Start left kick capacity %d, want 1", cap(f.s.sync.kick))
	}
	if len(f.s.sync.kick) != 0 {
		t.Error("Start queued a kick on its own")
	}
	called := false
	f.s.OnBackgroundRead(func(context.Context) { called = true })
	f.s.sync.idle(ctx)
	if !called {
		t.Error("OnBackgroundRead did not install the hook")
	}
}
