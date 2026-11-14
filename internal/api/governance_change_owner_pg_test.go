// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The owner pins: the rights a proposer holds over their own held change (read it, write its note,
// withdraw it) follow the principal, and follow a mailbox only when sign-in vouches for it. The refusal
// of a proposer approving their own change still matches an unvouched mailbox.

// govOwnerSession is a member's session, group snapshot taken and empty, carrying the email given.
func govOwnerSession(t *testing.T, sub, email string) *http.Cookie {
	t.Helper()
	payload, err := json.Marshal(oidc.Session{
		V: oidc.SessionCodecVersion, Sub: sub, Email: email, Role: oidc.RoleUser, UserType: types.UserTypeStandard,
		Expiry: time.Now().UTC().Add(time.Hour), Groups: []string{},
	})
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	return signedSessionCookie(payload)
}

// govOwnerHeld is a pending membership add on ua-a proposed by proposer (email proposer@corp.example), who
// is granted on ua-a for it; it returns the change and that grant.
func govOwnerHeld(e *unitAdminEnv, proposer string) (types.GovernanceChange, types.GovernanceUnitAdmin) {
	e.t.Helper()
	g := e.grant(e.a, types.CapabilitySubjectUser, proposer)
	person := e.fresh("sub-owner-person")
	treeMember(e.t, e.govEnv, e.child(e.a), types.CapabilitySubjectUser, person, 1, false)
	ch := e.pending(e.call(govOwnerSession(e.t, proposer, proposer+"@corp.example"), http.MethodPost, "/api/v1/governance/memberships", membershipBody(person, e.a.ID)))
	return ch, g
}

// govOwnerListed reports whether c's list of pending changes holds id.
func govOwnerListed(e *unitAdminEnv, c *http.Cookie, id uuid.UUID) bool {
	e.t.Helper()
	w := e.call(c, http.MethodGet, "/api/v1/governance/changes", "")
	if w.Code != http.StatusOK {
		e.t.Fatalf("list = %d %s", w.Code, w.Body)
	}
	var out []types.GovernanceChange
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		e.t.Fatalf("list body %s: %v", w.Body, err)
	}
	for _, ch := range out {
		if ch.ID == id {
			return true
		}
	}
	return false
}

func govOwnerNotePath(id uuid.UUID) string {
	return "/api/v1/governance/changes/" + id.String() + "/note"
}

// An admin of ua-b whose unvouched email equals the proposer's neither lists, reads, notes nor rejects
// the proposer's change on ua-a. The proposer, moved to ua-b alone and signed in with another email,
// still withdraws it: the principal owns the change.
func TestGovernanceOwnerUnvouchedMailboxGrantsNothing(t *testing.T) {
	e := newUnitAdminEnv(t, true)
	proposer := e.fresh("sub-owner-proposer")
	ch, g := govOwnerHeld(e, proposer)
	twin := govOwnerSession(t, uaSubAdminB, proposer+"@corp.example")

	if govOwnerListed(e, twin, ch.ID) {
		t.Error("the mailbox twin in ua-b lists the change")
	}
	if w := e.call(twin, http.MethodGet, "/api/v1/governance/changes/"+ch.ID.String(), ""); w.Code != http.StatusNotFound || wireReason(t, w) != reasonGovernanceChangeNotFound {
		t.Errorf("the twin's read = %d %s, want 404 %s", w.Code, w.Body, reasonGovernanceChangeNotFound)
	}
	if w := e.call(twin, http.MethodPut, govOwnerNotePath(ch.ID), `{"reason":"mine"}`); w.Code != http.StatusNotFound || wireReason(t, w) != reasonGovernanceChangeNotFound {
		t.Errorf("the twin's note = %d %s, want 404 %s", w.Code, w.Body, reasonGovernanceChangeNotFound)
	}
	if w := e.call(twin, http.MethodPost, rejectPath(ch.ID), ""); !scopeRefused(t, w) {
		t.Errorf("the twin's reject = %d %s, want 403 unit_scope_denied", w.Code, w.Body)
	}
	if st, n := e.changeState(ch.ID), len(e.audits("governance.change.reject")); st != types.GovernanceChangePending || n != 0 {
		t.Fatalf("after the twin's attempts the change is %q with %d reject rows, want pending and none", st, n)
	}

	e.grant(e.b, types.CapabilitySubjectUser, proposer)
	if _, err := store.RevokeUnitAdminQ(context.Background(), e.pool, e.a.ID, g.ID); err != nil {
		t.Fatalf("revoke the proposer's ua-a grant: %v", err)
	}
	if w := e.call(govOwnerSession(t, proposer, "moved@corp.example"), http.MethodPost, rejectPath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("the proposer's withdrawal = %d %s, want 200", w.Code, w.Body)
	}
	rows := e.audits("governance.change.reject")
	if st := e.changeState(ch.ID); st != types.GovernanceChangeRejected || len(rows) != 1 || auditData(t, rows[0])["withdrawn"] != true {
		t.Errorf("after the withdrawal the change is %q with %d reject rows, want rejected and one row marked withdrawn", st, len(rows))
	}
}

// An admin of ua-a whose unvouched email equals the proposer's (in another case) may decide the change
// but not approve it, the same-human refusal; may not write its note; and a reject is theirs as an
// approver, not a withdrawal.
func TestGovernanceOwnerSameMailboxApproverIsNoOwner(t *testing.T) {
	e := newUnitAdminEnv(t, true)
	proposer := e.fresh("sub-owner-proposer")
	ch, _ := govOwnerHeld(e, proposer)
	twin := govOwnerSession(t, uaSubPeerA, strings.ToUpper(proposer)+"@CORP.EXAMPLE")

	if w := e.call(twin, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusForbidden || wireReason(t, w) != "second_human_required" {
		t.Errorf("the twin approver's approval = %d %s, want 403 second_human_required", w.Code, w.Body)
	}
	if w := e.call(twin, http.MethodPut, govOwnerNotePath(ch.ID), `{"reason":"mine"}`); w.Code != http.StatusForbidden || wireReason(t, w) != reasonGovernanceChangeNoteProposerOnly {
		t.Errorf("the twin approver's note = %d %s, want 403 %s", w.Code, w.Body, reasonGovernanceChangeNoteProposerOnly)
	}
	if st := e.changeState(ch.ID); st != types.GovernanceChangePending {
		t.Fatalf("after a refused approval the change is %q, want pending", st)
	}
	if w := e.call(twin, http.MethodPost, rejectPath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("the twin approver's reject = %d %s, want 200", w.Code, w.Body)
	}
	if rows := e.audits("governance.change.reject"); len(rows) != 1 || auditData(t, rows[0])["withdrawn"] != nil {
		t.Errorf("reject rows = %d, want one not marked withdrawn (an approver decided it)", len(rows))
	}
}

// With email keys opted in (Config.AllowEmailMappings) sign-in vouches for every email, so a mailbox
// equal to the proposer's owns the change under another principal: it lists, reads, notes and
// withdraws it.
func TestGovernanceOwnerVouchedMailboxOwns(t *testing.T) {
	e := newUnitAdminEnv(t, true, func(c *Config) { c.AllowEmailMappings = true })
	proposer := e.fresh("sub-owner-proposer")
	ch, _ := govOwnerHeld(e, proposer)
	twin := govOwnerSession(t, uaSubAdminB, proposer+"@corp.example")

	if !govOwnerListed(e, twin, ch.ID) {
		t.Error("the vouched mailbox in ua-b does not list the change")
	}
	if w := e.call(twin, http.MethodGet, "/api/v1/governance/changes/"+ch.ID.String(), ""); w.Code != http.StatusOK {
		t.Errorf("the vouched mailbox's read = %d %s, want 200", w.Code, w.Body)
	}
	if w := e.call(twin, http.MethodPut, govOwnerNotePath(ch.ID), `{"reason":"mine"}`); w.Code != http.StatusOK {
		t.Errorf("the vouched mailbox's note = %d %s, want 200", w.Code, w.Body)
	}
	if w := e.call(twin, http.MethodPost, rejectPath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("the vouched mailbox's withdrawal = %d %s, want 200", w.Code, w.Body)
	}
	if rows := e.audits("governance.change.reject"); len(rows) != 1 || auditData(t, rows[0])["withdrawn"] != true {
		t.Errorf("reject rows = %d, want one marked withdrawn", len(rows))
	}
}

// On /governance/requests, which every member reaches: a decider whose unvouched email equals the
// requester's sees the request as neither theirs nor decidable, and a member outside the deciders with
// that email neither lists nor rejects it.
func TestGovernanceOwnerUnvouchedMailboxOnRequests(t *testing.T) {
	e := newElevEnv(t)
	c, sub := e.requester()
	got := e.ask(c, e.a1, "1h", "")
	mail := sub + "@corp.example"

	var view GovernanceRequestView
	for _, v := range e.list(govOwnerSession(t, uaSubAdminA1, mail)).Requests {
		if v.ID == got.Pending.ID {
			view = v
		}
	}
	if view.ID != got.Pending.ID || view.Mine || view.Decidable {
		t.Errorf("the decider twin's view = %+v, want the request listed, not mine and not decidable", view)
	}
	stranger := govOwnerSession(t, "sub-owner-stranger", mail)
	for _, v := range e.list(stranger).Requests {
		if v.ID == got.Pending.ID {
			t.Error("a member outside the deciders lists the request by its requester's email")
		}
	}
	if w := e.call(stranger, http.MethodPost, requestPath(got.Pending.ID, "reject"), ""); w.Code != http.StatusNotFound || wireReason(t, w) != reasonGovernanceChangeNotFound {
		t.Errorf("the stranger's reject = %d %s, want 404 %s", w.Code, w.Body, reasonGovernanceChangeNotFound)
	}
	if st := e.changeState(got.Pending.ID); st != types.GovernanceChangePending {
		t.Errorf("after the stranger's reject the request is %q, want pending", st)
	}
}
