// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// TestPG_ADOOwnPAT_OwnerOnlyNoFallback is TestReadADOOwnPAT_OwnerOnlyNoFallback
// against a real Postgres-backed store: an operator row and another person's
// row under the sealed own-token name never answer for a member who added
// none, and each person reads their own.
func TestPG_ADOOwnPAT_OwnerOnlyNoFallback(t *testing.T) {
	h, sec := newRunOwnerPGHarness(t)
	s := h.srv
	ctx := secretstore.WithPurpose(context.Background(), secretstore.PurposeStatus)
	name := adoOwnPATSecretName(ownPATRowID)
	seed := func(t *testing.T, owner, token string) {
		t.Helper()
		raw, _ := json.Marshal(adoOwnPATBlob{Token: token, Org: "contoso", ExpiresOn: ownPATNow.AddDate(0, 0, 5)})
		if err := sec.For(owner).Put(ctx, name, raw); err != nil {
			t.Fatalf("seed %q: %v", owner, err)
		}
	}
	seed(t, "", "operators-token")
	seed(t, "carol-own-pg", "carols-token")

	if blob, found, err := s.readADOOwnPAT(ctx, "bob-own-pg", ownPATRowID); err != nil || found {
		t.Fatalf("a member with no token read another namespace's: found=%v err=%v blob=%v", found, err, blob.Org)
	}
	if _, found, err := s.readADOOwnPAT(ctx, "", ownPATRowID); err != nil || found {
		t.Fatalf("an ownerless read resolved something: found=%v err=%v", found, err)
	}
	if blob, found, err := s.readADOOwnPAT(ctx, "carol-own-pg", ownPATRowID); err != nil || !found || blob.Token != "carols-token" {
		t.Fatalf("carol's own read: found=%v err=%v", found, err)
	}
}
