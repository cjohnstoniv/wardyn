// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_GovernanceProfileContact walks the contact through POST and PUT against
// Postgres, where "kept", "cleared" and "stored as NULL" are the upsert's own
// answers: absent keeps, null clears, {} stores NULL, POST without it is NULL, a
// bad value is a 400 that writes nothing, and the audit row names the fields and
// the request link but never the owner or email.
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.
func TestPG_GovernanceProfileContact(t *testing.T) {
	pool := throwawayPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	h := newHarness(t)
	srv := New(baseTestConfig(h, pg))

	write := func(method, path, body string) (types.GovernanceProfile, int, string) {
		t.Helper()
		w := do(t, srv, method, path, adminToken, body)
		var resp governanceProfileResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		return resp.Profile, w.Code, w.Body.String()
	}
	const base = `"name":"team-a","ceiling":{"min_confinement_class":"CC2"}`
	want := policyref.Contact{Owner: "Platform security", Email: "sec@example.com", RequestURL: "https://help.example.com/access"}

	// POST without contact: NULL.
	p, code, out := write(http.MethodPost, "/api/v1/governance/profiles", `{`+base+`}`)
	if code != http.StatusCreated || p.Contact != nil {
		t.Fatalf("POST without contact = %d contact=%+v %s, want 201 and none", code, p.Contact, out)
	}
	path := "/api/v1/governance/profiles/" + p.ID.String()
	stored := func() *policyref.Contact {
		t.Helper()
		got, err := pg.GetGovernanceProfile(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.Contact
	}

	// A bad value is refused and writes nothing.
	for _, bad := range []string{
		`{"request_url":"javascript:alert(1)"}`, `{"email":"a&b@example.com"}`, `{"owner":"a\u202eb"}`, `5`,
	} {
		if _, code, out := write(http.MethodPut, path, `{`+base+`,"contact":`+bad+`}`); code != http.StatusBadRequest ||
			!strings.Contains(out, reasonGovernanceProfileRequestInvalid) {
			t.Errorf("contact %s = %d %s, want 400 %s", bad, code, out, reasonGovernanceProfileRequestInvalid)
		}
	}
	if c := stored(); c != nil {
		t.Fatalf("a refused write stored %+v", c)
	}

	// Set it.
	if _, code, out := write(http.MethodPut, path, `{`+base+`,"contact":{"owner":"Platform security","email":"sec@example.com","request_url":"https://help.example.com/access"}}`); code != http.StatusOK {
		t.Fatalf("PUT with contact = %d %s", code, out)
	}
	if c := stored(); c == nil || *c != want {
		t.Fatalf("stored = %+v, want %+v", c, want)
	}

	// Absent keeps; null clears; {} stores NULL.
	if p2, code, out := write(http.MethodPut, path, `{`+base+`}`); code != http.StatusOK || p2.Contact == nil || *p2.Contact != want {
		t.Fatalf("PUT without contact = %d contact=%+v %s, want the stored value kept", code, p2.Contact, out)
	}
	if c := stored(); c == nil || *c != want {
		t.Fatalf("after an absent PUT stored = %+v, want %+v", c, want)
	}
	if _, code, out := write(http.MethodPut, path, `{`+base+`,"contact":null}`); code != http.StatusOK {
		t.Fatalf("PUT null = %d %s", code, out)
	}
	if c := stored(); c != nil {
		t.Fatalf("after null stored = %+v, want none", c)
	}
	write(http.MethodPut, path, `{`+base+`,"contact":{"owner":"x"}}`)
	if _, code, out := write(http.MethodPut, path, `{`+base+`,"contact":{}}`); code != http.StatusOK {
		t.Fatalf("PUT {} = %d %s", code, out)
	}
	var isNull bool
	if err := pool.QueryRow(ctx, `SELECT contact IS NULL FROM governance_profiles WHERE id = $1`, p.ID).Scan(&isNull); err != nil || !isNull {
		t.Fatalf("after {} contact IS NULL = %v (%v), want a SQL NULL, not an empty object", isNull, err)
	}

	// Audit rows: fields and request link only.
	write(http.MethodPut, path, `{`+base+`,"contact":{"owner":"Platform security","email":"sec@example.com","request_url":"https://help.example.com/access"}}`)
	var last types.AuditEvent
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "governance.profile.write" {
			last = ev
		}
	}
	var d map[string]any
	if err := json.Unmarshal(last.Data, &d); err != nil {
		t.Fatalf("decode audit data: %v", err)
	}
	if f, _ := json.Marshal(d["contact_fields"]); string(f) != `["owner","email","request_url"]` || d["contact_request_url"] != want.RequestURL {
		t.Errorf("audit data = %v, want the field list and request link", d)
	}
	for _, ev := range h.audit.snapshot() {
		if strings.Contains(string(ev.Data), "Platform security") || strings.Contains(string(ev.Data), "sec@example.com") {
			t.Errorf("audit row %s carries the owner or email: %s", ev.Action, ev.Data)
		}
	}
}

// A contact written straight into the database can hold anything the JSONB CHECK
// allows. Reading it must not fail the profile, and Project must keep only the
// fields that still pass.
func TestPG_GovernanceProfileContactDirectWrite(t *testing.T) {
	pool := throwawayPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	p, err := pg.UpsertGovernanceProfile(ctx, types.GovernanceProfile{
		Name: "direct", Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2}, CreatedBy: "seed",
	})
	if err != nil {
		t.Fatal(err)
	}

	// The column's own backstop: only an object under the size cap.
	if _, err := pool.Exec(ctx, `UPDATE governance_profiles SET contact = '"text"' WHERE id = $1`, p.ID); err == nil {
		t.Error("a non-object contact was accepted by the CHECK")
	}
	if _, err := pool.Exec(ctx, `UPDATE governance_profiles SET contact = jsonb_build_object('owner', repeat('x', 9000)) WHERE id = $1`, p.ID); err == nil {
		t.Error("a contact over 8 KiB was accepted by the CHECK")
	}

	if _, err := pool.Exec(ctx, `UPDATE governance_profiles SET contact = $2::jsonb WHERE id = $1`, p.ID,
		`{"owner":"Platform","email":"x?cc=evil@example.com","request_url":"javascript:alert(1)","request_text":"ask\nnow"}`); err != nil {
		t.Fatal(err)
	}
	got, err := pg.GetGovernanceProfile(ctx, p.ID)
	if err != nil {
		t.Fatalf("a bad stored contact failed the profile read: %v", err)
	}
	ref := policyref.Project(policyref.SourceProfile, got.Name, got.Contact)
	if want := (policyref.Ref{Source: "profile", Name: "direct", Owner: "Platform"}); ref == nil || *ref != want {
		t.Fatalf("Project = %+v, want %+v", ref, want)
	}
	if got.Contact != nil && got.Contact.Email != "" {
		if r := (policyref.Ref{Email: got.Contact.Email}).RequestRoute(); r != "" {
			t.Errorf("RequestRoute built %q from a directly written bad email", r)
		}
	}

	// A member the column cannot decode reads as no contact, not as an error.
	if _, err := pool.Exec(ctx, `UPDATE governance_profiles SET contact = '{"owner":5}'::jsonb WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := pg.GetGovernanceProfile(ctx, p.ID); err != nil || got.Contact != nil {
		t.Fatalf("undecodable contact: err=%v contact=%+v, want a clean read with none", err, got.Contact)
	}
}
