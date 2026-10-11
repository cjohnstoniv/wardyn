// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// personTargetEvent is a row of a person-target action: an admin acting on a
// person named in the target column.
func personTargetEvent(action, target string) types.AuditEvent {
	return types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorHuman, Actor: "admin",
		Action: action, Target: target, Outcome: "success", Data: []byte(`{"email":"alice@corp.test"}`),
	}
}

func TestSeal_FullStoresAPersonTargetAsItsSubject(t *testing.T) {
	s, mk, dir := newFullSealer(t, "alice")
	sealed, pending, err := s.Seal(t.Context(), personTargetEvent("person.create", "alice"))
	if err != nil || pending {
		t.Fatalf("Seal = pending %v, %v", pending, err)
	}
	if want := SubjectActorPrefix + dir.ids["alice"]; sealed.Target != want {
		t.Fatalf("stored target = %q, want %q", sealed.Target, want)
	}
	if sealed.Actor != "admin" {
		t.Errorf("an actor with no identity row = %q, want it kept", sealed.Actor)
	}
	if len(mk.keys["alice"]) != 1 {
		t.Errorf("the person's key was not created with the first row that names them: %v", mk.keys)
	}

	// fields keeps the name, as does a target of an action off the list.
	s.SealActor = false
	if got, _, err := s.Seal(t.Context(), personTargetEvent("person.create", "alice")); err != nil || got.Target != "alice" {
		t.Errorf("mode fields: target %q, %v; want the name", got.Target, err)
	}
	s.SealActor = true
	for name, ev := range map[string]types.AuditEvent{
		"an action off the list":    personTargetEvent("token.create", "alice"),
		"a person with no identity": personTargetEvent("person.create", "nobody"),
		"already a subject":         personTargetEvent("person.create", SubjectActorPrefix+uuid.NewString()),
		"no target":                 personTargetEvent("person.create", ""),
	} {
		if got, _, err := s.Seal(t.Context(), ev); err != nil || got.Target != ev.Target {
			t.Errorf("%s: target %q -> %q, %v; want it unchanged", name, ev.Target, got.Target, err)
		}
	}
}

func TestSeal_TargetOfAnUnsealedActionStaysClear(t *testing.T) {
	s, _, _ := newFullSealer(t, "alice")
	for _, a := range PersonTargetActions() {
		if slices.Contains(UnsealedActions, a) {
			t.Errorf("%s is a person-target action and also an unsealed one", a)
		}
	}
	// Even listed by mistake, an unsealed action's row keeps the person: the
	// record of an erasure must survive the key it destroys.
	saved := personTargetActions
	personTargetActions = append(slices.Clone(saved), UnsealedActions...)
	t.Cleanup(func() { personTargetActions = saved })
	for _, a := range UnsealedActions {
		got, _, err := s.Seal(t.Context(), personTargetEvent(a, "alice"))
		if err != nil || got.Target != "alice" {
			t.Errorf("%s: target %q, %v; want the principal in the clear", a, got.Target, err)
		}
	}
}

func TestUnseal_RendersATargetSubjectAndErasedAfterKeyDestroyed(t *testing.T) {
	s, mk, dir := newFullSealer(t, "alice")
	before, _, _ := s.Seal(t.Context(), personTargetEvent("person.create", "alice"))
	back, err := s.Unseal(t.Context(), []types.AuditEvent{before})
	if err != nil || back[0].Target != "alice" {
		t.Fatalf("read target = %q, %v; want alice", back[0].Target, err)
	}
	if before.Target == "alice" {
		t.Fatal("the stored target names the person")
	}
	mk.destroy("alice")
	dir.destroyed["alice"] = before.Time.Add(time.Minute)
	after := personTargetEvent("person.create", "alice")
	after.Time = before.Time.Add(2 * time.Minute)
	after, _, _ = s.Seal(t.Context(), after)
	back, err = s.Unseal(t.Context(), []types.AuditEvent{before, after})
	if err != nil {
		t.Fatal(err)
	}
	if back[0].Target != ErasedValue {
		t.Errorf("a target written before the erasure reads %q, want %q", back[0].Target, ErasedValue)
	}
	if back[1].Target != "alice" {
		t.Errorf("a target written after the person returned reads %q, want alice", back[1].Target)
	}
	// An id no identity row carries, and a subject-looking target off the list, read as stored.
	unknown := personTargetEvent("person.create", SubjectActorPrefix+uuid.NewString())
	offList := personTargetEvent("token.create", SubjectActorPrefix+dir.ids["alice"])
	back, err = s.Unseal(t.Context(), []types.AuditEvent{unknown, offList})
	if err != nil || back[0].Target != unknown.Target || back[1].Target != offList.Target {
		t.Errorf("unknown or off-list subject targets = %q, %q, %v; want them as stored", back[0].Target, back[1].Target, err)
	}
}

func TestReseal_PendingTargetOpensAsItsSubject(t *testing.T) {
	s, mk, dir := newFullSealer(t, "alice")
	mk.down = true
	sealed, pending, err := s.Seal(t.Context(), personTargetEvent("person.create", "alice"))
	if err != nil || !pending {
		t.Fatalf("Seal = pending %v, %v; want a pending row", pending, err)
	}
	if sealed.Target != PendingTarget || strings.Contains(string(sealed.Data), "alice\"") || !IsPending(sealed) {
		t.Fatalf("the pending row names the person or is not marked: target %q data %s", sealed.Target, sealed.Data)
	}
	if _, err := s.Reseal(t.Context(), sealed); err == nil {
		t.Fatal("Reseal succeeded while the key store was down")
	}
	mk.down = false
	done, err := s.Reseal(t.Context(), sealed)
	if err != nil {
		t.Fatal(err)
	}
	if want := SubjectActorPrefix + dir.ids["alice"]; done.Target != want {
		t.Errorf("resealed target = %q, want %q", done.Target, want)
	}
	if IsPending(done) || strings.Contains(string(done.Data), PendingTargetKey) {
		t.Errorf("resealed data = %s, want the marker and the pending target gone", done.Data)
	}
	back, err := s.Unseal(t.Context(), []types.AuditEvent{done})
	if err != nil || back[0].Target != "alice" || !strings.Contains(string(back[0].Data), "alice@corp.test") {
		t.Errorf("the resealed row reads target %q data %s, %v; want the person and the email back", back[0].Target, back[0].Data, err)
	}

	// Erased while waiting: the subject without a fresh key; with no subject, the erased value.
	mk.keys = map[string][]memKey{}
	s.GoneSince = func(_ context.Context, subject string, _ time.Time) (bool, error) { return subject == "alice", nil }
	done, err = s.Reseal(t.Context(), sealed)
	if err != nil || done.Target != SubjectActorPrefix+dir.ids["alice"] || len(mk.keys["alice"]) != 0 {
		t.Errorf("erased while waiting: target %q, keys %v, %v; want the subject and no fresh key", done.Target, mk.keys, err)
	}
	dir.ids = map[string]string{}
	if done, err = s.Reseal(t.Context(), sealed); err != nil || done.Target != ErasedValue {
		t.Errorf("erased with no subject: target %q, %v; want %q", done.Target, err, ErasedValue)
	}
}

// A row that also waits on its actor parks both, and the drain stores both.
func TestReseal_PendingActorAndTargetBothOpen(t *testing.T) {
	s, mk, dir := newFullSealer(t, "alice", "bob")
	mk.down = true
	ev := personTargetEvent("person.attach", "alice")
	ev.Actor = "bob"
	sealed, pending, err := s.Seal(t.Context(), ev)
	if err != nil || !pending || sealed.Actor != PendingActor || sealed.Target != PendingTarget {
		t.Fatalf("Seal = %q/%q pending %v, %v", sealed.Actor, sealed.Target, pending, err)
	}
	mk.down = false
	done, err := s.Reseal(t.Context(), sealed)
	if err != nil || done.Actor != SubjectActorPrefix+dir.ids["bob"] || done.Target != SubjectActorPrefix+dir.ids["alice"] {
		t.Errorf("resealed %q/%q, %v; want both subjects", done.Actor, done.Target, err)
	}
}

func TestPersonTargetActionsAreDocumented(t *testing.T) {
	raw, err := os.ReadFile("../../docs/AUDIT-ACTIONS.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	_, rest, ok := strings.Cut(doc, "\n## Sealed fields\n")
	if !ok {
		t.Fatal("docs/AUDIT-ACTIONS.md has no Sealed fields section")
	}
	section, _, _ := strings.Cut(rest, "\n## ")
	for _, a := range PersonTargetActions() {
		if !strings.Contains(doc, "\n| `"+a+"` |") {
			t.Errorf("%s is a person-target action with no row in docs/AUDIT-ACTIONS.md", a)
		}
		if !strings.Contains(section, "`"+a+"`") {
			t.Errorf("%s is a person-target action the Sealed fields section does not name", a)
		}
	}
}
