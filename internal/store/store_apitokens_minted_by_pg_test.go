// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_APITokens_RefreshIdentityMintedByArm pins the sign-in arm of
// RefreshAPITokenIdentity for a token an admin created for its owner (#1477).
// The route that made them is gone; the rows it made keep working, so the
// store seeds them directly. A sign-in under the SAME role keeps such a token
// working and re-stamps it with the person's groups (disclosed in the upgrade
// note); a sign-in under a DIFFERENT role revokes it instead of raising it;
// an owner-minted token (no minted_by) is re-stamped whatever the role does.
func TestPG_APITokens_RefreshIdentityMintedByArm(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	person := "legacy-" + uuid.NewString()
	seed := func(mintedBy string) (types.APIToken, string) {
		t.Helper()
		unknown := true
		raw := "wdn_" + uuid.NewString()
		row, err := st.CreateAPIToken(ctx, types.APIToken{
			ID: uuid.New(), Principal: person, Role: "user", UserType: "standard",
			GroupsTruncated: &unknown, Name: "ci", CreatedAt: time.Now().UTC(), MintedBy: mintedBy,
		}, raw)
		if err != nil {
			t.Fatal(err)
		}
		return row, raw
	}
	legacy, legacyRaw := seed("admin-1")
	own, ownRaw := seed("")
	live := func(raw string) (types.APIToken, bool) {
		t.Helper()
		got, err := st.GetAPITokenByRaw(ctx, raw)
		if errors.Is(err, store.ErrNotFound) {
			return types.APIToken{}, false
		}
		if err != nil {
			t.Fatal(err)
		}
		return got, true
	}

	// Same role: both keep authenticating as the person, groups now known.
	if err := st.RefreshAPITokenIdentity(ctx, person, "user", "standard", []string{"eng"}, false); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{"legacy": legacyRaw, "owner-minted": ownRaw} {
		got, ok := live(raw)
		if !ok || got.Principal != person || len(got.Groups) != 1 || got.Groups[0] != "eng" ||
			got.GroupsTruncated == nil || *got.GroupsTruncated {
			t.Fatalf("%s token after a same-role sign-in = %+v (live %v), want the person with their groups re-stamped", name, got, ok)
		}
	}
	if got, _ := live(legacyRaw); got.MintedBy != "admin-1" || got.ID != legacy.ID {
		t.Errorf("legacy token lost its minted_by: %+v", got)
	}

	// A different role: the legacy token is revoked, the owner-minted one follows.
	if err := st.RefreshAPITokenIdentity(ctx, person, "admin", "standard", []string{"eng"}, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := live(legacyRaw); ok {
		t.Error("legacy token survived a role-changing sign-in, want it revoked")
	}
	if got, ok := live(ownRaw); !ok || got.Role != "admin" || got.ID != own.ID {
		t.Errorf("owner-minted token after a role change = %+v (live %v), want re-stamped admin", got, ok)
	}
}

// TestPG_APITokens_ListMintedForOthersMatchesTheGoFilter pins the SQL inventory to the filter it
// replaced: over live, revoked, expired, owner-minted and admin-minted rows, with and without the
// finished ones, it keeps exactly the rows the Go predicate over ListAPITokens keeps, newest first.
func TestPG_APITokens_ListMintedForOthersMatchesTheGoFilter(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	now := time.Now().UTC()
	person := "inventory-" + uuid.NewString()
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	n := 0
	seed := func(mintedBy string, expires *time.Time, revoke bool) {
		t.Helper()
		n++
		unknown := true
		row, err := st.CreateAPIToken(ctx, types.APIToken{
			ID: uuid.New(), Principal: person, Role: "user", UserType: "standard", GroupsTruncated: &unknown,
			Name: "ci", CreatedAt: now.Add(-time.Duration(n) * time.Minute), MintedBy: mintedBy, ExpiresAt: expires,
		}, "wdn_"+uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		if revoke {
			if _, err := st.RevokeAPIToken(ctx, row.ID, person, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed("", nil, false)
	seed(person, nil, false)
	seed("admin-1", nil, false)
	seed("admin-1", &future, false)
	seed("admin-1", &past, false)
	seed("admin-1", nil, true)
	seed("admin-2", &future, true)
	seed(person, nil, true)

	all, err := st.ListAPITokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, withFinished := range []bool{false, true} {
		var want []uuid.UUID
		for _, tok := range all {
			finished := tok.RevokedAt != nil || (tok.ExpiresAt != nil && !tok.ExpiresAt.After(now))
			if tok.Principal == person && tok.MintedBy != "" && tok.MintedBy != tok.Principal && (!finished || withFinished) {
				want = append(want, tok.ID)
			}
		}
		got, err := st.ListAPITokensMintedForOthers(ctx, withFinished, now)
		if err != nil {
			t.Fatal(err)
		}
		var ids []uuid.UUID
		for _, tok := range got {
			if tok.Principal == person {
				ids = append(ids, tok.ID)
			}
		}
		if wantN := map[bool]int{false: 2, true: 5}[withFinished]; len(want) != wantN || !slices.Equal(ids, want) {
			t.Errorf("withFinished=%v: SQL kept %v, Go filter kept %v (want %d rows)", withFinished, ids, want, wantN)
		}
	}
}
