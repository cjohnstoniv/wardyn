// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func serviceEvent(action, actor string, data string) types.AuditEvent {
	return types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorService, Actor: actor,
		Action: action, Outcome: "success", Data: json.RawMessage(data),
	}
}

func dataField(t *testing.T, ev types.AuditEvent, key string) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(ev.Data, &m); err != nil {
		t.Fatal(err)
	}
	var s string
	_ = json.Unmarshal(m[key], &s)
	return s
}

// TestServiceIdentity_SponsorFieldsSealed: under SealFull a sponsor named on a service row, on a
// service_identity.* row (sponsor, sponsor_from, sponsor_to) and an invitation row (target) is stored as the
// person's subject, read back as the person, and read as erased once their key is destroyed. The service
// name in the actor column is not personal data and stays.
func TestServiceIdentity_SponsorFieldsSealed(t *testing.T) {
	s, mk, dir := newFullSealer(t, "alice", "bob", "carol")
	subject := func(p string) string { return SubjectActorPrefix + dir.ids[p] }
	cases := []struct {
		ev     types.AuditEvent
		fields map[string]string // data key -> person
	}{
		{serviceEvent("run.create", "service:ci", `{"sponsor":"alice","x":1}`), map[string]string{"sponsor": "alice"}},
		{func() types.AuditEvent {
			ev := serviceEvent("service_identity.update", "admin-token", `{"sponsor_from":"alice","sponsor_to":"bob","state":"active"}`)
			ev.ActorType = types.ActorSystem
			return ev
		}(), map[string]string{"sponsor_from": "alice", "sponsor_to": "bob"}},
		{serviceEvent("service_identity.create", "service:ci", `{"sponsor":"carol"}`), map[string]string{"sponsor": "carol"}},
		{func() types.AuditEvent {
			ev := serviceEvent("invitation.create", "root", `{"target":"bob","replaces_token_id":"none"}`)
			ev.ActorType = types.ActorSystem
			return ev
		}(), map[string]string{"target": "bob"}},
	}
	for _, c := range cases {
		sealed, pending, err := s.Seal(t.Context(), c.ev)
		if err != nil || pending {
			t.Fatalf("%s: Seal = pending %v, %v", c.ev.Action, pending, err)
		}
		if sealed.Actor != c.ev.Actor {
			t.Errorf("%s: actor %q became %q; a service name is not personal data", c.ev.Action, c.ev.Actor, sealed.Actor)
		}
		for key, person := range c.fields {
			if got := dataField(t, sealed, key); got != subject(person) {
				t.Errorf("%s: stored %s = %q, want %q", c.ev.Action, key, got, subject(person))
			}
		}
		if strings.Contains(string(sealed.Data), `"alice"`) || strings.Contains(string(sealed.Data), `"bob"`) || strings.Contains(string(sealed.Data), `"carol"`) {
			t.Errorf("%s: sealed data still names a person: %s", c.ev.Action, sealed.Data)
		}
		back, err := s.Unseal(t.Context(), []types.AuditEvent{sealed})
		if err != nil {
			t.Fatal(err)
		}
		for key, person := range c.fields {
			if got := dataField(t, back[0], key); got != person {
				t.Errorf("%s: read %s = %q, want %q", c.ev.Action, key, got, person)
			}
		}
	}
	// After the sponsor's erasure the service's trail names nobody.
	sealed, _, _ := s.Seal(t.Context(), serviceEvent("run.create", "service:ci", `{"sponsor":"alice"}`))
	mk.destroy("alice")
	dir.destroyed["alice"] = sealed.Time.Add(time.Minute)
	back, err := s.Unseal(t.Context(), []types.AuditEvent{sealed})
	if err != nil || dataField(t, back[0], "sponsor") != ErasedValue {
		t.Errorf("sponsor after erasure = %q (%v), want %q", dataField(t, back[0], "sponsor"), err, ErasedValue)
	}
	// A sponsor with no identity row keeps their name, as an actor does.
	kept, _, err := s.Seal(t.Context(), serviceEvent("run.create", "service:ci", `{"sponsor":"never-signed-in"}`))
	if err != nil || dataField(t, kept, "sponsor") != "never-signed-in" {
		t.Errorf("sponsor with no identity row = %q (%v), want the name", dataField(t, kept, "sponsor"), err)
	}
}

// TestServiceIdentity_SponsorWaitsUnderThePendingKey: with the subject directory or key store down the
// sponsor waits sealed under the pending key (the row is pending, the name absent), and Reseal stores the
// subject, or ErasedValue for a sponsor erased while it waited.
func TestServiceIdentity_SponsorWaitsUnderThePendingKey(t *testing.T) {
	s, mk, dir := newFullSealer(t, "alice")
	mk.down = true
	ev := serviceEvent("run.create", "service:ci", `{"sponsor":"alice","kept":"yes"}`)
	sealed, pending, err := s.Seal(t.Context(), ev)
	if err != nil || !pending || !IsPending(sealed) {
		t.Fatalf("Seal with the key store down = pending %v, %v; want a pending row", pending, err)
	}
	if strings.Contains(string(sealed.Data), "alice") {
		t.Fatalf("the pending row names the sponsor: %s", sealed.Data)
	}
	if _, err := s.Reseal(t.Context(), sealed); err == nil {
		t.Fatal("Reseal succeeded while the key store was down")
	}
	mk.down = false
	done, err := s.Reseal(t.Context(), sealed)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := dataField(t, done, "sponsor"), SubjectActorPrefix+dir.ids["alice"]; got != want || IsPending(done) || dataField(t, done, "kept") != "yes" {
		t.Errorf("resealed data = %s, want sponsor %q, the marker gone and the rest kept", done.Data, want)
	}
	s.GoneSince = func(_ context.Context, subject string, _ time.Time) (bool, error) { return subject == "alice", nil }
	gone, err := s.Reseal(t.Context(), sealed)
	if err != nil || dataField(t, gone, "sponsor") != SubjectActorPrefix+dir.ids["alice"] {
		t.Errorf("sponsor erased while waiting = %s (%v), want its subject without a fresh key", gone.Data, err)
	}
}

// TestServiceIdentity_ForgedSponsorSubjectIsRefused: a sponsor field that already begins with subject: was
// written by no Seal, and a reader would render it as the person it names.
func TestServiceIdentity_ForgedSponsorSubjectIsRefused(t *testing.T) {
	s, _, dir := newFullSealer(t, "alice")
	ev := serviceEvent("run.create", "service:ci", `{"sponsor":"`+SubjectActorPrefix+dir.ids["alice"]+`"}`)
	if _, _, err := s.Seal(t.Context(), ev); err == nil {
		t.Error("Seal accepted a forged subject in data.sponsor")
	}
	if !SealsSponsor(serviceEvent("run.create", "service:ci", `{"sponsor":"alice"}`)) || SealsSponsor(serviceEvent("run.create", "service:ci", `{"x":1}`)) {
		t.Error("SealsSponsor does not tell a row with a sponsor from one without")
	}
}

// Under SealFields and SealOff the sponsor stays as it is, as an actor does.
func TestServiceIdentity_SponsorStaysClearOutsideSealFull(t *testing.T) {
	s, _ := newSealer(t)
	sealed, pending, err := s.Seal(t.Context(), serviceEvent("run.create", "service:ci", `{"sponsor":"alice"}`))
	if err != nil || pending || dataField(t, sealed, "sponsor") != "alice" {
		t.Errorf("without SealActor: sponsor %q pending %v (%v), want it unchanged", dataField(t, sealed, "sponsor"), pending, err)
	}
}
