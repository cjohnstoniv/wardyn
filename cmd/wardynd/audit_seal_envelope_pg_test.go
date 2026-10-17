// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// What a WARDYN_AUDIT_SEAL=full row stores beside its sealed fields, read as
// bytes from Postgres, the spool and a sink (the rig is audit_seal_pg_test.go's):
// nothing that spells the person, in any encoding, before or after their
// erasure; one key handle for every name the person goes by; and erasure clears
// the handle from the key table too. Guarded by WARDYN_TEST_PG.

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/testutil"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_AuditSeal_FullRowsSpoolAndSinkNameNoPersonAndOneHandle(t *testing.T) {
	rg := newSealRigMode(t, audit.SealFull)
	ctx := t.Context()
	const (
		principal = "pat-principal-5c2e"
		email     = "pat.quinn@corp.test"
		tenant    = "6f1d2c3b-tenant"
		object    = "8a7e-object-31d0"
	)
	entra := "entra:" + tenant + ":" + object
	if _, err := rg.pg.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: principal, Issuer: "https://login.microsoftonline.com/" + tenant + "/v2.0",
		TenantID: tenant, ObjectID: object, Email: email}, time.Now()); err != nil {
		t.Fatal(err)
	}
	people := []string{principal, email, entra, object}
	const action = "governance.change.reject"
	reject := func(actor string) {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"proposed_by": "proposer", "reason": "words " + actor + " typed", "target_key": "p"})
		if err := rg.rec.Record(ctx, types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorHuman,
			Actor: actor, Action: action, Target: uuid.NewString(), Outcome: "success", Data: data}); err != nil {
			t.Fatalf("record %s's rejection: %v", actor, err)
		}
	}
	namesNoOne := func(where, content string) {
		t.Helper()
		for _, p := range people {
			for _, f := range testutil.Spellings(p) {
				if strings.Contains(content, f) {
					t.Errorf("%s spells %q (as %q):\n%s", where, p, f, content)
				}
			}
		}
	}
	stored := func() string {
		t.Helper()
		var all string
		if err := rg.pool.QueryRow(ctx, `SELECT COALESCE(string_agg(actor || ' ' || target || ' ' || source_ip || ' ' || data::text, E'\n' ORDER BY seq), '')
			FROM audit_events WHERE action = $1`, action).Scan(&all); err != nil {
			t.Fatal(err)
		}
		return all
	}

	for _, name := range []string{principal, email, entra} {
		reject(name)
	}
	rg.flaky.down.Store(true) // the next row waits in the spool under the pending key
	reject(email)
	namesNoOne("the spool", readFileOrEmpty(rg.spoolPath))
	rg.flaky.down.Store(false)
	if n, err := rg.spool.Drain(ctx, rg.drain, 10); err != nil || n != 1 {
		t.Fatalf("drain = %d, %v", n, err)
	}

	rows := stored()
	namesNoOne("the stored rows", rows)
	var sunk []string
	for _, line := range strings.Split(readFileOrEmpty(rg.sinkPath), "\n") {
		if strings.Contains(line, `"`+action+`"`) {
			sunk = append(sunk, line)
		}
	}
	if len(sunk) == 0 {
		t.Fatal("the sink received no row to inspect")
	}
	namesNoOne("the sink", strings.Join(sunk, "\n"))

	// Every name the person goes by seals under one key, named by one handle.
	handles := map[string]bool{}
	for _, m := range regexp.MustCompile(`seal2\.([0-9a-f-]{36})\.`).FindAllStringSubmatch(rows, -1) {
		handles[m[1]] = true
	}
	if len(handles) != 1 || strings.Count(rows, "seal2.") != 4 {
		t.Fatalf("handles %v over %d sealed fields, want one handle for all four rows:\n%s", handles, strings.Count(rows, "seal2."), rows)
	}
	var handle string
	for h := range handles {
		handle = h
	}

	if w := rg.call(rg.a, http.MethodPost, "/api/v1/people/"+principal+"/erasure", `{"scopes":["audit_personal_fields"]}`); w.Code != http.StatusOK {
		t.Fatalf("erasure = %d %s", w.Code, w.Body)
	}
	w := rg.call(rg.b, http.MethodGet, "/api/v1/audit?action="+action, "")
	var evs []types.AuditEvent
	if err := json.Unmarshal(w.Body.Bytes(), &evs); w.Code != http.StatusOK || err != nil || len(evs) != 4 {
		t.Fatalf("audit read = %d %v %s", w.Code, err, w.Body)
	}
	for _, ev := range evs {
		var d map[string]any
		_ = json.Unmarshal(ev.Data, &d)
		if ev.Actor != audit.ErasedValue || d["reason"] != audit.ErasedValue {
			t.Errorf("after the erasure a row reads actor %q reason %v, want both %q", ev.Actor, d["reason"], audit.ErasedValue)
		}
	}
	var holders int
	if err := rg.pool.QueryRow(ctx, `SELECT count(*) FROM principal_keys WHERE handle = $1`, handle).Scan(&holders); err != nil || holders != 0 {
		t.Errorf("%d key rows still carry the erased handle (%v), want none: the key table would map it back to the person", holders, err)
	}
	if after := stored(); after != rows {
		t.Errorf("the erasure rewrote stored rows:\nbefore %s\nafter %s", rows, after)
	}
	if st, err := rg.pg.VerifyAuditChain(ctx); err != nil || !st.OK {
		t.Fatalf("chain = %+v, %v, want ok", st, err)
	}
}
