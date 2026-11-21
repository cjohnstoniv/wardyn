// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// personListRow is one row of the table of admin-written person lists: the plan's row number, what the list
// is, and whether it may name a portal person (allow) or must refuse one (refuse).
type personListRow struct {
	n      int
	list   string
	refuse bool
}

// personListRows are rows 1-14 and 16. Rows 12-14 (an invitation target, a person create, a credential
// issued to a principal) are refused at their own doors by the reserved prefix.
var personListRows = []personListRow{
	{1, "capability grant, user subject", false},
	{2, "governance membership, user subject", false},
	{3, "restriction subject and members", false},
	{4, "drive grant subject", false},
	{5, "UI-app callers, user arm", false},
	{6, "runner pool use-policy subjects", false},
	{7, "remote runner people subjects", true},
	{8, "unit admin grant", true},
	{9, "template group admin person", true},
	{10, "role mapping value", true},
	{11, "service identity sponsor", true},
	{16, "key-domain assignment, user subject", true},
}

// A person list's ruling is the store write's, and its answer is the API's: whichever write refused a portal
// person, a direct write, a held change's apply and the held decision all answer 422 with the list's reason,
// and a held change stays pending. The prefix-only half of the ruling folds case first, as the user-grant
// path does.
func TestAdminPersonLists_DelegatePersonRuling(t *testing.T) {
	key := types.PortalIssuerKey("https://idp.example.com")
	person := types.PortalPrincipal(key, "00u1abc")
	for _, subject := range []string{person, "DELEGATE:PERSON:" + key + ":ABC", "  " + person} {
		if err := types.RefuseDelegatePersonSubject("user", subject); err == nil {
			t.Errorf("the prefix-only ruling let %q through a refuse row", subject)
		}
	}
	for _, c := range []struct{ typ, subject string }{{"group", person}, {"user", "alice@corp.example"}, {"user", "delegate:abc"}, {"user", "sub-123"}} {
		if err := types.RefuseDelegatePersonSubject(c.typ, c.subject); err != nil {
			t.Errorf("RefuseDelegatePersonSubject(%q, %q) = %v, want nil", c.typ, c.subject, err)
		}
	}

	h := newHarness(t)
	for _, row := range personListRows {
		t.Run(fmt.Sprintf("row %d %s", row.n, row.list), func(t *testing.T) {
			var storeErr error
			wantReason := reasonDelegatePersonUnknownPortal
			if row.refuse {
				storeErr, wantReason = store.ErrDelegatePersonNotAllowed, reasonDelegatePersonNotAllowed
			} else {
				storeErr = store.ErrDelegatePersonUnknownPortal
			}
			// The write's own error, wrapped as a caller wraps it, reaches the answer as 422.
			r := httptest.NewRequest(http.MethodPost, "/api/v1/x", nil)
			w := httptest.NewRecorder()
			writeServerError(w, r, "write person list", fmt.Errorf("write row %d: %w", row.n, storeErr))
			if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != wantReason {
				t.Errorf("write = %d %s, want 422 %s", w.Code, w.Body.String(), wantReason)
			}
			// The decision of a held change that carries it: 422, one failure row, and no application.
			head := types.GovernanceChange{TargetKind: "capability_grant", TargetKey: fmt.Sprintf("row-%d", row.n)}
			w = httptest.NewRecorder()
			h.srv.answerFailedDecision(w, r, head, true, false, types.ActorHuman, "approver", fmt.Errorf("apply: %w", storeErr))
			if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != wantReason {
				t.Errorf("held decision = %d %s, want 422 %s", w.Code, w.Body.String(), wantReason)
			}
		})
	}
	if got := h.auditOutcomes("governance.change.approve"); len(got) != len(personListRows) {
		t.Errorf("approve rows = %v, want one failure per row", got)
	} else {
		for _, o := range got {
			if o != "failure" {
				t.Errorf("approve outcome = %q, want failure", o)
			}
		}
	}
}

// A person list that names a portal person whose issuer no portal is registered for is refused 422
// delegate_person_unknown_portal, and the refusal is not a server fault: nothing is logged as a 500.
func TestAdminPersonLists_UnknownPortalKeyRefused(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/api/v1/x", nil)
	w := httptest.NewRecorder()
	writeServerError(w, r, "write person list", fmt.Errorf("write: %w", store.ErrDelegatePersonUnknownPortal))
	if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != reasonDelegatePersonUnknownPortal {
		t.Fatalf("= %d %s, want 422 %s", w.Code, w.Body.String(), reasonDelegatePersonUnknownPortal)
	}
	// The two rulings are different answers: a role a portal person never holds, and an unregistered issuer.
	w = httptest.NewRecorder()
	writeServerError(w, r, "write person list", store.ErrDelegatePersonNotAllowed)
	if reasonOf(t, w) == reasonDelegatePersonUnknownPortal || w.Code != http.StatusUnprocessableEntity {
		t.Errorf("not-allowed = %d %s", w.Code, w.Body.String())
	}
	// Anything else is still a server fault.
	w = httptest.NewRecorder()
	writeServerError(w, r, "write person list", fmt.Errorf("db down"))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("a store failure = %d, want 500", w.Code)
	}
}
