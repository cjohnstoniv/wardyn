// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestMintedTokenRowNamesItsServiceIdentity: GET /tokens names the service identity a moved token was
// linked to, and only a token with a move row carries one.
func TestMintedTokenRowNamesItsServiceIdentity(t *testing.T) {
	e := newMoveEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	linked, plain := e.minted(t), e.minted(t)
	if w := e.move(t, e.sec, linked.ID, `{"service_identity":"`+svcName+`"}`); w.Code != http.StatusOK {
		t.Fatalf("move: %d %s", w.Code, w.Body.String())
	}
	inv := e.inventory(t, true)
	if got := inv[linked.ID]; got.Disposition != types.TokenLinkedToService || got.ServiceIdentity != svcName {
		t.Errorf("moved token row = disposition %q, service_identity %q; want %q, %q", got.Disposition, got.ServiceIdentity, types.TokenLinkedToService, svcName)
	}
	if got := inv[plain.ID]; got.ServiceIdentity != "" {
		t.Errorf("an unmoved token names service identity %q", got.ServiceIdentity)
	}
	// The name survives the revoke that finishes the move.
	e.revokeAsAdmin(t, linked.ID)
	if got := e.inventory(t, true)[linked.ID]; got.Disposition != types.TokenMovedToService || got.ServiceIdentity != svcName {
		t.Errorf("revoked moved token row = disposition %q, service_identity %q; want %q, %q", got.Disposition, got.ServiceIdentity, types.TokenMovedToService, svcName)
	}
}

// TestServiceIdentity_MintRefusedWhileTheSponsorIsInactive: an identity whose record is active but whose
// sponsor's session is not live answers the mint like a suspended one, because its token would be refused
// at once. The sponsor's reactivation lifts the refusal.
func TestServiceIdentity_MintRefusedWhileTheSponsorIsInactive(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	setDeactivated := func(sql string) {
		t.Helper()
		if _, err := e.pool.Exec(context.Background(), sql, svcSponsor); err != nil {
			t.Fatal(err)
		}
	}
	setDeactivated(`UPDATE principal_identities SET deactivated_at = now() WHERE principal = $1`)
	w := e.mintReq(t, e.sponsor, svcName, `{"name":"deploy"}`)
	if w.Code != http.StatusConflict || respReason(w) != "service_identity_inactive" {
		t.Fatalf("mint with an inactive sponsor: %d %s, want 409 service_identity_inactive", w.Code, w.Body.String())
	}
	setDeactivated(`UPDATE principal_identities SET deactivated_at = NULL WHERE principal = $1`)
	if w := e.mintReq(t, e.sponsor, svcName, `{"name":"deploy"}`); w.Code != http.StatusCreated {
		t.Fatalf("mint after reactivation: %d %s, want 201", w.Code, w.Body.String())
	}
}
