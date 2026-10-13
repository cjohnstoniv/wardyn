// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// WARDYN_AUDIT_SEAL=full through the real audit chain, a real Postgres and the
// real spool (the rig is audit_seal_pg_test.go's): a human actor is stored as
// the person's subject, reads and an actor filter resolve it back, and erasing
// the person makes the row read erased while the chain still verifies. Guarded
// by WARDYN_TEST_PG; skipped cleanly when unset.

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// actorsStored is every audit row's stored actor of one action, oldest first.
func (rg *sealRig) actorsStored(action string) []string {
	rg.t.Helper()
	rows, err := rg.pool.Query(rg.t.Context(), `SELECT actor FROM audit_events WHERE action = $1 ORDER BY seq`, action)
	if err != nil {
		rg.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rg.t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// actorsRead is the actor of every approval.decide row a reader sees through h,
// narrowed by ?actor= when actor is not "", sorted.
func (rg *sealRig) actorsRead(h http.Handler, actor string) []string {
	rg.t.Helper()
	path := "/api/v1/audit?action=approval.decide"
	if actor != "" {
		path += "&actor=" + actor
	}
	w := rg.call(h, http.MethodGet, path, "")
	if w.Code != http.StatusOK {
		rg.t.Fatalf("audit read = %d %s", w.Code, w.Body)
	}
	var evs []types.AuditEvent
	if err := json.Unmarshal(w.Body.Bytes(), &evs); err != nil {
		rg.t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		out = append(out, ev.Actor)
	}
	slices.Sort(out)
	return out
}

func TestPG_AuditSeal_FullStoresTheActorAsASubjectAndErasureShredsIt(t *testing.T) {
	rg := newSealRigMode(t, audit.SealFull)
	ctx := t.Context()

	subjects := map[string]string{}
	for _, p := range []string{"alice", "bob"} {
		id, err := rg.pg.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: p, Issuer: "https://idp.test", Email: p + "@corp.test"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		subjects[p] = audit.SubjectActorPrefix + id.ID.String()
	}
	rg.decide("alice", "alice typed these words")
	rg.decide("bob", "bob typed other words")
	rg.decide("carol", "carol has no identity row") // never signed in: her actor is not a subject

	// The store holds subjects; the sink never saw a person's name as the actor.
	got := rg.actorsStored("approval.decide")
	if !slices.Equal(got, []string{subjects["alice"], subjects["bob"], "carol"}) {
		t.Fatalf("stored actors = %v, want alice and bob as subjects and carol as she was", got)
	}
	sink := readFileOrEmpty(rg.sinkPath)
	if strings.Contains(sink, `"actor":"alice"`) || strings.Contains(sink, `"actor":"bob"`) || !strings.Contains(sink, subjects["alice"]) {
		t.Fatalf("the sink does not carry subjects in place of names:\n%s", sink)
	}

	// Both wardynds render the person; a filter by person (name or email) finds the row.
	for name, h := range map[string]http.Handler{"A": rg.a, "B": rg.b} {
		if got := rg.reasons(h); got["alice"] != "alice typed these words" || got["bob"] != "bob typed other words" {
			t.Fatalf("wardynd %s reads %v, want the persons and their fields opened", name, got)
		}
		for _, who := range []string{"alice", "alice@corp.test"} {
			if got := rg.actorsRead(h, who); !slices.Equal(got, []string{"alice"}) {
				t.Errorf("wardynd %s ?actor=%s reads %v, want alice's one row", name, who, got)
			}
		}
		if got := rg.actorsRead(h, "carol"); !slices.Equal(got, []string{"carol"}) {
			t.Errorf("wardynd %s ?actor=carol reads %v, want her row, which has no subject", name, got)
		}
	}
	if exp := rg.call(rg.b, http.MethodGet, "/api/v1/audit/export?action=approval.decide&actor=alice", ""); exp.Code != http.StatusOK || !strings.Contains(exp.Body.String(), `"actor":"alice"`) {
		t.Errorf("export = %d %s, want alice named in the readable form", exp.Code, exp.Body)
	}

	if w := rg.call(rg.a, http.MethodPost, "/api/v1/people/alice/erasure", `{"scopes":["audit_personal_fields"]}`); w.Code != http.StatusOK {
		t.Fatalf("erasure = %d %s", w.Code, w.Body)
	}
	for name, h := range map[string]http.Handler{"A": rg.a, "B": rg.b} {
		if got := rg.actorsRead(h, ""); !slices.Equal(got, []string{audit.ErasedValue, "bob", "carol"}) {
			t.Errorf("wardynd %s after the erasure reads actors %v, want alice erased", name, got)
		}
		if got := rg.actorsRead(h, "alice"); !slices.Equal(got, []string{audit.ErasedValue}) {
			t.Errorf("wardynd %s ?actor=alice after the erasure reads %v, want the one row, erased", name, got)
		}
	}
	if st, err := rg.pg.VerifyAuditChain(ctx); err != nil || !st.OK {
		t.Fatalf("chain after the shred = %+v, %v; the hash covers the stored subject, so verify must still pass", st, err)
	}
	if got := rg.actorsStored("approval.decide"); got[0] != subjects["alice"] {
		t.Errorf("stored actor after the erasure = %q, want it unchanged", got[0])
	}
}

func TestPG_AuditSeal_FullActorWaitsInTheSpoolThenTheDrainStoresTheSubject(t *testing.T) {
	rg := newSealRigMode(t, audit.SealFull)
	ctx := t.Context()
	id, err := rg.pg.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: "erin", Issuer: "https://idp.test"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rg.flaky.down.Store(true)
	rg.decide("erin", "erin typed while down")
	if got := rg.actorsStored("approval.decide"); len(got) != 0 {
		t.Fatalf("a pending row reached the store: %v", got)
	}
	spooled := readFileOrEmpty(rg.spoolPath)
	if !strings.Contains(spooled, audit.PendingActor) || strings.Contains(spooled, `"actor":"erin"`) {
		t.Fatalf("the spool = %q, want the actor held under the pending key", spooled)
	}
	rg.flaky.down.Store(false)
	if n, err := rg.spool.Drain(ctx, rg.drain, 10); err != nil || n != 1 {
		t.Fatalf("drain = %d, %v", n, err)
	}
	if got := rg.actorsStored("approval.decide"); !slices.Equal(got, []string{audit.SubjectActorPrefix + id.ID.String()}) {
		t.Errorf("stored actors = %v, want erin's subject", got)
	}
	if got := rg.actorsRead(rg.a, ""); !slices.Equal(got, []string{"erin"}) {
		t.Errorf("read actors = %v, want erin", got)
	}
}

// targetsStored is every audit row's stored target of one action, oldest first.
func (rg *sealRig) targetsStored(action string) []string {
	rg.t.Helper()
	rows, err := rg.pool.Query(rg.t.Context(), `SELECT target FROM audit_events WHERE action = $1 ORDER BY seq`, action)
	if err != nil {
		rg.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rg.t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// targetsRead is the target of every row of action a reader sees through h, narrowed by ?target= when not "".
func (rg *sealRig) targetsRead(h http.Handler, action, target string) []string {
	rg.t.Helper()
	path := "/api/v1/audit?action=" + action
	if target != "" {
		path += "&target=" + target
	}
	w := rg.call(h, http.MethodGet, path, "")
	if w.Code != http.StatusOK {
		rg.t.Fatalf("audit read = %d %s", w.Code, w.Body)
	}
	var evs []types.AuditEvent
	if err := json.Unmarshal(w.Body.Bytes(), &evs); err != nil {
		rg.t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		out = append(out, ev.Target)
	}
	return out
}

// A person named as the target of a person-target row is stored as their subject
// through the real recorder chain, read back as themself, and reads erased once
// they are erased; a person who never signed in keeps the name, and a subject
// string typed by a caller is refused.
func TestPG_AuditSeal_FullStoresAPersonTargetAsASubjectAndErasureShredsIt(t *testing.T) {
	rg := newSealRigMode(t, audit.SealFull)
	ctx := t.Context()
	id, err := rg.pg.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: "alice", Issuer: "https://idp.test", Email: "alice@corp.test"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	subject := audit.SubjectActorPrefix + id.ID.String()
	record := func(actorType types.ActorType, actor, action, target string) error {
		return rg.rec.Record(ctx, types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: actorType, Actor: actor,
			Action: action, Target: target, Outcome: "success", Data: []byte(`{}`)})
	}
	for _, c := range []struct {
		typ           types.ActorType
		actor, action string
	}{
		{types.ActorHuman, "admin", "person.token.create"},
		{types.ActorSystem, "delegate:d1", "delegation.exchange"},
	} {
		if err := record(c.typ, c.actor, c.action, "alice"); err != nil {
			t.Fatalf("record %s: %v", c.action, err)
		}
		if err := record(c.typ, c.actor, c.action, "newcomer"); err != nil { // no identity row yet
			t.Fatalf("record %s for a person who never signed in: %v", c.action, err)
		}
		if got := rg.targetsStored(c.action); !slices.Equal(got, []string{subject, "newcomer"}) {
			t.Fatalf("%s stored targets = %v, want alice as her subject and newcomer as she was", c.action, got)
		}
		if err := record(c.typ, c.actor, c.action, subject); err == nil {
			t.Errorf("%s: a caller-typed subject target was recorded", c.action)
		}
		for name, h := range map[string]http.Handler{"A": rg.a, "B": rg.b} {
			if got := rg.targetsRead(h, c.action, "alice"); !slices.Equal(got, []string{"alice"}) {
				t.Errorf("wardynd %s ?target=alice on %s reads %v, want alice's one row", name, c.action, got)
			}
		}
	}
	if sink := readFileOrEmpty(rg.sinkPath); strings.Contains(sink, `"target":"alice"`) {
		t.Errorf("the sink carries alice's name as a target:\n%s", sink)
	}

	if w := rg.call(rg.a, http.MethodPost, "/api/v1/people/alice/erasure", `{"scopes":["audit_personal_fields"]}`); w.Code != http.StatusOK {
		t.Fatalf("erasure = %d %s", w.Code, w.Body)
	}
	for _, action := range []string{"person.token.create", "delegation.exchange"} {
		for name, h := range map[string]http.Handler{"A": rg.a, "B": rg.b} {
			if got := rg.targetsRead(h, action, ""); !slices.Equal(got, []string{audit.ErasedValue, "newcomer"}) && !slices.Equal(got, []string{"newcomer", audit.ErasedValue}) {
				t.Errorf("wardynd %s reads %s targets %v after the erasure, want alice erased and newcomer kept", name, action, got)
			}
		}
		if got := rg.targetsStored(action); got[0] != subject {
			t.Errorf("%s stored target after the erasure = %q, want it unchanged", action, got[0])
		}
	}
	if st, err := rg.pg.VerifyAuditChain(ctx); err != nil || !st.OK {
		t.Fatalf("chain after the shred = %+v, %v", st, err)
	}
}
