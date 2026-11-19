// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

// -reconcile beside a serving daemon: a save or delete that lands between its
// reads of the rows and of the store is not drift. Guarded by WARDYN_TEST_PG.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// raceExt is a memExt that lists what it holds. hold(owner, name) stops the
// next Put or Delete of that row just after its value is written or removed,
// until release is closed: the row's write is in flight. beforeWalk runs
// before each listing, with the listing's number from 1.
type raceExt struct {
	*memExt
	holdKey    string
	entered    chan struct{}
	release    chan struct{}
	walks      int
	beforeWalk func(n int)
}

func newRaceExt() *raceExt { return &raceExt{memExt: newMemExt("vaultkv")} }

func (r *raceExt) hold(owner, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.holdKey, r.entered, r.release = r.key(owner, name), make(chan struct{}), make(chan struct{})
}

func (r *raceExt) wait(owner, name string) {
	r.mu.Lock()
	hit := r.holdKey != "" && r.holdKey == r.key(owner, name)
	if hit {
		r.holdKey = ""
	}
	entered, release := r.entered, r.release
	r.mu.Unlock()
	if hit {
		close(entered)
		<-release
	}
}

func (r *raceExt) set(owner, name string, present bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if present {
		r.vals[r.key(owner, name)] = []byte("synthetic")
	} else {
		delete(r.vals, r.key(owner, name))
	}
}

func (r *raceExt) Put(ctx context.Context, owner, name, prev string, v []byte, createOnly bool) (string, error) {
	ref, err := r.memExt.Put(ctx, owner, name, prev, v, createOnly)
	r.wait(owner, name)
	return ref, err
}

func (r *raceExt) Delete(ctx context.Context, owner, name, ref string) error {
	err := r.memExt.Delete(ctx, owner, name, ref)
	r.wait(owner, name)
	return err
}

func (r *raceExt) Walk(context.Context) ([]secretstore.ExternalEntry, error) {
	r.mu.Lock()
	r.walks++
	n, hook := r.walks, r.beforeWalk
	r.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []secretstore.ExternalEntry
	for k := range r.vals {
		owner, name, _ := strings.Cut(k, "/")
		out = append(out, secretstore.ExternalEntry{Owner: owner, Name: name, Ref: k})
	}
	return out, nil
}

// reconcileBehind runs Reconcile once the held write is in flight, and fails
// if it finishes before release: its recheck must wait for the write.
func reconcileBehind(t *testing.T, s *Store, ext *raceExt, write <-chan error) ReconcileReport {
	t.Helper()
	select {
	case <-ext.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the held write never reached the store")
	}
	type result struct {
		rep ReconcileReport
		err error
	}
	done := make(chan result, 1)
	go func() {
		rep, err := s.Reconcile(t.Context())
		done <- result{rep, err}
	}()
	select {
	case r := <-done:
		close(ext.release)
		t.Fatalf("reconcile finished while a write of a row it found was in flight: %+v, %v", r.rep, r.err)
	case <-time.After(time.Second): // held behind the write's row lock
	}
	close(ext.release)
	if err := <-write; err != nil {
		t.Fatalf("held write: %v", err)
	}
	var r result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("reconcile never finished after the write did")
	}
	if r.err != nil {
		t.Fatalf("Reconcile: %v", r.err)
	}
	return r.rep
}

// A save whose value is in the store but whose row is not yet written is not
// a value without a pointer.
func TestPG_Reconcile_ASaveInFlightIsNotAnOrphan(t *testing.T) {
	pool := rekeyDatabase(t)
	ext := newRaceExt()
	s := mixedStore(t, pool, mustIdentity(t), false, ext, true)
	mustPut(t, s, "bob", "kept", "v1")
	ext.hold("bob", "added")
	write := make(chan error, 1)
	go func() { write <- s.For("bob").Put(t.Context(), "added", []byte("v2")) }()
	rep := reconcileBehind(t, s, ext, write)
	if len(rep.Orphans) != 0 || len(rep.Dangling) != 0 || rep.Checked != 1 {
		t.Fatalf("report = %+v; want 1 checked and no drift", rep)
	}
}

// A delete whose value is gone but whose row is not yet removed is not a
// pointer without a value.
func TestPG_Reconcile_ADeleteInFlightIsNotDangling(t *testing.T) {
	pool := rekeyDatabase(t)
	ext := newRaceExt()
	s := mixedStore(t, pool, mustIdentity(t), false, ext, true)
	mustPut(t, s, "bob", "kept", "v1")
	mustPut(t, s, "bob", "gone", "v1")
	ext.hold("bob", "gone")
	write := make(chan error, 1)
	go func() { write <- s.For("bob").Delete(t.Context(), "gone") }()
	rep := reconcileBehind(t, s, ext, write)
	if len(rep.Orphans) != 0 || len(rep.Dangling) != 0 {
		t.Fatalf("report = %+v; want no drift", rep)
	}
}

// The recheck keeps real drift: a row whose value is gone, and a value no row
// points to that the store still lists. A value the store lists once and then
// no longer (a failed save's, removed again by its writer) is not reported.
func TestPG_Reconcile_ReportsDriftThatHoldsAfterTheRecheck(t *testing.T) {
	pool := rekeyDatabase(t)
	ext := newRaceExt()
	s := mixedStore(t, pool, mustIdentity(t), false, ext, true)
	mustPut(t, s, "bob", "pointer", "v1")
	ext.set("bob", "pointer", false)
	ext.set("bob", "stray", true)
	ext.beforeWalk = func(n int) { ext.set("bob", "brief", n == 1) }
	rep, err := s.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.Dangling) != 1 || !strings.Contains(rep.Dangling[0], `name="pointer"`) {
		t.Fatalf("dangling = %q; want the pointer row only", rep.Dangling)
	}
	if len(rep.Orphans) != 1 || rep.Orphans[0].Name != "stray" {
		t.Fatalf("orphans = %+v; want the stray value only", rep.Orphans)
	}
}

// A write that holds its row for longer than the recheck waits aborts the
// reconcile: a finding it could not recheck is never reported as drift.
func TestPG_Reconcile_AWriteThatOutlastsTheWaitAborts(t *testing.T) {
	pool := rekeyDatabase(t)
	ext := newRaceExt()
	s := mixedStore(t, pool, mustIdentity(t), false, ext, true)
	ext.hold("bob", "added")
	write := make(chan error, 1)
	go func() { write <- s.For("bob").Put(t.Context(), "added", []byte("v2")) }()
	select {
	case <-ext.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the held write never reached the store")
	}
	impatient := *s
	impatient.extTimeout = 50 * time.Millisecond
	rep, err := impatient.Reconcile(t.Context())
	close(ext.release)
	if werr := <-write; werr != nil {
		t.Fatalf("held write: %v", werr)
	}
	if err == nil || len(rep.Orphans) != 0 || len(rep.Dangling) != 0 {
		t.Fatalf("Reconcile = %+v, %v; want an abort and no drift", rep, err)
	}
}
