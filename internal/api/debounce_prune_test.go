// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fillStamps is a map of n times, all at at.
func fillStamps(n int, at time.Time) map[int]time.Time {
	m := make(map[int]time.Time, n)
	for i := range n {
		m[i] = at
	}
	return m
}

// Past the soft bound only the entries a window old are deleted; the fresh ones stay.
func TestPruneStale_DeletesOnlyEntriesPastTheWindow(t *testing.T) {
	now := time.Now()
	const window = time.Minute
	m := make(map[int]time.Time, 5000)
	for i := range 5000 {
		if i%2 == 0 {
			m[i] = now.Add(-2 * window)
		} else {
			m[i] = now.Add(-window / 2)
		}
	}
	var swept time.Time
	pruneStale(m, stampTime, window, now, &swept)
	if len(m) != 2500 {
		t.Fatalf("%d entries remain, want the 2500 fresh ones", len(m))
	}
	for k := range m {
		if k%2 == 0 {
			t.Fatalf("a stale entry %d was kept", k)
		}
	}
}

// A sweep runs at most once per window, however many stale entries arrive in between.
func TestPruneStale_SweepsAtMostOncePerWindow(t *testing.T) {
	now := time.Now()
	const window = time.Minute
	m := fillStamps(5000, now.Add(-2*window))
	var swept time.Time
	pruneStale(m, stampTime, window, now, &swept)
	if len(m) != 0 {
		t.Fatalf("first sweep left %d entries", len(m))
	}
	for i := range 5000 {
		m[i] = now.Add(-2 * window)
	}
	pruneStale(m, stampTime, window, now.Add(window/2), &swept)
	if len(m) != 5000 {
		t.Errorf("a second call inside the window left %d entries, want all 5000 (no sweep)", len(m))
	}
	pruneStale(m, stampTime, window, now.Add(window), &swept)
	if len(m) != 0 {
		t.Errorf("a call one window later left %d entries, want 0", len(m))
	}
}

// Past the hard bound the map is cleared, as it was before.
func TestPruneStale_ClearsPastTheHardBound(t *testing.T) {
	now := time.Now()
	m := fillStamps(debounceMapHard+1, now)
	var swept time.Time
	pruneStale(m, stampTime, time.Minute, now, &swept)
	if len(m) != 0 {
		t.Errorf("%d entries remain past the hard bound, want 0", len(m))
	}
}

// Past the soft bound a fresh touch stays debounced: the old code cleared the map, so every run read due again.
// An entry just inside the window survives the sweep, and one exactly a window old is gone and due.
func TestTouchDue_PastTheBoundAFreshTouchStaysDebounced(t *testing.T) {
	srv := &Server{}
	now := time.Now()
	ids := make([]uuid.UUID, debounceMapSoft+1)
	srv.lastTouch = make(map[uuid.UUID]time.Time)
	for i := range ids {
		ids[i] = uuid.New()
		srv.lastTouch[ids[i]] = now
	}
	kept, gone := uuid.New(), uuid.New()
	srv.lastTouch[kept] = now.Add(-(touchDebounce - time.Second))
	srv.lastTouch[gone] = now.Add(-touchDebounce)

	fresh := uuid.New()
	if !srv.touchDue(fresh) {
		t.Fatal("a new run's first touch is not due")
	}
	if got := len(srv.lastTouch); got != debounceMapSoft+1+1+1 {
		t.Errorf("%d entries after the sweep, want %d (4,097 fresh, the kept aged one and the new id)", got, debounceMapSoft+3)
	}
	if srv.touchDue(ids[0]) {
		t.Error("a run touched just now reads due after the map passed its bound")
	}
	if srv.touchDue(kept) {
		t.Error("a run touched inside the window reads due")
	}
	if !srv.touchDue(gone) {
		t.Error("a run touched exactly a window ago is not due")
	}
}

// The two stamp clocks keep their live entries past the bound; one a window old is due either way.
func TestPauseClocks_PastTheBoundAFreshStampStaysNotDue(t *testing.T) {
	for _, presence := range []bool{true, false} {
		c := &pauseClocks{}
		now := time.Now()
		ids := make([]uuid.UUID, debounceMapSoft+1)
		m := c.clock(presence)
		for i := range ids {
			ids[i] = uuid.New()
			m[ids[i]] = now
		}
		kept, gone := uuid.New(), uuid.New()
		m[kept] = now.Add(-(presenceStampEvery - time.Second))
		m[gone] = now.Add(-presenceStampEvery)

		c.stamped(presence, uuid.New(), now)
		if c.stampDue(presence, ids[0], now) {
			t.Errorf("presence=%v: a stamp made just now reads due after the map passed its bound", presence)
		}
		if c.stampDue(presence, kept, now) {
			t.Errorf("presence=%v: a stamp inside the window reads due", presence)
		}
		if _, ok := m[gone]; ok || !c.stampDue(presence, gone, now) {
			t.Errorf("presence=%v: a stamp exactly a window old is kept or not due", presence)
		}
	}
}

// A root-filesystem sample is kept ten minutes: a stale one is still shown when a walk is refused.
func TestPauseClocks_PastTheBoundARecentRootSampleStays(t *testing.T) {
	c := &pauseClocks{}
	now := time.Now()
	c.diskRoot = map[uuid.UUID]diskRootSample{}
	first := uuid.New()
	c.diskRoot[first] = diskRootSample{kb: "42", at: now}
	for range debounceMapSoft {
		c.diskRoot[uuid.New()] = diskRootSample{kb: "1", at: now}
	}
	aging, old := uuid.New(), uuid.New()
	c.diskRoot[aging] = diskRootSample{kb: "7", at: now.Add(-9 * time.Minute)}
	c.diskRoot[old] = diskRootSample{kb: "8", at: now.Add(-11 * time.Minute)}

	c.setRootSample(uuid.New(), "9", now)
	if kb, fresh := c.rootSample(first, now); kb != "42" || !fresh {
		t.Errorf("rootSample = %q, %v after the map passed its bound, want 42 and fresh", kb, fresh)
	}
	if kb, fresh := c.rootSample(aging, now); kb != "7" || fresh {
		t.Errorf("a nine-minute-old sample = %q, %v, want 7 kept and not fresh", kb, fresh)
	}
	if _, ok := c.diskRoot[old]; ok {
		t.Error("an eleven-minute-old sample survived the sweep")
	}
}

// The ready and re-assert caches keep their live entries past the bound too.
func TestUICaches_PastTheBoundLiveEntriesStay(t *testing.T) {
	now := time.Now()
	srv := &Server{cfg: Config{Now: func() time.Time { return now }}}

	srv.uiReady = map[string]time.Time{}
	for i := range debounceMapSoft + 1 {
		srv.uiReady[fmt.Sprintf("key-%d", i)] = now
	}
	srv.uiReady["kept"] = now.Add(-(uiReadyTTL - time.Second))
	srv.uiReady["gone"] = now.Add(-uiReadyTTL)
	srv.markUIAppReady("new")
	if !srv.uiAppReady("key-0") {
		t.Error("an app ready just now reads not ready after the map passed its bound")
	}
	if !srv.uiAppReady("kept") {
		t.Error("an app ready a second inside the window reads not ready")
	}
	if _, ok := srv.uiReady["gone"]; ok {
		t.Error("an app ready exactly a window ago survived the sweep")
	}

	session := func() uiSession { return uiSession{Run: uuid.New(), Principal: "p", IssuedAt: 1} }
	srv.uiReassert = map[string]time.Time{}
	first, kept, gone := session(), session(), session()
	srv.uiReassert[uiPassKey(first)] = now
	for range debounceMapSoft {
		srv.uiReassert[uiPassKey(session())] = now
	}
	srv.uiReassert[uiPassKey(kept)] = now.Add(-(uiReassertInterval - time.Second))
	srv.uiReassert[uiPassKey(gone)] = now.Add(-uiReassertInterval)
	srv.markUIReasserted(session(), now, srv.uiInbound.drops.Load())
	if srv.uiReassertDue(first, now) {
		t.Error("a session re-checked just now is due after the map passed its bound")
	}
	if srv.uiReassertDue(kept, now) {
		t.Error("a session re-checked a second inside the window is due")
	}
	if !srv.uiReassertDue(gone, now) {
		t.Error("a session re-checked exactly a window ago is not due")
	}
}
