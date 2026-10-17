// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/testutil"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// namesNoOne fails when stored spells any of people in any way a reader undoes
// without a key.
func namesNoOne(t *testing.T, where, stored string, people ...string) {
	t.Helper()
	for _, p := range people {
		for _, f := range testutil.Spellings(p) {
			if strings.Contains(stored, f) {
				t.Errorf("%s names %q (as %q): %s", where, p, f, stored)
			}
		}
	}
}

// The person a sealed field describes is never stored beside its ciphertext,
// in any encoding: not the name the event carried, not the principal it
// resolved to. Under full the whole row (actor and data) names no one; under
// fields the data does not. This holds for a row sealed under the person's key
// and for one waiting under the pending key.
func TestSealedRowNamesNoPersonInAnyEncoding(t *testing.T) {
	const principal = "alice-principal-7f3a"
	aliases := []string{"alice.cooper@corp.test", "entra:5d1e7a0c-tenant:9b2f-object-41c8"}
	people := append([]string{principal}, aliases...)
	resolve := func(_ context.Context, name string) (string, error) {
		for _, a := range aliases {
			if name == a {
				return principal, nil
			}
		}
		return name, nil
	}
	reject := func(actor string) types.AuditEvent {
		data, _ := json.Marshal(map[string]any{"proposed_by": "proposer", "reason": "the words the admin typed", "target_key": "p"})
		return types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorHuman, Actor: actor,
			Action: "governance.change.reject", Target: uuid.NewString(), Outcome: "success", Data: data}
	}
	for _, full := range []bool{false, true} {
		t.Run(map[bool]string{false: "fields", true: "full"}[full], func(t *testing.T) {
			for _, down := range []bool{false, true} {
				s, mk, _ := newFullSealer(t, principal)
				s.SealActor, s.Resolve, mk.down = full, resolve, down
				for _, actor := range people {
					got, pending, err := s.Seal(t.Context(), reject(actor))
					if err != nil || pending != down {
						t.Fatalf("Seal(%s) = pending %v, %v; want pending %v", actor, pending, err, down)
					}
					stored := string(got.Data)
					if full {
						stored = got.Actor + " " + stored
					}
					namesNoOne(t, map[bool]string{false: "a sealed row", true: "a pending row"}[down], stored, people...)
				}
			}
		})
	}
	// The created person's email is sealed under their key; the data names them nowhere.
	s, _, _ := newFullSealer(t, principal)
	s.Resolve = resolve
	data, _ := json.Marshal(map[string]any{"email": aliases[0], "issuer": "https://idp.test"})
	got, _, err := s.Seal(t.Context(), types.AuditEvent{ID: uuid.New(), Time: time.Now(), ActorType: types.ActorHuman,
		Actor: "admin", Action: "person.create", Target: principal, Outcome: "success", Data: data})
	if err != nil {
		t.Fatal(err)
	}
	namesNoOne(t, "person.create data", string(got.Data), people...)
}

// Every name a person goes by resolves to one principal, so their fields seal
// under one key and name it by one handle; another person's is another handle.
func TestSealedFieldsOfOnePersonShareOneHandle(t *testing.T) {
	s, mk := newSealer(t)
	s.Resolve = func(_ context.Context, name string) (string, error) {
		if name == "Alice@Corp.Test" || name == "entra:tid:oid" {
			return "alice-sub", nil
		}
		return name, nil
	}
	handleOf := func(actor string) uuid.UUID {
		t.Helper()
		sealed, _, err := s.Seal(t.Context(), decideEvent(actor, "x"))
		if err != nil {
			t.Fatal(err)
		}
		h, _, ok := parseSealed(field(t, sealed, "reason").(string))
		if !ok {
			t.Fatalf("%s's reason is not sealed: %s", actor, sealed.Data)
		}
		return h
	}
	want := handleOf("alice-sub")
	for _, alias := range []string{"Alice@Corp.Test", "entra:tid:oid"} {
		if got := handleOf(alias); got != want {
			t.Errorf("%s seals under handle %s, want alice-sub's %s", alias, got, want)
		}
	}
	if len(mk.keys) != 1 || len(mk.keys["alice-sub"]) != 1 {
		t.Errorf("keys %v, want one generation of alice-sub's", mk.keys)
	}
	if handleOf("bob") == want {
		t.Error("two people share a handle")
	}
}

// The handle is bound into the ciphertext's additional data: re-pointed at
// another handle, a sealed field does not open, even when that handle serves
// the very same key bytes. A handle no key carries reads erased, as a destroyed
// one does.
func TestSealedFieldRepointedAtAnotherHandleDoesNotOpen(t *testing.T) {
	s, mk := newSealer(t)
	sealed, _, err := s.Seal(t.Context(), decideEvent("alice", "alice's words"))
	if err != nil {
		t.Fatal(err)
	}
	alice := mk.keys["alice"][0]
	twin := memKey{handle: uuid.New(), key: append([]byte(nil), alice.key...)}
	mk.keys["mallory"] = []memKey{twin}
	if _, _, err := s.Seal(t.Context(), decideEvent("bob", "y")); err != nil {
		t.Fatal(err)
	}
	for name, to := range map[string]uuid.UUID{"the same key under another handle": twin.handle, "another person's key": mk.keys["bob"][0].handle} {
		moved := sealed
		moved.Data = json.RawMessage(strings.Replace(string(sealed.Data), alice.handle.String(), to.String(), 1))
		got, err := s.Unseal(t.Context(), []types.AuditEvent{moved})
		if err != nil {
			t.Fatal(err)
		}
		if v, _ := field(t, got[0], "reason").(string); !strings.HasPrefix(v, sealedPrefix+to.String()) {
			t.Errorf("%s: a field re-pointed at handle %s reads %q, want it left sealed", name, to, v)
		}
	}
	unknown := sealed
	unknown.Data = json.RawMessage(strings.Replace(string(sealed.Data), alice.handle.String(), uuid.NewString(), 1))
	got, err := s.Unseal(t.Context(), []types.AuditEvent{unknown})
	if err != nil || field(t, got[0], "reason") != ErasedValue {
		t.Errorf("a handle no key carries reads %v, %v; want %q", field(t, got[0], "reason"), err, ErasedValue)
	}
}
