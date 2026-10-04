// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// memDir is a SubjectDirectory over maps.
type memDir struct {
	ids       map[string]string // principal -> subject id
	destroyed map[string]time.Time
	down      bool
}

func (d *memDir) AuditSubjectFor(_ context.Context, principal string) (string, bool, error) {
	if d.down {
		return "", false, errors.New("directory down")
	}
	id, ok := d.ids[principal]
	return id, ok, nil
}

func (d *memDir) AuditPrincipalOf(_ context.Context, id string) (string, bool, error) {
	if d.down {
		return "", false, errors.New("directory down")
	}
	for p, i := range d.ids {
		if i == id {
			return p, true, nil
		}
	}
	return "", false, nil
}

func (d *memDir) AuditKeyDestroyedAt(_ context.Context, principal string) (time.Time, bool, error) {
	at, ok := d.destroyed[principal]
	return at, ok, nil
}

func newFullSealer(t *testing.T, people ...string) (*Sealer, *memKeys, *memDir) {
	t.Helper()
	s, mk := newSealer(t)
	dir := &memDir{ids: map[string]string{}, destroyed: map[string]time.Time{}}
	for _, p := range people {
		dir.ids[p] = uuid.NewString()
	}
	s.Subjects, s.SealActor = dir, true
	return s, mk, dir
}

func humanEvent(actor, action string) types.AuditEvent {
	return types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorHuman, Actor: actor, Action: action, Outcome: "success"}
}

func TestSealFullStoresAHumanActorAsItsSubjectAndReadsItBack(t *testing.T) {
	s, mk, dir := newFullSealer(t, "alice")
	sealed, pending, err := s.Seal(t.Context(), humanEvent("alice", "run.kill"))
	if err != nil || pending {
		t.Fatalf("Seal = pending %v, %v", pending, err)
	}
	if want := SubjectActorPrefix + dir.ids["alice"]; sealed.Actor != want {
		t.Fatalf("stored actor = %q, want %q", sealed.Actor, want)
	}
	if len(mk.keys["alice"]) != 1 {
		t.Errorf("the person's key was not created with their first actor row: %v", mk.keys)
	}
	back, err := s.Unseal(t.Context(), []types.AuditEvent{sealed})
	if err != nil || back[0].Actor != "alice" {
		t.Fatalf("read actor = %v, %v; want alice", back[0].Actor, err)
	}
}

func TestSealFullAfterErasureRendersErasedButNotRowsWrittenAfter(t *testing.T) {
	s, mk, dir := newFullSealer(t, "alice")
	before := humanEvent("alice", "run.kill")
	before, _, _ = s.Seal(t.Context(), before)
	mk.destroy("alice")
	dir.destroyed["alice"] = before.Time.Add(time.Minute)
	after := humanEvent("alice", "run.kill")
	after.Time = before.Time.Add(2 * time.Minute)
	after, _, _ = s.Seal(t.Context(), after)
	back, err := s.Unseal(t.Context(), []types.AuditEvent{before, after})
	if err != nil {
		t.Fatal(err)
	}
	if back[0].Actor != ErasedValue {
		t.Errorf("a row from before the erasure reads %q, want %q", back[0].Actor, ErasedValue)
	}
	if back[1].Actor != "alice" {
		t.Errorf("a row written after the person returned reads %q, want alice", back[1].Actor)
	}
}

func TestSealFullLeavesWhatIsNotAHumanWithAnIdentityAsItIs(t *testing.T) {
	s, _, _ := newFullSealer(t, "alice")
	agent := humanEvent("spiffe://wardyn.local/run/1", "run.start")
	agent.ActorType = types.ActorAgent
	system := humanEvent("admin-token", "setup.write")
	system.ActorType = types.ActorSystem
	for name, ev := range map[string]types.AuditEvent{
		"agent":                    agent,
		"system":                   system,
		"a person with no subject": humanEvent("nobody", "run.kill"),
		"an erasure record":        humanEvent("alice", "person.erasure"),
		"already a subject":        humanEvent(SubjectActorPrefix+uuid.NewString(), "run.kill"),
		"no actor":                 humanEvent("", "run.kill"),
	} {
		got, pending, err := s.Seal(t.Context(), ev)
		if err != nil || pending || got.Actor != ev.Actor {
			t.Errorf("%s: actor %q -> %q (pending %v, err %v), want it unchanged", name, ev.Actor, got.Actor, pending, err)
		}
	}
}

func TestSealFieldsModeLeavesTheActorAloneAndStillReadsSubjects(t *testing.T) {
	s, _, dir := newFullSealer(t, "alice")
	s.SealActor = false
	ev := humanEvent("alice", "run.kill")
	got, _, err := s.Seal(t.Context(), ev)
	if err != nil || got.Actor != "alice" {
		t.Fatalf("mode fields: actor %q, %v; want it as it was", got.Actor, err)
	}
	// A row written while full was on stays readable after it is turned off.
	stored := ev
	stored.Actor = SubjectActorPrefix + dir.ids["alice"]
	back, err := s.Unseal(t.Context(), []types.AuditEvent{stored})
	if err != nil || back[0].Actor != "alice" {
		t.Errorf("a subject actor reads %q, %v; want alice", back[0].Actor, err)
	}
}

func TestSealFullResolvesAnAliasToTheOnePerson(t *testing.T) {
	s, _, dir := newFullSealer(t, "alice-sub")
	s.Resolve = func(_ context.Context, name string) (string, error) {
		if name == "alice@corp.test" {
			return "alice-sub", nil
		}
		return name, nil
	}
	sealed, _, err := s.Seal(t.Context(), humanEvent("alice@corp.test", "run.kill"))
	if err != nil || sealed.Actor != SubjectActorPrefix+dir.ids["alice-sub"] {
		t.Fatalf("actor = %q, %v; want the resolved person's subject", sealed.Actor, err)
	}
	for _, name := range []string{"alice@corp.test", "alice-sub"} {
		got, ok, err := s.StoredActor(t.Context(), name)
		if err != nil || !ok || got != sealed.Actor {
			t.Errorf("StoredActor(%q) = %q, %v, %v; want %q", name, got, ok, err, sealed.Actor)
		}
	}
	if _, ok, _ := s.StoredActor(t.Context(), "nobody"); ok {
		t.Error("a person with no subject was given one")
	}
	if _, ok, _ := s.StoredActor(t.Context(), sealed.Actor); ok {
		t.Error("a filter already naming a subject was translated")
	}
}

func TestSealFullActorWaitsUnderThePendingKeyThenResealsToTheSubject(t *testing.T) {
	s, mk, dir := newFullSealer(t, "alice")
	mk.down = true
	ev := humanEvent("alice", "run.kill")
	ev.Data = []byte(`{"reason":"superseded_by_new_login"}`)
	sealed, pending, err := s.Seal(t.Context(), ev)
	if err != nil || !pending {
		t.Fatalf("Seal = pending %v, %v; want a pending row", pending, err)
	}
	if sealed.Actor != PendingActor || strings.Contains(string(sealed.Data), "alice") {
		t.Fatalf("the pending row names the person: actor %q data %s", sealed.Actor, sealed.Data)
	}
	if !IsPending(sealed) {
		t.Fatalf("not marked pending: %s", sealed.Data)
	}
	if _, err := s.Reseal(t.Context(), sealed); err == nil {
		t.Fatal("Reseal succeeded while the key store was down")
	}
	mk.down = false
	done, err := s.Reseal(t.Context(), sealed)
	if err != nil {
		t.Fatal(err)
	}
	if want := SubjectActorPrefix + dir.ids["alice"]; done.Actor != want {
		t.Errorf("resealed actor = %q, want %q", done.Actor, want)
	}
	if IsPending(done) || strings.Contains(string(done.Data), PendingActorKey) || !strings.Contains(string(done.Data), "superseded_by_new_login") {
		t.Errorf("resealed data = %s, want the marker and the pending actor gone and the rest kept", done.Data)
	}
}

func TestSealFullDirectoryOutageWaitsAndErasedWhileWaitingStoresNoFreshKey(t *testing.T) {
	s, mk, dir := newFullSealer(t, "alice")
	dir.down = true
	sealed, pending, err := s.Seal(t.Context(), humanEvent("alice", "run.kill"))
	if err != nil || !pending {
		t.Fatalf("Seal with the directory down = pending %v, %v", pending, err)
	}
	dir.down = false
	s.GoneSince = func(_ context.Context, subject string, _ time.Time) (bool, error) { return subject == "alice", nil }
	done, err := s.Reseal(t.Context(), sealed)
	if err != nil {
		t.Fatal(err)
	}
	if done.Actor != SubjectActorPrefix+dir.ids["alice"] {
		t.Errorf("actor = %q, want the subject", done.Actor)
	}
	if len(mk.keys["alice"]) != 0 {
		t.Errorf("a key was minted for a person erased while their row waited: %v", mk.keys)
	}
	// Erased and no subject to store: the name is never written.
	dir.ids = map[string]string{}
	sealed.Actor = PendingActor
	done, err = s.Reseal(t.Context(), sealed)
	if err != nil || done.Actor != ErasedValue {
		t.Errorf("an erased person with no subject = %q, %v; want %q", done.Actor, err, ErasedValue)
	}
}

func TestSealFullFailsRatherThanWriteTheActorInTheClear(t *testing.T) {
	s, mk, _ := newFullSealer(t, "alice")
	s.Pending = nil
	mk.down = true
	if _, _, err := s.Seal(t.Context(), humanEvent("alice", "run.kill")); err == nil {
		t.Fatal("Seal wrote the actor with no key and no pending key")
	}
}

func TestUnsealReadsAnUnknownSubjectAsStoredAndAsksOncePerSubject(t *testing.T) {
	s, _, dir := newFullSealer(t, "alice")
	unknown := humanEvent(SubjectActorPrefix+uuid.NewString(), "run.kill")
	var evs []types.AuditEvent
	for range 4 {
		ev, _, _ := s.Seal(t.Context(), humanEvent("alice", "run.kill"))
		evs = append(evs, ev)
	}
	evs = append(evs, unknown)
	back, err := s.Unseal(t.Context(), evs)
	if err != nil {
		t.Fatal(err)
	}
	if back[4].Actor != unknown.Actor {
		t.Errorf("an id no identity carries reads %q, want it as stored", back[4].Actor)
	}
	dir.down = true
	if _, err := s.Unseal(t.Context(), evs); err == nil {
		t.Error("Unseal answered with the directory down; a subject id would pass for the record")
	}
}
