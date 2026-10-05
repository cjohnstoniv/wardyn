// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
)

// killLogin marks principal's sign-in identity suspended and purged, as a leaver's is.
func killLogin(t *testing.T, pool *pgxpool.Pool, principal string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `INSERT INTO principal_identities (principal, issuer, deactivated_at, purged_at) VALUES ($1, 'test-issuer', now(), now())`, principal); err != nil {
		t.Fatal(err)
	}
}

// A deactivated or purged person's stale login groups refuse nobody: their last sign-in was an overage login
// (or carried a group), but they can never sign in again, so a group write goes through.
func TestPG_KeyDomains_DeadLoginDoesNotRefuseAGroupWrite(t *testing.T) {
	_, srv, svc, pool := keyDomainFixturePool(t)
	if err := svc.RecordLoginGroups(t.Context(), "leaver1", []string{"eng"}, true); err != nil {
		t.Fatal(err)
	}
	killLogin(t, pool, "leaver1")
	if err := svc.RecordLoginGroups(t.Context(), "leaver2", []string{"eng", "ops"}, false); err != nil {
		t.Fatal(err)
	}
	killLogin(t, pool, "leaver2")
	if code, body := keyDomainPut(t, srv, "group", "ops", `{"domain":"a"}`); code != http.StatusCreated {
		t.Fatalf("PUT group beside a purged truncated login = %d %s; want 201", code, body)
	}
	if code, body := keyDomainPut(t, srv, "group", "eng", `{"domain":"b"}`); code != http.StatusCreated {
		t.Fatalf("PUT group that would split a purged person across two domains = %d %s; want 201", code, body)
	}
}

// A live truncated login is refused by name.
func TestPG_KeyDomains_TruncatedRefusalNamesThePeople(t *testing.T) {
	_, srv, svc, _ := keyDomainFixturePool(t)
	if err := svc.RecordLoginGroups(t.Context(), "carol", []string{"eng"}, true); err != nil {
		t.Fatal(err)
	}
	code, body := keyDomainPut(t, srv, "group", "finance", `{"domain":"a"}`)
	if code != http.StatusConflict || !strings.Contains(body, "carol") {
		t.Fatalf("PUT group beside a truncated login = %d %s; want 409 naming carol", code, body)
	}
}

// Deleting the user assignment of a person whose last sign-in lost groups, while a group is assigned, would
// leave their next key refused by name, so it is refused like the group write that would do the same.
func TestPG_KeyDomains_DeleteOfATruncatedPersonsAssignmentIsRefused(t *testing.T) {
	_, srv, svc, pool := keyDomainFixturePool(t)
	ctx := t.Context()
	if err := svc.RecordLoginGroups(ctx, "bob", []string{"eng"}, true); err != nil {
		t.Fatal(err)
	}
	if code, body := keyDomainPut(t, srv, "user", "bob", `{"domain":"a"}`); code != http.StatusCreated {
		t.Fatalf("PUT user = %d %s, want 201", code, body)
	}
	if code, body := keyDomainPut(t, srv, "group", "finance", `{"domain":"b"}`); code != http.StatusCreated {
		t.Fatalf("PUT group with the person assigned = %d %s, want 201", code, body)
	}
	del := func() (int, string) {
		sa := ssoSession(t, "sec-1", "sec@corp.example", oidc.RoleSecurityAdmin)
		w := doSSO(t, srv, http.MethodDelete, "/api/v1/key-domains/assignments/user/bob", sa, "")
		return w.Code, w.Body.String()
	}
	if code, body := del(); code != http.StatusConflict || !strings.Contains(body, "key_domain_ambiguous_membership") || !strings.Contains(body, "bob") {
		t.Fatalf("DELETE the truncated person's assignment = %d %s; want 409 key_domain_ambiguous_membership naming bob", code, body)
	}
	if _, ok, _ := svc.Get(ctx, "user", "bob"); !ok {
		t.Fatal("a refused delete removed the assignment")
	}
	// Once they cannot sign in, nothing is left to refuse.
	killLogin(t, pool, "bob")
	if code, body := del(); code != http.StatusNoContent {
		t.Fatalf("DELETE after the person is purged = %d %s; want 204", code, body)
	}
}

// A held delete of that assignment is refused when proposed and again at approval, the check re-run on the
// assignments as they then stand.
func TestPG_KeyDomainAssignment_HeldDeleteOfATruncatedPersonIsRefused(t *testing.T) {
	const userPath = "/api/v1/key-domains/assignments/user/dana"
	e, svc := newKeyDomainGovEnv(t)
	ctx := t.Context()
	if _, err := svc.Set(ctx, keydomain.Assignment{SubjectType: "user", Subject: "dana", Domain: "a", SetBy: "seed"}); err != nil {
		t.Fatal(err)
	}
	// Proposed while no group is assigned: nothing to refuse yet.
	held := e.pending(e.call(e.alice, http.MethodDelete, userPath, ""))
	if err := svc.RecordLoginGroups(ctx, "dana", []string{"eng"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Set(ctx, keydomain.Assignment{SubjectType: "group", Subject: "eng", Domain: "b", SetBy: "seed"}); err != nil {
		t.Fatal(err)
	}
	if w := e.call(e.bob, http.MethodPost, approvePath(held.ID), ""); w.Code != http.StatusConflict || wireReason(t, w) != "key_domain_ambiguous_membership" {
		t.Fatalf("approving the delete = %d %s, want 409 key_domain_ambiguous_membership", w.Code, w.Body)
	}
	if _, found := e.keyDomainOf(svc, "user", "dana"); !found {
		t.Fatal("a refused approval removed the assignment")
	}
	if w := e.call(e.alice, http.MethodDelete, userPath, ""); w.Code != http.StatusConflict || wireReason(t, w) != "key_domain_ambiguous_membership" {
		t.Fatalf("proposing the delete now = %d %s, want 409 key_domain_ambiguous_membership", w.Code, w.Body)
	}
}
