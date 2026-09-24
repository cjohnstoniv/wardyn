// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed proof of the per-user Azure DevOps sign-in store's
// namespace discipline. ado_entra_test.go's TestADOEntraBlobIsNotReadableByAnotherPrincipal
// and TestADOEntraSecretNameIsReserved already pin this against memSecrets;
// these three cases pin the SAME properties against a real
// secretstore/pg.Store (age-encrypted Postgres), the way
// injection_owner_pg_test.go pins invariant 1 against a real store rather
// than only a fake that could quietly diverge from it. Skipped cleanly when
// WARDYN_TEST_PG is unset (via throwawayPGPool, through newRunOwnerPGHarness).
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// TestADOEntraStorePG_NamespaceIsolation is case (a): two principals each
// store a blob under the same provider row, and each read sees only its own.
func TestADOEntraStorePG_NamespaceIsolation(t *testing.T) {
	h, _ := newRunOwnerPGHarness(t)
	s := h.srv
	ctx := context.Background()
	const alice, bob = "alice-ado-pg", "bob-ado-pg"
	const rowID = "ado-row-pg-a"

	if err := s.storeADOEntraBlob(ctx, alice, rowID, adoEntraBlob{
		RefreshToken: "alices-refresh-token", Scopes: []string{"vso.code"},
		TenantID: "tenant-a", ClientID: "client-a", Subject: alice,
	}); err != nil {
		t.Fatalf("store alice's sign-in: %v", err)
	}
	if err := s.storeADOEntraBlob(ctx, bob, rowID, adoEntraBlob{
		RefreshToken: "bobs-refresh-token", Scopes: []string{"vso.code"},
		TenantID: "tenant-a", ClientID: "client-a", Subject: bob,
	}); err != nil {
		t.Fatalf("store bob's sign-in: %v", err)
	}

	aliceBlob, found, err := s.readADOEntraBlob(ctx, alice, rowID)
	if err != nil || !found || aliceBlob.RefreshToken != "alices-refresh-token" {
		t.Fatalf("alice's own read: found=%v err=%v blob=%+v; want her own refresh token", found, err, aliceBlob)
	}
	bobBlob, found, err := s.readADOEntraBlob(ctx, bob, rowID)
	if err != nil || !found || bobBlob.RefreshToken != "bobs-refresh-token" {
		t.Fatalf("bob's own read: found=%v err=%v blob=%+v; want his own refresh token", found, err, bobBlob)
	}

	// Each principal's List (the read path's own first step) carries only
	// their own row, on the real backing store.
	aliceNames, err := s.cfg.Secrets.For(alice).List(ctx)
	if err != nil {
		t.Fatalf("list alice's namespace: %v", err)
	}
	bobNames, err := s.cfg.Secrets.For(bob).List(ctx)
	if err != nil {
		t.Fatalf("list bob's namespace: %v", err)
	}
	name := adoEntraSecretName(rowID)
	if !slices.Contains(aliceNames, name) || !slices.Contains(bobNames, name) {
		t.Fatalf("both namespaces should list their own row %q: alice=%v bob=%v", name, aliceNames, bobNames)
	}
}

// TestADOEntraStorePG_ReservedNameRefusedThroughGenericSecrets is case (b): a
// secret name matching the reserved ADO pattern (adoEntraSecretName /
// adoEntraRowIDPattern), attempted through the GENERIC secrets PUT, must not
// silently land a caller-controlled blob where the ADO reader would find it.
//
// CURRENT BEHAVIOR (asserted below, not assumed): the generic secrets API
// refuses the write outright with 403 before it ever reaches the store —
// writableSecretName's secretsAPIReserved guard covers it via reservedSecret's
// `wardyn-harness-*-oauth` pattern (secrets.go). So there is nothing for
// readADOEntraBlob to ever misread as an ADO blob; this test pins the refusal
// AND confirms the row stays absent from the ADO read path, both against a
// real Postgres-backed store.
func TestADOEntraStorePG_ReservedNameRefusedThroughGenericSecrets(t *testing.T) {
	h, _ := newRunOwnerPGHarness(t)
	const owner = "alice-ado-pg-b"
	alice := ssoSession(t, owner, "alice-b@corp.example", oidc.RoleUser)
	const rowID = "ado-row-pg-b"
	name := adoEntraSecretName(rowID)

	w := doSSO(t, h.srv, http.MethodPut, "/api/v1/secrets/"+name, alice, `{"value":"not-a-real-oauth-blob"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("PUT %s through the generic secrets API: status %d body %q; want 403 (refused as a reserved name)",
			name, w.Code, w.Body.String())
	}

	if _, found, err := h.srv.readADOEntraBlob(context.Background(), owner, rowID); err != nil || found {
		t.Fatalf("readADOEntraBlob after a refused generic PUT: found=%v err=%v; want nothing stored", found, err)
	}
}

// TestADOEntraStorePG_OwnerNamespaceFirstNoOperatorFallback is case (c): the
// same lookup-order property ado_entra_test.go's
// TestADOEntraBlobIsNotReadableByAnotherPrincipal pins against memSecrets —
// the owner's own namespace is checked first (via List, never a bare
// For(owner).Get, which falls back to the operator's row by contract) — here
// proven against a real Postgres-backed store.
func TestADOEntraStorePG_OwnerNamespaceFirstNoOperatorFallback(t *testing.T) {
	h, sec := newRunOwnerPGHarness(t)
	s := h.srv
	ctx := context.Background()
	const rowID = "ado-row-pg-c"
	name := adoEntraSecretName(rowID)

	// Seed an operator-namespace blob directly against the real store
	// (bypassing the generic API, which case (b) proved refuses this name),
	// at the exact reserved name a member's own row would use.
	raw, err := json.Marshal(adoEntraBlob{
		RefreshToken: "operators-refresh-token", Scopes: []string{"vso.code"},
		TenantID: "tenant-c", ClientID: "client-c", Subject: "the-operator",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := sec.Put(ctx, name, raw); err != nil {
		t.Fatalf("seed the operator namespace: %v", err)
	}

	// A member who has captured nothing herself must not be served the
	// operator's row.
	if blob, found, err := s.readADOEntraBlob(ctx, "carol-ado-pg-c", rowID); err != nil || found {
		t.Fatalf("a member with no capture read the operator's row: found=%v err=%v blob=%+v", found, err, blob)
	}
	// An ownerless read resolves nothing either — there is no principal to
	// read it for, so it must not be answered from the operator namespace.
	if _, found, err := s.readADOEntraBlob(ctx, "", rowID); err != nil || found {
		t.Fatalf("an ownerless read resolved something: found=%v err=%v", found, err)
	}
}
