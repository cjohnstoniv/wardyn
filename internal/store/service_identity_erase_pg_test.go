// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A purge also clears the person from the token moves they linked (linked_by),
// and leaves another person's link as it was. Guarded by WARDYN_TEST_PG.
func TestPG_EraseServiceIdentityAttributionOf_ClearsTheTokenMoveLinker(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	now := time.Now().UTC()
	const leaver, other = "sub-leaver", "sub-other"

	if _, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: other, Issuer: "https://issuer.test", Email: "other@corp.example"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateServiceIdentity(ctx, types.ServiceIdentity{Name: "ci-bot", Sponsor: other, UserType: types.UserTypeStandard, CreatedBy: other}); err != nil {
		t.Fatal(err)
	}
	var tokens [2]uuid.UUID
	for i := range tokens {
		tok, err := st.CreateAPIToken(ctx, types.APIToken{ID: uuid.New(), Principal: "auth0|" + uuid.NewString(), Role: "user",
			UserType: "standard", Name: "minted", CreatedAt: now, MintedBy: "admin"}, "wdn_"+uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		tokens[i] = tok.ID
	}
	for i, by := range []string{leaver, other} {
		if _, err := st.MoveAPIToken(ctx, tokens[i], "ci-bot", by, now); err != nil {
			t.Fatalf("move %d: %v", i, err)
		}
	}

	if _, _, err := st.EraseServiceIdentityAttributionOf(ctx, leaver); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"", other} {
		var got string
		if err := pool.QueryRow(ctx, `SELECT linked_by FROM api_token_moves WHERE token_id = $1`, tokens[i]).Scan(&got); err != nil || got != want {
			t.Errorf("move %d linked_by = %q (%v), want %q", i, got, err, want)
		}
	}
}
