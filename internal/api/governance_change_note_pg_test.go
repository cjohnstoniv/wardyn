// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The note on a held governance change: PUT /governance/changes/{id}/note, who may write it, and what
// the change read, the propose row and the changes list say about it. Guarded by WARDYN_TEST_PG
// (throwawayPGPool): skipped cleanly when unset, must PASS when set.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func notePath(id uuid.UUID) string { return "/api/v1/governance/changes/" + id.String() + "/note" }

// noteHeld is a membership add held under unit u, with the session of the admin of u who proposed it.
func (e *unitAdminEnv) noteHeld(u types.GovernanceUnit) (types.GovernanceChange, *http.Cookie) {
	e.t.Helper()
	proposer := e.fresh("sub-note-proposer")
	e.grant(u, types.CapabilitySubjectUser, proposer)
	person := e.fresh("sub-note-person")
	treeMember(e.t, e.govEnv, e.child(u), types.CapabilitySubjectUser, person, 1, false)
	cookie := uaSession(e.t, proposer, []string{}, false)
	return e.pending(e.call(cookie, http.MethodPost, "/api/v1/governance/memberships", membershipBody(person, u.ID))), cookie
}

// readChange is GET /governance/changes/{id} as a security admin, decoded to a generic map.
func (e *unitAdminEnv) readChange(id uuid.UUID) map[string]any {
	e.t.Helper()
	w := e.call(e.alice, http.MethodGet, "/api/v1/governance/changes/"+id.String(), "")
	if w.Code != http.StatusOK {
		e.t.Fatalf("read change %s = %d %s", id, w.Code, w.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		e.t.Fatalf("decode change: %v", err)
	}
	return out
}

func noteReasonOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	return wireReason(t, w)
}

// TestChangeNoteProposerOnly: the proposer sets the note; another admin who can decide the change, a
// security admin, an admin of another unit and a member are refused, and none of them leaves a note.
func TestChangeNoteProposerOnly(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	ch, proposer := e.noteHeld(e.a2)
	body := `{"reason":"needed for the payments launch","ticket":"INC-42"}`

	if w := e.call(e.peerA, http.MethodPut, notePath(ch.ID), body); w.Code != http.StatusForbidden || noteReasonOf(t, w) != "governance_change_note_proposer_only" {
		t.Errorf("a peer who may decide it: %d %s, want 403 governance_change_note_proposer_only", w.Code, w.Body)
	}
	if w := e.call(e.alice, http.MethodPut, notePath(ch.ID), body); w.Code != http.StatusForbidden || noteReasonOf(t, w) != "governance_change_note_proposer_only" {
		t.Errorf("a security admin: %d %s, want 403 governance_change_note_proposer_only", w.Code, w.Body)
	}
	if w := e.call(e.adminB, http.MethodPut, notePath(ch.ID), body); w.Code != http.StatusNotFound || noteReasonOf(t, w) != reasonGovernanceChangeNotFound {
		t.Errorf("an admin of another unit: %d %s, want the hidden 404 %s", w.Code, w.Body, reasonGovernanceChangeNotFound)
	}
	if w := e.call(e.member, http.MethodPut, notePath(ch.ID), body); !scopeRefused(t, w) {
		t.Errorf("a member: %d %s, want 403 unit_scope_denied", w.Code, w.Body)
	}
	if w := e.call(nil, http.MethodPut, notePath(ch.ID), body); w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated: %d %s, want 401", w.Code, w.Body)
	}
	if _, has := e.readChange(ch.ID)["note"]; has {
		t.Fatalf("a refused write left a note on the change")
	}

	w := e.call(proposer, http.MethodPut, notePath(ch.ID), body)
	if w.Code != http.StatusOK {
		t.Fatalf("the proposer: %d %s, want 200", w.Code, w.Body)
	}
	note, _ := e.readChange(ch.ID)["note"].(map[string]any)
	if note["reason"] != "needed for the payments launch" || note["ticket"] != "INC-42" {
		t.Errorf("note on the read = %v, want the reason and ticket the proposer wrote", note)
	}
}

// TestChangeNotePendingOnly: a change that is approved or rejected takes no note.
func TestChangeNotePendingOnly(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	approved, proposerA := e.noteHeld(e.a2)
	rejected, proposerR := e.noteHeld(e.a2)
	if w := e.call(e.alice, http.MethodPost, approvePath(approved.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", w.Code, w.Body)
	}
	if w := e.call(e.alice, http.MethodPost, rejectPath(rejected.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("reject = %d %s", w.Code, w.Body)
	}
	for name, c := range map[string]struct {
		id     uuid.UUID
		cookie *http.Cookie
	}{"approved": {approved.ID, proposerA}, "rejected": {rejected.ID, proposerR}} {
		w := e.call(c.cookie, http.MethodPut, notePath(c.id), `{"reason":"too late"}`)
		if w.Code != http.StatusConflict || noteReasonOf(t, w) != reasonGovernanceChangeNotPending {
			t.Errorf("%s change: %d %s, want 409 %s", name, w.Code, w.Body, reasonGovernanceChangeNotPending)
		}
		if _, has := e.readChange(c.id)["note"]; has {
			t.Errorf("%s change took a note", name)
		}
	}
}

// TestChangeNoteBounds: the reason is at most 500 characters and the ticket one line of at most 128,
// both without control characters; a body with another key is refused. A refusal changes nothing.
func TestChangeNoteBounds(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	ch, proposer := e.noteHeld(e.a2)
	for name, body := range map[string]string{
		"reason too long":   `{"reason":"` + strings.Repeat("r", 501) + `"}`,
		"reason control":    `{"reason":"a\u0007b"}`,
		"reason newline":    `{"reason":"a\nb"}`,
		"ticket too long":   `{"ticket":"` + strings.Repeat("t", 129) + `"}`,
		"ticket two lines":  `{"ticket":"INC-1\nINC-2"}`,
		"ticket control":    `{"ticket":"INC\u0000"}`,
		"unknown field":     `{"reason":"x","approve":true}`,
		"not an object":     `"reason"`,
		"reason not string": `{"reason":7}`,
	} {
		w := e.call(proposer, http.MethodPut, notePath(ch.ID), body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, w.Code, w.Body)
		}
	}
	// One value, with no field written twice: the decode would keep the last.
	for name, body := range map[string]string{
		"a second value":     `{"reason":"first"} {"reason":"second"}`,
		"a key twice":        `{"reason":"first","reason":"second"}`,
		"a key in two cases": `{"reason":"first","REASON":"second"}`,
	} {
		if w := e.call(proposer, http.MethodPut, notePath(ch.ID), body); w.Code != http.StatusBadRequest || noteReasonOf(t, w) != reasonInvalidRequestBody {
			t.Errorf("%s: %d %s, want 400 %s", name, w.Code, w.Body, reasonInvalidRequestBody)
		}
	}
	if _, has := e.readChange(ch.ID)["note"]; has {
		t.Fatalf("a refused note left one on the change")
	}

	// At the bounds, and trimmed.
	atBounds := `{"reason":"  ` + strings.Repeat("r", 500) + `  ","ticket":"` + strings.Repeat("t", 128) + `"}`
	if w := e.call(proposer, http.MethodPut, notePath(ch.ID), atBounds); w.Code != http.StatusOK {
		t.Fatalf("a note at the bounds: %d %s, want 200", w.Code, w.Body)
	}
	note, _ := e.readChange(ch.ID)["note"].(map[string]any)
	if r, _ := note["reason"].(string); r != strings.Repeat("r", 500) {
		t.Errorf("stored reason has %d characters, want the 500 left after trimming", len(r))
	}
	// A second write replaces the first.
	if w := e.call(proposer, http.MethodPut, notePath(ch.ID), `{"reason":"second"}`); w.Code != http.StatusOK {
		t.Fatalf("a second note: %d %s", w.Code, w.Body)
	}
	note, _ = e.readChange(ch.ID)["note"].(map[string]any)
	if note["reason"] != "second" || note["ticket"] != nil && note["ticket"] != "" {
		t.Errorf("note after the second write = %v, want reason second and no ticket", note)
	}
}

// TestChangeNoteAudited: a note writes governance.change.note, naming the change, its target kind and
// key, the units it touches, the reason and the ticket; a refused write writes none.
func TestChangeNoteAudited(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	ch, proposer := e.noteHeld(e.a2)
	_ = e.call(e.alice, http.MethodPut, notePath(ch.ID), `{"reason":"not mine"}`)
	if n := len(e.audits("governance.change.note")); n != 0 {
		t.Fatalf("%d governance.change.note rows after a refused write, want 0", n)
	}
	if w := e.call(proposer, http.MethodPut, notePath(ch.ID), `{"reason":"launch","ticket":"INC-7"}`); w.Code != http.StatusOK {
		t.Fatalf("note = %d %s", w.Code, w.Body)
	}
	rows := e.audits("governance.change.note")
	if len(rows) != 1 || rows[0].Target != ch.ID.String() {
		t.Fatalf("governance.change.note rows = %+v, want one naming %s", rows, ch.ID)
	}
	d := auditData(t, rows[0])
	if d["target_kind"] != ch.TargetKind || d["target_key"] != ch.TargetKey || d["reason"] != "launch" || d["ticket"] != "INC-7" {
		t.Errorf("note row data = %v, want target_kind, target_key, reason launch and ticket INC-7", d)
	}
	if units, _ := d["units"].([]any); len(units) != 1 || units[0] != e.a2.ID.String() {
		t.Errorf("note row units = %v, want [%s]", d["units"], e.a2.ID)
	}
}

// TestProposeRowCarriesUnits: the propose row of a unit-scoped change names the units it touches and
// the field paths that differ, so a relay routes without the diff.
func TestProposeRowCarriesUnits(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	ch, _ := e.noteHeld(e.a2)
	var row types.AuditEvent
	for _, ev := range e.audits("governance.change.propose") {
		if ev.Target == ch.ID.String() {
			row = ev
		}
	}
	if row.Target == "" {
		t.Fatalf("no governance.change.propose row for %s", ch.ID)
	}
	d := auditData(t, row)
	if units, _ := d["units"].([]any); len(units) != 1 || units[0] != e.a2.ID.String() {
		t.Errorf("propose row units = %v, want [%s]", d["units"], e.a2.ID)
	}
	if _, ok := d["changed"]; !ok {
		t.Errorf("propose row data = %v, want a changed list", d)
	}
}

// TestChangesApproverSecurityFilter: ?approver=security lists the changes only a security admin may
// decide, and leaves out one a unit admin may also decide.
func TestChangesApproverSecurityFilter(t *testing.T) {
	e := newUnitAdminEnv(t, true)
	unitHeld, _ := e.noteHeld(e.a2)
	p := e.seedProfile("note-standalone", "pypi.org")
	securityOnly := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/units/"+p.ID.String(), profileBody("note-standalone", "pypi.org", "github.com")))

	ids := func(path string) map[uuid.UUID]bool {
		w := e.call(e.bob, http.MethodGet, path, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, w.Code, w.Body)
		}
		var list []types.GovernanceChange
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		out := map[uuid.UUID]bool{}
		for _, c := range list {
			out[c.ID] = true
		}
		return out
	}
	all := ids("/api/v1/governance/changes")
	if !all[unitHeld.ID] || !all[securityOnly.ID] {
		t.Fatalf("unfiltered list = %v, want both held changes", all)
	}
	only := ids("/api/v1/governance/changes?approver=security")
	if !only[securityOnly.ID] || only[unitHeld.ID] {
		t.Errorf("?approver=security = %v, want the security-only change and not the unit-scoped one", only)
	}
	if w := e.call(e.bob, http.MethodGet, "/api/v1/governance/changes?approver=nobody", ""); w.Code != http.StatusBadRequest {
		t.Errorf("?approver=nobody = %d %s, want 400", w.Code, w.Body)
	}
}

// TestChangeReadWithoutNoteGolden: a change nobody wrote a note on reads with no note key at all, and
// the same keys it had before notes existed.
func TestChangeReadWithoutNoteGolden(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	ch, _ := e.noteHeld(e.a2)
	got := e.readChange(ch.ID)
	if _, has := got["note"]; has {
		t.Errorf("a change with no note reads with a note key: %v", got["note"])
	}
	for _, k := range []string{"id", "target_kind", "op", "target_key", "state", "proposed_by", "proposed_at", "expires_at", "diff", "approvers"} {
		if _, ok := got[k]; !ok {
			t.Errorf("change read lacks %q", k)
		}
	}
}
