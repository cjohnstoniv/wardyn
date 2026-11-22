// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// No door before the exchange reaches out: a direct registration, a held proposal, the reads an approver
// previews it with (the change and its impact) and the approval make no call through the default transport
// and no connection to either registered issuer or the internal address each names.
func TestPortalRegistration_NoOutboundCallBeforeExchange_PG(t *testing.T) {
	e := newRegPG(t)
	calls := countDefaultTransport(t) // after the harness's own sign-in fixture
	direct, held, internal := newDiscoveryEndpoint(t), newDiscoveryEndpoint(t), newDiscoveryEndpoint(t)
	t.Setenv(envPortalInternalIssuerHosts, internal.authority)
	quiet := func(door string) {
		t.Helper()
		if n := calls.Load(); n != 0 {
			t.Errorf("after the %s: %d call(s) through the default transport, want 0", door, n)
		}
		if n := direct.conns.Load() + held.conns.Load() + internal.conns.Load(); n != 0 {
			t.Errorf("after the %s: %d connection(s) to a registered issuer or internal address, want 0", door, n)
		}
	}

	e.registered(t, map[string]any{"issuer": direct.url, "internal_issuer": internal.url})
	quiet("direct registration")

	t.Setenv(envGovernanceSecondHuman, "true")
	change, _, w := e.proposeHeld(t, map[string]any{"issuer": held.url, "internal_issuer": internal.url})
	if change == "" {
		t.Fatalf("proposal = %d %s, want 202", w.Code, w.Body.String())
	}
	quiet("proposal")

	for _, path := range []string{"/api/v1/governance/changes/" + change, "/api/v1/governance/changes/" + change + "/impact"} {
		if w := doSSO(t, e.h.srv, http.MethodGet, path, e.admin2, ""); w.Code != http.StatusOK {
			t.Errorf("GET %s = %d %s, want 200", path, w.Code, w.Body.String())
		}
	}
	quiet("preview")

	if w := e.decideHeld(t, change, "approve"); w.Code != http.StatusOK {
		t.Fatalf("approval = %d %s, want 200", w.Code, w.Body.String())
	}
	quiet("approval")
}

// A portal that chooses its own people (portal-managed admission, no required claim) carries the plain note in
// the held change's diff and in the delegate.create row, direct or approved; a portal restricted by a required
// claim carries neither.
func TestPortalRegistration_PortalManagedNoteInDiffAndAudit_PG(t *testing.T) {
	e := newRegPG(t)
	restricted := map[string]any{"required_claim": map[string]string{"claim": "tenant", "value": "prod"}}
	auditNote := func(t *testing.T, target string) (string, bool) {
		t.Helper()
		for _, ev := range e.rows("delegate.create") {
			if ev.Target != target {
				continue
			}
			var data map[string]any
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				t.Fatal(err)
			}
			note, ok := data["note"].(string)
			return note, ok
		}
		t.Fatalf("no delegate.create row for %s", target)
		return "", false
	}
	diffNotes := func(t *testing.T, change string) []string {
		t.Helper()
		w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/governance/changes/"+change, e.admin2, "")
		var got struct {
			Diff governanceDiffBody `json:"diff"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
			t.Fatalf("GET change = %d %s", w.Code, w.Body.String())
		}
		return got.Diff.Notes
	}

	t.Run("direct", func(t *testing.T) {
		d := e.registered(t, map[string]any{"issuer": "https://note-direct.example.com"})
		if note, _ := auditNote(t, d.ID.String()); note != portalManagedNote {
			t.Errorf("audit note = %q, want %q", note, portalManagedNote)
		}
		r := e.registered(t, map[string]any{"issuer": "https://note-direct-claim.example.com", "required_claim": restricted["required_claim"]})
		if note, ok := auditNote(t, r.ID.String()); ok {
			t.Errorf("a restricted portal's audit row carries a note: %q", note)
		}
	})
	t.Run("held", func(t *testing.T) {
		t.Setenv(envGovernanceSecondHuman, "true")
		change, _, w := e.proposeHeld(t, map[string]any{"issuer": "https://note-held.example.com"})
		if change == "" {
			t.Fatalf("proposal = %d %s", w.Code, w.Body.String())
		}
		if notes := diffNotes(t, change); !slices.Contains(notes, portalManagedNote) {
			t.Errorf("diff notes = %q, want the portal-managed note", notes)
		}
		if w := e.decideHeld(t, change, "approve"); w.Code != http.StatusOK {
			t.Fatalf("approval = %d %s", w.Code, w.Body.String())
		}
		var id string
		if err := e.pool.QueryRow(context.Background(), `SELECT target_key FROM governance_changes WHERE id = $1`, change).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if note, _ := auditNote(t, id); note != portalManagedNote {
			t.Errorf("approved audit note = %q, want %q", note, portalManagedNote)
		}
		rc, _, w := e.proposeHeld(t, map[string]any{"issuer": "https://note-held-claim.example.com", "required_claim": restricted["required_claim"]})
		if rc == "" {
			t.Fatalf("restricted proposal = %d %s", w.Code, w.Body.String())
		}
		if notes := diffNotes(t, rc); slices.Contains(notes, portalManagedNote) {
			t.Errorf("a restricted portal's diff carries the note: %q", notes)
		}
	})
}

// A console portal keeps the shape 0.10.0 served, though the database gives every row an admission revision:
// neither the registration's answer nor the list of persisted rows names admission_rev for it, while an issuer
// portal's row keeps its revision.
func TestPortalRegistration_ConsolePortalShapeFromPersistedRows_PG(t *testing.T) {
	e := newRegPG(t)
	ctx := context.Background()
	w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/admin/delegates", e.admin, `{"name":"legacy","idp_client_id":"legacy-client","group":"portal-users"}`)
	var posted map[string]json.RawMessage
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &posted) != nil {
		t.Fatalf("legacy registration = %d %s", w.Code, w.Body.String())
	}
	var postedID string
	_ = json.Unmarshal(posted["id"], &postedID)
	if _, ok := posted["admission_rev"]; ok {
		t.Errorf("legacy registration answer names admission_rev: %s", w.Body.String())
	}
	// A console portal written by the store as 0.10.0 wrote one, and the database default it then carries.
	old, err := e.st.CreateDelegate(ctx, types.Delegate{ID: uuid.New(), Name: "old", IdPClientID: "old-client", Group: "old-users", RegisteredBy: "root"}, newBearer(delegateCredentialPrefix))
	if err != nil {
		t.Fatal(err)
	}
	var rev int64
	if err := e.pool.QueryRow(ctx, `SELECT admission_rev FROM delegates WHERE id = $1`, old.ID).Scan(&rev); err != nil || rev == 0 {
		t.Fatalf("stored admission_rev = %d, %v; the column default should give the row one", rev, err)
	}
	issuer := e.registered(t, map[string]any{"issuer": "https://shape-issuer.example.com"})

	list := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/admin/delegates", e.admin, "")
	var rows []map[string]json.RawMessage
	if list.Code != http.StatusOK || json.Unmarshal(list.Body.Bytes(), &rows) != nil {
		t.Fatalf("list = %d %s", list.Code, list.Body.String())
	}
	seen := 0
	for _, row := range rows {
		var id string
		_ = json.Unmarshal(row["id"], &id)
		_, hasRev := row["admission_rev"]
		switch id {
		case old.ID.String(), postedID:
			seen++
			if hasRev {
				t.Errorf("console portal %s lists admission_rev", id)
			}
		case issuer.ID.String():
			seen++
			if string(row["admission_rev"]) != "1" {
				t.Errorf("issuer portal admission_rev = %s, want 1", row["admission_rev"])
			}
		}
	}
	if seen != 3 {
		t.Errorf("found %d of the 3 portals in the list: %s", seen, list.Body.String())
	}
}

// The admin token registers a portal directly under the second-human switch: 201 with the portal row and its
// delegate.create row, nothing held, and one governance.change.bypass row naming the delegate kind.
func TestPortalRegistration_AdminTokenBypassesSecondHuman_PG(t *testing.T) {
	e := newRegPG(t)
	t.Setenv(envGovernanceSecondHuman, "true")
	issuer := "https://token-bypass.example.com"
	w := do(t, e.h.srv, http.MethodPost, "/api/v1/admin/delegates", adminToken, e.body(map[string]any{"issuer": issuer}))
	if w.Code != http.StatusCreated {
		t.Fatalf("admin-token registration = %d %s, want 201", w.Code, w.Body.String())
	}
	var d types.Delegate
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil || d.ID == uuid.Nil {
		t.Fatalf("registration body = %s, %v", w.Body.String(), err)
	}
	if n := e.portals(t, issuer); n != 1 {
		t.Errorf("portal rows = %d, want 1", n)
	}
	if n := e.countPending(t); n != 0 {
		t.Errorf("pending changes = %d, want 0: the admin token's registration is not held", n)
	}
	created := 0
	for _, ev := range e.rows("delegate.create") {
		if ev.Target == d.ID.String() && ev.Outcome == "success" {
			created++
		}
	}
	if created != 1 {
		t.Errorf("delegate.create rows for the portal = %d, want 1", created)
	}
	var bypass []string
	for _, ev := range e.rows("governance.change.bypass") {
		var data struct {
			Kind string `json:"target_kind"`
		}
		_ = json.Unmarshal(ev.Data, &data)
		bypass = append(bypass, ev.Target+"/"+ev.Outcome+"/"+data.Kind)
	}
	if want := d.ID.String() + "/success/" + govKindDelegate; len(bypass) != 1 || bypass[0] != want {
		t.Errorf("bypass rows = %v, want [%s]", bypass, want)
	}
}
