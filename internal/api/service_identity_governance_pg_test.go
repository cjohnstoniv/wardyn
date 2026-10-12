// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// svcGovEnv is a service identity deployment with WARDYN_GOVERNANCE_SECOND_HUMAN on: sec proposes, sec2
// is a second security admin.
type svcGovEnv struct {
	svcEnv
	sec2 *http.Cookie
}

func newSvcGovEnv(t *testing.T) svcGovEnv {
	t.Helper()
	e := svcGovEnv{svcEnv: newSvcEnv(t)}
	e.bind(t, "sec2", "https://idp.example", "", "")
	e.sec2 = accessSession(t, "sec2", "sec2@corp.example", oidc.RoleSecurityAdmin, []string{})
	return e
}

func (e svcGovEnv) on(t *testing.T) { t.Setenv(envGovernanceSecondHuman, "1") }

// held reads the pending change out of a 202, failing unless the write was held.
func (e svcGovEnv) held(t *testing.T, w *httptest.ResponseRecorder) types.GovernanceChange {
	t.Helper()
	var body struct {
		PendingChange types.GovernanceChange `json:"pending_change"`
	}
	if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.PendingChange.ID == uuid.Nil {
		t.Fatalf("want the write held (202 pending_change), got %d %s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/api/v1/governance/changes/"+body.PendingChange.ID.String() {
		t.Errorf("Location %q, want the change", loc)
	}
	if body.PendingChange.TargetKind != govKindServiceIdentity {
		t.Errorf("target_kind %q, want %q", body.PendingChange.TargetKind, govKindServiceIdentity)
	}
	return body.PendingChange
}

func (e svcGovEnv) decide(t *testing.T, as *http.Cookie, id uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.h.srv, http.MethodPost, "/api/v1/governance/changes/"+id.String()+"/approve", as, "")
}

func (e svcGovEnv) patch(t *testing.T, as *http.Cookie, name, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.h.srv, http.MethodPatch, "/api/v1/service-identities/"+name, as, body)
}

func (e svcGovEnv) stored(t *testing.T, name string) types.ServiceIdentity {
	t.Helper()
	si, err := e.st.GetServiceIdentity(context.Background(), name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return si
}

func (e svcGovEnv) audits(action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range e.h.audit.snapshot() {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

// TestServiceIdentityGov_CreateHeld: a human's create is held, nothing is created until a distinct
// security-tier human approves it, the proposer (by principal or by mailbox) cannot, and the approval
// writes the create row with the approver as actor.
func TestServiceIdentityGov_CreateHeld(t *testing.T) {
	e := newSvcGovEnv(t)
	e.on(t)
	ch := e.held(t, e.create(t, e.sec, svcName, svcSponsor))
	if _, err := e.st.GetServiceIdentity(context.Background(), svcName); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a held create is stored: %v", err)
	}
	if n := len(e.audits("service_identity.create")); n != 0 {
		t.Errorf("service_identity.create rows before approval: %d, want 0", n)
	}
	if w := e.decide(t, e.sec, ch.ID); w.Code != http.StatusForbidden || respReason(w) != string(authz.ReasonSecondHumanRequired) {
		t.Errorf("the proposer approves: %d %s, want 403 second_human_required", w.Code, w.Body.String())
	}
	sameMailbox := accessSession(t, "sec-alt", "sec@corp.example", oidc.RoleSecurityAdmin, []string{})
	if w := e.decide(t, sameMailbox, ch.ID); w.Code != http.StatusForbidden {
		t.Errorf("the proposer's mailbox under another principal approves: %d %s, want 403", w.Code, w.Body.String())
	}
	if w := e.decide(t, e.mem, ch.ID); w.Code != http.StatusForbidden {
		t.Errorf("a member approves: %d %s, want 403", w.Code, w.Body.String())
	}
	if w := e.decide(t, e.sec2, ch.ID); w.Code != http.StatusOK {
		t.Fatalf("a second security admin approves: %d %s, want 200", w.Code, w.Body.String())
	}
	si := e.stored(t, svcName)
	if si.Sponsor != svcSponsor || si.CreatedBy != "sec" {
		t.Errorf("created %+v, want sponsor %q created by the proposer", si, svcSponsor)
	}
	rows := e.audits("service_identity.create")
	if len(rows) != 1 || rows[0].Actor != "sec2" || dataOf(t, rows[0])["change_id"] != ch.ID.String() {
		t.Errorf("service_identity.create rows %+v, want one by the approver carrying the change id", rows)
	}
}

// TestServiceIdentityGov_SponsorChangeHeldWhole: a PATCH that changes the sponsor is held whole, its
// description and suspend parts included, and on approval it all applies and the identity's tokens are
// revoked.
func TestServiceIdentityGov_SponsorChangeHeldWhole(t *testing.T) {
	e := newSvcGovEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	tok := e.mustMint(t, e.sponsor, svcName)
	e.on(t)
	ch := e.held(t, e.patch(t, e.sec, svcName, `{"sponsor":"`+svcSponsor2+`","description":"moved","suspended":true}`))
	if si := e.stored(t, svcName); si.Sponsor != svcSponsor || si.Description == "moved" || si.SuspendedAt != nil {
		t.Fatalf("part of a held PATCH applied before approval: %+v", si)
	}
	if w := e.as(t, tok.Token, http.MethodGet, "/api/v1/me", ""); w.Code != http.StatusOK {
		t.Errorf("the service token stopped working while the change is held: %d", w.Code)
	}
	if w := e.decide(t, e.sec, ch.ID); w.Code != http.StatusForbidden {
		t.Errorf("the proposer approves: %d, want 403", w.Code)
	}
	if w := e.decide(t, e.sec2, ch.ID); w.Code != http.StatusOK {
		t.Fatalf("a second security admin approves: %d %s", w.Code, w.Body.String())
	}
	si := e.stored(t, svcName)
	if si.Sponsor != svcSponsor2 || si.Description != "moved" || si.SuspendedAt == nil {
		t.Errorf("after approval %+v, want sponsor %q, the description and suspended", si, svcSponsor2)
	}
	if w := e.as(t, tok.Token, http.MethodGet, "/api/v1/me", ""); w.Code == http.StatusOK {
		t.Error("the service token still works after the approved sponsor change")
	}
	rows := e.audits("service_identity.update")
	if len(rows) != 1 {
		t.Fatalf("service_identity.update rows: %d, want 1", len(rows))
	}
	if d := dataOf(t, rows[0]); d["sponsor_from"] != svcSponsor || d["sponsor_to"] != svcSponsor2 || d["tokens_revoked"] != float64(1) ||
		d["change_id"] != ch.ID.String() || rows[0].Actor != "sec2" {
		t.Errorf("service_identity.update %s data %v, want the transition, one token revoked and the change id", rows[0].Actor, d)
	}
}

// TestServiceIdentityGov_NotHeld: with the switch on, a description edit, a suspend and an unsuspend, a
// PATCH naming the current sponsor, and a retire apply directly.
func TestServiceIdentityGov_NotHeld(t *testing.T) {
	e := newSvcGovEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	e.on(t)
	for _, body := range []string{`{"description":"d1"}`, `{"suspended":true}`, `{"suspended":false}`, `{"sponsor":"` + svcSponsor + `","description":"d2"}`} {
		if w := e.patch(t, e.sec, svcName, body); w.Code != http.StatusOK {
			t.Errorf("PATCH %s: %d %s, want 200 applied", body, w.Code, w.Body.String())
		}
	}
	if si := e.stored(t, svcName); si.Description != "d2" || si.SuspendedAt != nil || si.Sponsor != svcSponsor {
		t.Errorf("after the direct edits %+v", si)
	}
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/service-identities/"+svcName, e.sec, ""); w.Code != http.StatusOK {
		t.Errorf("retire: %d %s, want 200", w.Code, w.Body.String())
	}
	if n := len(e.audits("governance.change.propose")); n != 0 {
		t.Errorf("governance.change.propose rows: %d, want 0", n)
	}
}

// TestServiceIdentityGov_StaleApply: a held write applies only against the state it was reviewed on;
// otherwise the approval answers 409 governance_change_stale and nothing changes.
func TestServiceIdentityGov_StaleApply(t *testing.T) {
	ctx := context.Background()
	stale := func(t *testing.T, e svcGovEnv, id uuid.UUID) {
		t.Helper()
		if w := e.decide(t, e.sec2, id); w.Code != http.StatusConflict || respReason(w) != reasonGovernanceChangeStale {
			t.Errorf("approve: %d %s, want 409 %s", w.Code, w.Body.String(), reasonGovernanceChangeStale)
		}
	}
	unbind := func(t *testing.T, e svcGovEnv, principal string) {
		t.Helper()
		if _, err := e.pool.Exec(ctx, `UPDATE principal_identities SET deactivated_at = now() WHERE principal = $1`, principal); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("create_name_taken", func(t *testing.T) {
		e := newSvcGovEnv(t)
		e.on(t)
		ch := e.held(t, e.create(t, e.sec, svcName, svcSponsor))
		if _, err := e.st.CreateServiceIdentity(ctx, types.ServiceIdentity{Name: svcName, Sponsor: svcSponsor2, UserType: types.UserTypeStandard, CreatedBy: "x"}); err != nil {
			t.Fatal(err)
		}
		stale(t, e, ch.ID)
		if si := e.stored(t, svcName); si.Sponsor != svcSponsor2 {
			t.Errorf("the identity holding the name was changed: %+v", si)
		}
	})
	t.Run("create_type_deleted", func(t *testing.T) {
		e := newSvcGovEnv(t)
		if _, err := e.st.CreateUserType(ctx, types.UserType{ID: "ut-gov", Name: "Gov", Priority: 2, CreatedBy: "sec"}); err != nil {
			t.Fatal(err)
		}
		e.on(t)
		ch := e.held(t, doSSO(t, e.h.srv, http.MethodPost, "/api/v1/service-identities", e.sec,
			`{"name":"`+svcName+`","sponsor":"`+svcSponsor+`","user_type":"ut-gov"}`))
		if err := e.st.DeleteUserType(ctx, "ut-gov"); err != nil {
			t.Fatal(err)
		}
		stale(t, e, ch.ID)
	})
	t.Run("create_sponsor_unbound", func(t *testing.T) {
		e := newSvcGovEnv(t)
		e.on(t)
		ch := e.held(t, e.create(t, e.sec, svcName, svcSponsor))
		unbind(t, e, svcSponsor)
		stale(t, e, ch.ID)
	})
	transfer := func(t *testing.T) (svcGovEnv, types.GovernanceChange) {
		t.Helper()
		e := newSvcGovEnv(t)
		e.mustCreate(t, svcName, svcSponsor)
		e.on(t)
		return e, e.held(t, e.patch(t, e.sec, svcName, `{"sponsor":"`+svcSponsor2+`","description":"moved"}`))
	}
	// unwritten checks that a stale transfer left none of its PATCH behind.
	unwritten := func(t *testing.T, e svcGovEnv) {
		t.Helper()
		if si := e.stored(t, svcName); si.Description == "moved" {
			t.Errorf("a stale held PATCH wrote its description: %+v", si)
		}
	}
	t.Run("transfer_sponsor_changed", func(t *testing.T) {
		e, ch := transfer(t)
		s2 := svcSponsor2
		if _, err := e.st.UpdateServiceIdentity(ctx, svcName, store.ServiceIdentityChange{Sponsor: &s2, By: "x"}); err != nil {
			t.Fatal(err)
		}
		stale(t, e, ch.ID)
		unwritten(t, e)
	})
	t.Run("transfer_retired", func(t *testing.T) {
		e, ch := transfer(t)
		if _, err := e.st.RetireServiceIdentity(ctx, svcName, "x"); err != nil {
			t.Fatal(err)
		}
		stale(t, e, ch.ID)
		unwritten(t, e)
	})
	t.Run("transfer_recreated", func(t *testing.T) {
		e, ch := transfer(t)
		if _, err := e.pool.Exec(ctx, `DELETE FROM service_identities WHERE name = $1`, svcName); err != nil {
			t.Fatal(err)
		}
		if _, err := e.st.CreateServiceIdentity(ctx, types.ServiceIdentity{Name: svcName, Sponsor: svcSponsor, UserType: types.UserTypeStandard, CreatedBy: "x"}); err != nil {
			t.Fatal(err)
		}
		stale(t, e, ch.ID)
		unwritten(t, e)
		if si := e.stored(t, svcName); si.Sponsor != svcSponsor {
			t.Errorf("the recreated identity's sponsor moved: %+v", si)
		}
	})
	t.Run("transfer_new_sponsor_unbound", func(t *testing.T) {
		e, ch := transfer(t)
		unbind(t, e, svcSponsor2)
		stale(t, e, ch.ID)
		unwritten(t, e)
	})
}

// TestServiceIdentityGov_AdminTokenBypass: the admin token creates and moves a service identity directly
// with the switch on, each beside a governance.change.bypass row.
func TestServiceIdentityGov_AdminTokenBypass(t *testing.T) {
	e := newSvcGovEnv(t)
	e.on(t)
	if w := do(t, e.h.srv, http.MethodPost, "/api/v1/service-identities", adminToken,
		`{"name":"`+svcName+`","sponsor":"`+svcSponsor+`"}`); w.Code != http.StatusCreated {
		t.Fatalf("admin-token create: %d %s, want 201", w.Code, w.Body.String())
	}
	if w := do(t, e.h.srv, http.MethodPatch, "/api/v1/service-identities/"+svcName, adminToken,
		`{"sponsor":"`+svcSponsor2+`"}`); w.Code != http.StatusOK {
		t.Fatalf("admin-token sponsor change: %d %s, want 200", w.Code, w.Body.String())
	}
	rows := e.audits("governance.change.bypass")
	if len(rows) != 2 {
		t.Fatalf("governance.change.bypass rows: %d, want 2", len(rows))
	}
	for _, ev := range rows {
		if d := dataOf(t, ev); d["target_kind"] != govKindServiceIdentity || !strings.HasPrefix(ev.Target, types.ServicePrincipalPrefix) {
			t.Errorf("bypass row %s %v, want target_kind %q on the identity", ev.Target, d, govKindServiceIdentity)
		}
	}
}
