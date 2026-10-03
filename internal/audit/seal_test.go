// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// memKeys is a SealKeys over a map, with destroy and outage switches.
type memKeys struct {
	keys      map[string][][]byte // owner -> key per version (index+1); nil entry = destroyed
	down      bool
	keyCalls  int
	currCalls int
}

func (m *memKeys) Current(_ context.Context, owner, purpose string) (int, []byte, error) {
	m.currCalls++
	if m.down {
		return 0, nil, errors.New("key store down")
	}
	vs := m.keys[owner]
	if n := len(vs); n > 0 && vs[n-1] != nil {
		return n, append([]byte(nil), vs[n-1]...), nil
	}
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	m.keys[owner] = append(vs, k)
	return len(m.keys[owner]), append([]byte(nil), k...), nil
}

func (m *memKeys) Key(_ context.Context, owner, purpose string, version int) ([]byte, error) {
	m.keyCalls++
	if m.down {
		return nil, errors.New("key store down")
	}
	vs := m.keys[owner]
	if version < 1 || version > len(vs) || vs[version-1] == nil {
		return nil, ErrKeyErased
	}
	return append([]byte(nil), vs[version-1]...), nil
}

func (m *memKeys) destroy(owner string) {
	for i := range m.keys[owner] {
		m.keys[owner][i] = nil
	}
}

func newSealer(t *testing.T) (*Sealer, *memKeys) {
	t.Helper()
	mk := &memKeys{keys: map[string][][]byte{}}
	pk := make([]byte, 32)
	_, _ = rand.Read(pk)
	return &Sealer{Keys: mk, Pending: func() []byte { return pk }}, mk
}

func decideEvent(actor, reason string) types.AuditEvent {
	data, _ := json.Marshal(map[string]any{"approval_id": "a1", "decision": "APPROVED", "reason": reason, "host": "example.test"})
	return types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorHuman, Actor: actor,
		Action: "approval.decide", Target: "a1", Outcome: "success", Data: data,
	}
}

func field(t *testing.T, ev types.AuditEvent, key string) any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(ev.Data, &m); err != nil {
		t.Fatal(err)
	}
	return m[key]
}

func TestSealThenUnsealRoundTrips(t *testing.T) {
	s, _ := newSealer(t)
	ev := decideEvent("alice", "needed for the release")
	sealed, pending, err := s.Seal(t.Context(), ev)
	if err != nil || pending {
		t.Fatalf("Seal = pending %v, %v", pending, err)
	}
	if strings.Contains(string(sealed.Data), "needed for the release") {
		t.Fatalf("plaintext survived sealing: %s", sealed.Data)
	}
	if got := field(t, sealed, "host"); got != "example.test" {
		t.Errorf("an unsealed field changed: host = %v", got)
	}
	back, err := s.Unseal(t.Context(), []types.AuditEvent{sealed})
	if err != nil {
		t.Fatal(err)
	}
	if got := field(t, back[0], "reason"); got != "needed for the release" {
		t.Errorf("reason = %v, want the original", got)
	}
}

func TestSealLeavesWhatIsNotListedClear(t *testing.T) {
	s, _ := newSealer(t)
	for name, ev := range map[string]types.AuditEvent{
		"machine reason on another action": {ID: uuid.New(), Action: "run.kill", ActorType: types.ActorHuman, Actor: "alice", Data: json.RawMessage(`{"reason":"superseded_by_new_login"}`)},
		"agent actor":                      {ID: uuid.New(), Action: "approval.decide", ActorType: types.ActorSystem, Actor: "admin-token", Data: json.RawMessage(`{"reason":"ok"}`)},
		"empty reason":                     decideEvent("alice", ""),
		"not an object":                    {ID: uuid.New(), Action: "approval.decide", ActorType: types.ActorHuman, Actor: "alice", Data: json.RawMessage(`"x"`)},
	} {
		got, pending, err := s.Seal(t.Context(), ev)
		if err != nil || pending || string(got.Data) != string(ev.Data) {
			t.Errorf("%s: Seal changed the row (pending %v, err %v): %s", name, pending, err, got.Data)
		}
	}
}

func TestSealSealsEachPersonsFieldUnderThatPersonsKey(t *testing.T) {
	s, mk := newSealer(t)
	// person.create: the email is the created person's (target), whoever the admin is.
	data, _ := json.Marshal(map[string]any{"email": "bob@corp.test", "issuer": "https://idp.test"})
	ev := types.AuditEvent{ID: uuid.New(), Time: time.Now(), ActorType: types.ActorHuman, Actor: "admin-alice", Action: "person.create", Target: "bob", Outcome: "success", Data: data}
	sealed, _, err := s.Seal(t.Context(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mk.keys["bob"]; !ok || len(mk.keys["admin-alice"]) != 0 {
		t.Fatalf("keys minted for %v, want bob only", mk.keys)
	}
	mk.destroy("bob")
	back, err := s.Unseal(t.Context(), []types.AuditEvent{sealed})
	if err != nil {
		t.Fatal(err)
	}
	if got := field(t, back[0], "email"); got != ErasedValue {
		t.Errorf("email after bob's erasure = %v, want %q", got, ErasedValue)
	}
	if got := field(t, back[0], "issuer"); got != "https://idp.test" {
		t.Errorf("issuer = %v, want it untouched", got)
	}
}

func TestSealedValueDoesNotOpenOnAnotherEventFieldOrPerson(t *testing.T) {
	s, _ := newSealer(t)
	a, _, _ := s.Seal(t.Context(), decideEvent("alice", "first"))
	b := decideEvent("alice", "second")
	// Move a's ciphertext onto b's row.
	b.Data = a.Data
	got, err := s.Unseal(t.Context(), []types.AuditEvent{b})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := field(t, got[0], "reason").(string); !strings.HasPrefix(v, "seal1.") {
		t.Errorf("a ciphertext moved to another event opened: reason = %q", v)
	}
	// Moved to another action's path.
	c := a
	c.Action = "person.create"
	c.Target = "alice"
	moved := strings.Replace(string(a.Data), `"reason"`, `"email"`, 1)
	c.Data = json.RawMessage(moved)
	got, _ = s.Unseal(t.Context(), []types.AuditEvent{c})
	if v, _ := field(t, got[0], "email").(string); !strings.HasPrefix(v, "seal1.") {
		t.Errorf("a ciphertext moved to another action opened: email = %q", v)
	}
}

func TestSealFallsBackToThePendingKeyAndResealsOnReplay(t *testing.T) {
	s, mk := newSealer(t)
	mk.down = true
	ev := decideEvent("alice", "typed while the key store was down")
	sealed, pending, err := s.Seal(t.Context(), ev)
	if err != nil || !pending {
		t.Fatalf("Seal = pending %v, %v; want a pending row", pending, err)
	}
	if strings.Contains(string(sealed.Data), "typed while") {
		t.Fatalf("plaintext in a pending row: %s", sealed.Data)
	}
	if !IsPending(sealed) || field(t, sealed, PendingMarker) != true {
		t.Fatalf("pending row not marked: %s", sealed.Data)
	}

	// Still down: the drain keeps the line.
	if _, err := s.Reseal(t.Context(), sealed); err == nil {
		t.Fatal("Reseal succeeded while the key store was down")
	}
	mk.down = false
	resealed, err := s.Reseal(t.Context(), sealed)
	if err != nil {
		t.Fatal(err)
	}
	if IsPending(resealed) || field(t, resealed, PendingMarker) != nil {
		t.Fatalf("still pending after reseal: %s", resealed.Data)
	}
	back, err := s.Unseal(t.Context(), []types.AuditEvent{resealed})
	if err != nil || field(t, back[0], "reason") != "typed while the key store was down" {
		t.Fatalf("resealed row reads %v, %v", back, err)
	}
	mk.destroy("alice")
	back, _ = s.Unseal(t.Context(), []types.AuditEvent{resealed})
	if field(t, back[0], "reason") != ErasedValue {
		t.Errorf("after erasure the resealed row reads %v, want erased", field(t, back[0], "reason"))
	}
}

func TestSealFailsRatherThanWriteInTheClearWithNoPendingKey(t *testing.T) {
	s, mk := newSealer(t)
	s.Pending = nil
	mk.down = true
	if _, _, err := s.Seal(t.Context(), decideEvent("alice", "secret words")); err == nil {
		t.Fatal("Seal succeeded with no key and no pending key")
	}
}

func TestResealErasesAFieldWhoseSubjectWasErasedWhileItWaited(t *testing.T) {
	s, mk := newSealer(t)
	mk.down = true
	ev := decideEvent("alice", "waiting")
	sealed, _, _ := s.Seal(t.Context(), ev)
	mk.down = false
	s.GoneSince = func(_ context.Context, subject string, since time.Time) (bool, error) {
		return subject == "alice" && since.Before(time.Now()), nil
	}
	resealed, err := s.Reseal(t.Context(), sealed)
	if err != nil {
		t.Fatal(err)
	}
	if got := field(t, resealed, "reason"); got != ErasedValue {
		t.Errorf("reason = %v, want erased, not sealed under a fresh key", got)
	}
	if len(mk.keys["alice"]) != 0 {
		t.Errorf("a key was minted for an erased subject: %v", mk.keys)
	}
}

func TestSealResolvesAliasesToOnePerson(t *testing.T) {
	s, mk := newSealer(t)
	s.Resolve = func(_ context.Context, name string) (string, error) {
		if name == "Alice@Corp.Test" {
			return "alice-sub", nil
		}
		return name, nil
	}
	sealed, _, err := s.Seal(t.Context(), decideEvent("Alice@Corp.Test", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if len(mk.keys["alice-sub"]) != 1 || len(mk.keys["Alice@Corp.Test"]) != 0 {
		t.Fatalf("key under %v, want the resolved principal", mk.keys)
	}
	_, subject, _, ok := parseSealed(field(t, sealed, "reason").(string))
	if !ok || subject != "alice-sub" {
		t.Errorf("sealed subject = %q, want alice-sub", subject)
	}
}

func TestUnsealFailsOnAKeyStoreOutage(t *testing.T) {
	s, mk := newSealer(t)
	sealed, _, _ := s.Seal(t.Context(), decideEvent("alice", "x"))
	mk.down = true
	if _, err := s.Unseal(t.Context(), []types.AuditEvent{sealed}); err == nil {
		t.Fatal("Unseal answered with the key store down; ciphertext would pass for the record")
	}
}

func TestUnsealAsksForEachKeyOncePerRead(t *testing.T) {
	s, mk := newSealer(t)
	var evs []types.AuditEvent
	for range 5 {
		ev, _, _ := s.Seal(t.Context(), decideEvent("alice", "x"))
		evs = append(evs, ev)
	}
	before := mk.keyCalls
	if _, err := s.Unseal(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	if n := mk.keyCalls - before; n != 1 {
		t.Errorf("Key called %d times for 5 rows of one person, want 1", n)
	}
}

func TestUnsealedActionsAreNeverSealed(t *testing.T) {
	for _, a := range UnsealedActions {
		if SealsAction(a) {
			t.Errorf("%s is listed as fully unsealed and also has sealed fields", a)
		}
	}
	for action, fs := range sealFields {
		for _, f := range fs {
			leaf := f.Path[strings.LastIndex(f.Path, ".")+1:]
			found := false
			for _, n := range SealLeafNames {
				found = found || n == leaf
			}
			if !found {
				t.Errorf("%s seals %q, which is not a name in SealLeafNames", action, f.Path)
			}
		}
	}
}

func TestRewriteWalksDottedPathsAndLeavesAMissingPathAlone(t *testing.T) {
	obj := json.RawMessage(`{"a":{"b":"x","c":1},"d":2}`)
	upper := func(json.RawMessage) (json.RawMessage, bool, error) { return json.RawMessage(`"Y"`), true, nil }
	got, changed, err := rewrite(obj, []string{"a", "b"}, upper)
	if err != nil || !changed || string(got) != `{"a":{"b":"Y","c":1},"d":2}` {
		t.Fatalf("nested rewrite = %s, %v, %v", got, changed, err)
	}
	for _, path := range [][]string{{"a", "zz"}, {"zz"}, {"d", "x"}} {
		got, changed, err := rewrite(obj, path, upper)
		if err != nil || changed || string(got) != string(obj) {
			t.Errorf("path %v: %s, %v, %v; want the object untouched", path, got, changed, err)
		}
	}
}

func TestParseSealMode(t *testing.T) {
	for in, want := range map[string]SealMode{"": SealOff, "off": SealOff, "fields": SealFields, "full": SealFull} {
		if got, err := ParseSealMode(in); err != nil || got != want {
			t.Errorf("ParseSealMode(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParseSealMode("fileds"); err == nil {
		t.Error("a misspelt mode was accepted")
	}
}
