// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Governance four-eyes for key-domain assignments, against a real Postgres: with the switch on, a
// human's PUT or DELETE is held, and only a distinct security admin's approval writes it.
//
// Guarded by WARDYN_TEST_PG (throwawayPGPool): skipped cleanly when unset, must PASS when set.

import (
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
)

// newKeyDomainGovEnv is a four-eyes environment whose key-domain service declares "a" and "b".
func newKeyDomainGovEnv(t *testing.T) (*govEnv, *keydomain.Service) {
	t.Helper()
	t.Setenv(envGovernanceSecondHuman, "true")
	pool := throwawayPGPool(t)
	svc := keydomain.NewService(pool, []string{"a", "b"})
	return newGovEnvOn(t, pool, func(c *Config) { c.KeyDomains = svc }), svc
}

const keyDomainEngPath = "/api/v1/key-domains/assignments/group/eng"

func (e *govEnv) keyDomainOf(svc *keydomain.Service, subjectType, subject string) (string, bool) {
	e.t.Helper()
	got, found, err := svc.Get(e.t.Context(), subjectType, subject)
	if err != nil {
		e.t.Fatal(err)
	}
	return got.Domain, found
}

func TestPG_KeyDomainAssignment_HeldForSecondHuman(t *testing.T) {
	e, svc := newKeyDomainGovEnv(t)

	// A set is held: nothing is written until a second security admin approves.
	set := e.pending(e.call(e.alice, http.MethodPut, keyDomainEngPath, `{"domain":"a"}`))
	if set.TargetKind != "key_domain_assignment" || set.Op != "upsert" || set.TargetKey != "group:eng" {
		t.Errorf("held set = %s %s %s, want key_domain_assignment upsert group:eng", set.TargetKind, set.Op, set.TargetKey)
	}
	if _, found := e.keyDomainOf(svc, "group", "eng"); found {
		t.Fatal("a held set was written")
	}
	if n := len(e.audits("key_domain.assignment.set")); n != 0 {
		t.Fatalf("%d key_domain.assignment.set rows before approval, want 0", n)
	}
	if w := e.call(e.alice, http.MethodPost, approvePath(set.ID), ""); w.Code != http.StatusForbidden || wireReason(t, w) != "second_human_required" {
		t.Fatalf("self-approval = %d %s, want 403 second_human_required", w.Code, w.Body)
	}
	if _, found := e.keyDomainOf(svc, "group", "eng"); found {
		t.Fatal("a refused self-approval wrote the assignment")
	}
	e.approve(set.ID)
	if d, found := e.keyDomainOf(svc, "group", "eng"); !found || d != "a" {
		t.Fatalf("after approval group:eng = (%q, %v), want a", d, found)
	}
	if got, _, _ := svc.Get(t.Context(), "group", "eng"); got.SetBy != govSubAlice {
		t.Errorf("set_by = %q, want the proposer", got.SetBy)
	}
	if rows := e.audits("key_domain.assignment.set"); len(rows) != 1 || rows[0].Actor != govSubBob || rows[0].Target != "group:eng" {
		t.Errorf("key_domain.assignment.set rows = %+v, want one by the approver naming group:eng", rows)
	} else if d := auditData(t, rows[0]); d["proposed_by"] != govSubAlice || d["change_id"] == nil || d["domain"] != "a" || d["created"] != true {
		t.Errorf("set audit data = %v, want domain, created, change_id and proposed_by", d)
	}

	// A delete is held likewise.
	del := e.pending(e.call(e.alice, http.MethodDelete, keyDomainEngPath, ""))
	if del.Op != "delete" || del.TargetKey != "group:eng" {
		t.Errorf("held delete = %s %s, want delete group:eng", del.Op, del.TargetKey)
	}
	if _, found := e.keyDomainOf(svc, "group", "eng"); !found {
		t.Fatal("a held delete removed the assignment")
	}
	if w := e.call(e.alice, http.MethodPost, approvePath(del.ID), ""); w.Code != http.StatusForbidden {
		t.Fatalf("self-approval of the delete = %d %s, want 403", w.Code, w.Body)
	}
	e.approve(del.ID)
	if _, found := e.keyDomainOf(svc, "group", "eng"); found {
		t.Fatal("the approved delete left the assignment")
	}
	if rows := e.audits("key_domain.assignment.delete"); len(rows) != 1 || rows[0].Actor != govSubBob {
		t.Errorf("key_domain.assignment.delete rows = %+v, want one by the approver", rows)
	} else if d := auditData(t, rows[0]); d["proposed_by"] != govSubAlice || d["change_id"] == nil || d["domain"] != "a" {
		t.Errorf("delete audit data = %v, want the removed domain, change_id and proposed_by", d)
	}

	// A delete of nothing is the direct 404, never held; an undeclared domain is the direct refusal.
	if w := e.call(e.alice, http.MethodDelete, keyDomainEngPath, ""); w.Code != http.StatusNotFound {
		t.Errorf("held delete of a missing assignment = %d %s, want 404", w.Code, w.Body)
	}
	if w := e.call(e.alice, http.MethodPut, keyDomainEngPath, `{"domain":"ghost"}`); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("an undeclared domain = %d %s, want the direct 422", w.Code, w.Body)
	}
	if n := e.pendingCount(); n != 0 {
		t.Errorf("%d pending changes after refusals, want 0", n)
	}
}

// TestPG_KeyDomainAssignment_StaleAndRevalidated: an approval compares the assignment with what the
// proposal saw, and re-checks the membership against the state it finds.
func TestPG_KeyDomainAssignment_StaleAndRevalidated(t *testing.T) {
	e, svc := newKeyDomainGovEnv(t)

	// The target changed under a held set (the admin token's break-glass write): stale.
	set := e.pending(e.call(e.alice, http.MethodPut, keyDomainEngPath, `{"domain":"a"}`))
	if w := e.admin(http.MethodPut, keyDomainEngPath, `{"domain":"b"}`); w.Code != http.StatusCreated {
		t.Fatalf("admin-token PUT = %d %s, want the direct 201", w.Code, w.Body)
	}
	if rows := e.audits("governance.change.bypass"); len(rows) != 1 || rows[0].Target != "group:eng" {
		t.Errorf("governance.change.bypass rows = %+v, want one naming group:eng", rows)
	}
	w := e.call(e.bob, http.MethodPost, approvePath(set.ID), "")
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeStale {
		t.Fatalf("approving a set whose target changed = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeStale)
	}
	if d, _ := e.keyDomainOf(svc, "group", "eng"); d != "b" {
		t.Fatalf("a stale approval wrote %q", d)
	}

	// The same for a held delete.
	del := e.pending(e.call(e.alice, http.MethodDelete, keyDomainEngPath, ""))
	if w := e.admin(http.MethodPut, keyDomainEngPath, `{"domain":"a"}`); w.Code != http.StatusOK {
		t.Fatalf("admin-token re-PUT = %d %s, want 200", w.Code, w.Body)
	}
	if w := e.call(e.bob, http.MethodPost, approvePath(del.ID), ""); w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeStale {
		t.Fatalf("approving a delete whose target changed = %d %s, want 409 stale", w.Code, w.Body)
	}
	if _, found := e.keyDomainOf(svc, "group", "eng"); !found {
		t.Fatal("a stale delete removed the assignment")
	}

	// A held group set that another assignment has since made ambiguous is refused at approval.
	if err := svc.RecordLoginGroups(t.Context(), "gina", []string{"ops", "qa"}, false); err != nil {
		t.Fatal(err)
	}
	ops := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/key-domains/assignments/group/ops", `{"domain":"a"}`))
	if _, err := svc.Set(t.Context(), keydomain.Assignment{SubjectType: "group", Subject: "qa", Domain: "b", SetBy: "seed"}); err != nil {
		t.Fatal(err)
	}
	if w := e.call(e.bob, http.MethodPost, approvePath(ops.ID), ""); w.Code != http.StatusConflict || wireReason(t, w) != "key_domain_ambiguous_membership" {
		t.Fatalf("approving a set made ambiguous = %d %s, want 409 key_domain_ambiguous_membership", w.Code, w.Body)
	}
	if _, found := e.keyDomainOf(svc, "group", "ops"); found {
		t.Fatal("a refused approval wrote the assignment")
	}
}

// TestPG_KeyDomainAssignment_SwitchOffIsDirect: with the switch off a human's write applies at once.
func TestPG_KeyDomainAssignment_SwitchOffIsDirect(t *testing.T) {
	e, svc := newKeyDomainGovEnv(t)
	t.Setenv(envGovernanceSecondHuman, "")
	if w := e.call(e.alice, http.MethodPut, keyDomainEngPath, `{"domain":"a"}`); w.Code != http.StatusCreated {
		t.Fatalf("switch-off PUT = %d %s, want 201", w.Code, w.Body)
	}
	if d, found := e.keyDomainOf(svc, "group", "eng"); !found || d != "a" {
		t.Fatalf("switch-off PUT stored (%q, %v)", d, found)
	}
	if w := e.call(e.alice, http.MethodDelete, keyDomainEngPath, ""); w.Code != http.StatusNoContent {
		t.Fatalf("switch-off DELETE = %d %s, want 204", w.Code, w.Body)
	}
	if n := e.pendingCount(); n != 0 || len(e.audits("governance.change.bypass")) != 0 {
		t.Errorf("switch off: %d pending, %d bypass rows; want none", n, len(e.audits("governance.change.bypass")))
	}
}
