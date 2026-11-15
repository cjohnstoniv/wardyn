// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// eraReads seals one row for alice, then reads copies of it written at each time in at.
func eraReads(t *testing.T, s *Sealer, sealed types.AuditEvent, at ...time.Time) []string {
	t.Helper()
	evs := make([]types.AuditEvent, len(at))
	for i, a := range at {
		evs[i] = sealed
		evs[i].Time = a
	}
	back, err := s.Unseal(t.Context(), evs)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(back))
	for i, ev := range back {
		got[i] = ev.Actor
	}
	return got
}

func sealedFor(t *testing.T, s *Sealer, person string) types.AuditEvent {
	t.Helper()
	ev, _, err := s.Seal(t.Context(), humanEvent(person, "run.kill"))
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func wantReads(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: read %q, want %q", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: read %q, want %q", what, got, want)
			return
		}
	}
}

// A row written at or before an unbind reads as the person it released, by that person's own key: the
// next holder's erasure, which destroys under the subject, does not erase it, and the released person's
// erasure does, whoever holds the identity row now.
func TestSealFullRowBeforeAnUnbindReadsAsThePersonItReleased(t *testing.T) {
	s, _, dir := newFullSealer(t, "alice")
	id := dir.ids["alice"]
	sealed := sealedFor(t, s, "alice")
	unbound := time.Now().UTC().Truncate(time.Microsecond).Add(time.Minute)
	delete(dir.ids, "alice")
	dir.ids["bob"] = id
	dir.eras = map[string][]SubjectEra{id: {{Principal: "alice", Until: unbound}}}
	before, at, after := unbound.Add(-time.Minute), unbound, unbound.Add(time.Microsecond)

	wantReads(t, "after the rebind", eraReads(t, s, sealed, before, at, after), "alice", "alice", "bob")

	bobErased := after.Add(time.Hour)
	dir.destroyed["bob"], dir.destroyed[SubjectActorPrefix+id] = bobErased, bobErased
	wantReads(t, "after the new holder's erasure", eraReads(t, s, sealed, before, at, after), "alice", "alice", ErasedValue)

	dir.eras[id][0].Principal = ""
	wantReads(t, "after the released person's erasure", eraReads(t, s, sealed, before, at, after), ErasedValue, ErasedValue, ErasedValue)
}

// An earlier binding whose name is still recorded reads by its person's key as any row does: erased when
// the key was destroyed after the row, and readable when the person was erased at or before the row and
// seen again.
func TestSealFullEarlierBindingReadsByItsOwnKey(t *testing.T) {
	s, _, dir := newFullSealer(t, "alice")
	id := dir.ids["alice"]
	sealed := sealedFor(t, s, "alice")
	unbound := time.Now().UTC().Truncate(time.Microsecond).Add(time.Minute)
	delete(dir.ids, "alice")
	dir.ids["bob"] = id
	dir.eras = map[string][]SubjectEra{id: {{Principal: "alice", Until: unbound}}}
	row := unbound.Add(-time.Minute)

	dir.destroyed["alice"] = unbound.Add(time.Hour)
	wantReads(t, "key destroyed after the row", eraReads(t, s, sealed, row), ErasedValue)
	dir.destroyed["alice"] = row.Add(-time.Minute)
	wantReads(t, "key destroyed before the row", eraReads(t, s, sealed, row), "alice")
	dir.destroyed["alice"] = row
	wantReads(t, "key destroyed at the row's time", eraReads(t, s, sealed, row), "alice")
}

// Several unbinds of one identity row: each row is the person whose binding it falls in, the boundary
// belonging to the binding that ends there; past the last unbind, the current holder.
func TestSealFullSeveralUnbindsEachRowIsItsBindingsPerson(t *testing.T) {
	s, _, dir := newFullSealer(t, "alice")
	id := dir.ids["alice"]
	sealed := sealedFor(t, s, "alice")
	u1 := time.Now().UTC().Truncate(time.Microsecond).Add(time.Minute)
	u2 := u1.Add(time.Hour)
	delete(dir.ids, "alice")
	dir.ids["bob"] = id
	dir.eras = map[string][]SubjectEra{id: {{Principal: "alice", Until: u1}, {Principal: "carol", Until: u2}}}

	wantReads(t, "two unbinds", eraReads(t, s, sealed, u1, u1.Add(time.Microsecond), u2, u2.Add(time.Microsecond)),
		"alice", "carol", "carol", "bob")
}

// An unbind that left no binding history (made before it was kept) still reads as the current holder,
// as before it was kept, and the holder's erasure, under the subject too, still erases it.
func TestSealFullUnrecordedUnbindReadsAsTheCurrentHolder(t *testing.T) {
	s, _, dir := newFullSealer(t, "alice")
	id := dir.ids["alice"]
	sealed := sealedFor(t, s, "alice")
	delete(dir.ids, "alice")
	dir.ids["bob"] = id
	row := sealed.Time

	wantReads(t, "no history", eraReads(t, s, sealed, row), "bob")
	dir.destroyed[SubjectActorPrefix+id] = row.Add(time.Hour)
	wantReads(t, "no history, holder erased", eraReads(t, s, sealed, row), ErasedValue)
}

// An erasure of whoever held the identity row before an unbind that left no record destroyed under the
// subject. The next recorded binding still reads the rows written before it as erased, and its own rows
// after it by its person; a blank binding reads erased while the row's current holder is not.
func TestSealFullErasureBeforeAnUnrecordedUnbindStaysErased(t *testing.T) {
	s, _, dir := newFullSealer(t, "alice")
	id := dir.ids["alice"]
	sealed := sealedFor(t, s, "alice")
	gone := time.Now().UTC().Truncate(time.Microsecond).Add(time.Minute)
	until := gone.Add(time.Hour)
	dir.destroyed["alice"], dir.destroyed[SubjectActorPrefix+id] = gone, gone
	delete(dir.ids, "alice")
	dir.ids["bob"] = id
	dir.eras = map[string][]SubjectEra{id: {{Principal: "carol", Until: until, SubjectGone: gone}}}

	wantReads(t, "erased before an unrecorded unbind", eraReads(t, s, sealed, gone.Add(-time.Second), gone, until),
		ErasedValue, "carol", "carol")

	dir.eras[id][0] = SubjectEra{Until: until}
	wantReads(t, "a blank binding, its holder not erased", eraReads(t, s, sealed, until, until.Add(time.Microsecond)), ErasedValue, "bob")
}

// An actor that waited under the pending key while an unbind left its person with no identity row is stored
// as the identity row that was theirs when the row was written: it reads as them, and as erased once their
// name is cleared from the binding history, never as their name in clear.
func TestSealFullAnActorWaitingAcrossAnUnbindIsStoredAsItsBinding(t *testing.T) {
	s, mk, dir := newFullSealer(t, "alice")
	id := dir.ids["alice"]
	mk.down = true
	sealed, pending, err := s.Seal(t.Context(), humanEvent("alice", "run.kill"))
	if err != nil || !pending {
		t.Fatalf("Seal with the keys down = pending %v, %v; want a pending row", pending, err)
	}
	mk.down = false
	delete(dir.ids, "alice")
	// The unbind is at the row's own time, before the drain: the lookup is by the row's time, not the drain's.
	dir.eras = map[string][]SubjectEra{id: {{Principal: "alice", Until: sealed.Time}}}
	done, err := s.Reseal(t.Context(), sealed)
	if err != nil {
		t.Fatal(err)
	}
	if want := SubjectActorPrefix + id; done.Actor != want {
		t.Fatalf("resealed actor = %q, want %q", done.Actor, want)
	}
	wantReads(t, "after the reseal", eraReads(t, s, done, done.Time), "alice")
	dir.eras[id][0].Principal = ""
	wantReads(t, "after the person's erasure", eraReads(t, s, done, done.Time), ErasedValue)
}

// An erasure destroys the person's keys, then clears them from the binding history. One that does both
// while a waiting actor of a person an unbind left with no identity row is being stored leaves no binding
// to find: the actor is stored as erased, never as the name.
func TestSealFullAnActorWaitingAcrossAnUnbindIsStoredErasedWhenAnErasureRunsMeanwhile(t *testing.T) {
	s, mk, dir := newFullSealer(t, "alice")
	id := dir.ids["alice"]
	mk.down = true
	sealed, pending, err := s.Seal(t.Context(), humanEvent("alice", "run.kill"))
	if err != nil || !pending {
		t.Fatalf("Seal with the keys down = pending %v, %v; want a pending row", pending, err)
	}
	mk.down = false
	delete(dir.ids, "alice")
	dir.eras = map[string][]SubjectEra{id: {{Principal: "alice", Until: sealed.Time}}}
	erased := false
	s.GoneSince = func(context.Context, string, time.Time) (bool, error) {
		was := erased
		dir.eras[id][0].Principal, erased = "", true // the erasure runs right after the first check
		return was, nil
	}
	done, err := s.Reseal(t.Context(), sealed)
	if err != nil {
		t.Fatal(err)
	}
	if done.Actor != ErasedValue {
		t.Fatalf("resealed actor = %q, want %q", done.Actor, ErasedValue)
	}
}
