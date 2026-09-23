// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for user types (migration 0069_user_types). Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset. Every case mints unique ids and
// names and deletes what it created.
package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_UserTypes_SeededStandard(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx := context.Background()

	std, err := st.GetUserType(ctx, types.UserTypeStandard)
	if err != nil {
		t.Fatalf("get standard: %v", err)
	}
	if !std.BuiltIn || std.Priority != 0 || std.Name == "" {
		t.Fatalf("standard = %+v, want built in, priority 0, named", std)
	}
	list, err := st.ListUserTypes(ctx)
	if err != nil || len(list) == 0 || list[0].ID != types.UserTypeStandard {
		t.Fatalf("list = %+v (%v), want the built-in type first", list, err)
	}

	// Never deletable, by the store too — not only by the handler in front of it.
	if err := st.DeleteUserType(ctx, types.UserTypeStandard); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("delete standard = %v, want ErrConflict", err)
	}
	if _, err := st.GetUserType(ctx, types.UserTypeStandard); err != nil {
		t.Fatalf("standard gone after a refused delete: %v", err)
	}
	// The database refuses a priority on the tie floor even if a caller forgot.
	std.Priority = 5
	if _, err := st.UpdateUserType(ctx, std); err == nil {
		t.Fatal("standard accepted a priority; the CHECK (NOT built_in OR priority = 0) is missing")
	}
}

func TestPG_UserTypes_RoundTrip(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	id := "ut-" + suffix

	created, err := st.CreateUserType(ctx, types.UserType{ID: id, Name: "Type " + suffix, Priority: 7, CreatedBy: "sec@example.com"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = st.DeleteUserType(ctx, id) })
	if created.BuiltIn || created.CreatedAt.IsZero() || created.CreatedBy != "sec@example.com" {
		t.Fatalf("created = %+v", created)
	}

	if _, err := st.CreateUserType(ctx, types.UserType{ID: id, Name: "Other " + suffix}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("create taken id = %v, want ErrConflict", err)
	}
	if _, err := st.CreateUserType(ctx, types.UserType{ID: "ut2-" + suffix, Name: "Type " + suffix}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("create taken name = %v, want ErrConflict", err)
	}
	if _, err := st.CreateUserType(ctx, types.UserType{ID: "Bad_" + suffix, Name: "Bad " + suffix}); err == nil {
		t.Fatal("created an id outside the slug CHECK")
	}

	updated, err := st.UpdateUserType(ctx, types.UserType{ID: id, Name: "Renamed " + suffix, Description: "d", Priority: 9})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "Renamed "+suffix || updated.Priority != 9 || updated.CreatedBy != "sec@example.com" {
		t.Fatalf("updated = %+v, want the new fields and the original author", updated)
	}
	if _, err := st.UpdateUserType(ctx, types.UserType{ID: "nobody-" + suffix, Name: "x" + suffix}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("update missing = %v, want ErrNotFound", err)
	}

	// The reference count runs against the three subject tables' real schema.
	if n, err := st.UserTypeReferences(ctx, id); err != nil || n != 0 {
		t.Fatalf("references = %d, %v; want 0, nil", n, err)
	}
	if err := st.DeleteUserType(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.DeleteUserType(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete again = %v, want ErrNotFound", err)
	}
}

// TestPG_UserTypes_DeleteRefusedWhileATokenCarriesIt: a live API token stamped
// with a type keeps it from deletion, in the count the handler reports and in
// the DELETE's own predicate; once the token is revoked the type can go.
func TestPG_UserTypes_DeleteRefusedWhileATokenCarriesIt(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	id := "ut-tok-" + suffix
	if _, err := st.CreateUserType(ctx, types.UserType{ID: id, Name: "Token type " + suffix}); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = st.DeleteUserType(ctx, id) })

	tok, err := st.CreateAPIToken(ctx, types.APIToken{
		ID: uuid.New(), Principal: "holder-" + suffix, Role: "user", UserType: id, Name: "ci",
	}, "wdn_"+uuid.NewString())
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if tok.UserType != id {
		t.Fatalf("token user_type = %q, want %q read back", tok.UserType, id)
	}

	if n, err := st.UserTypeTokenStamps(ctx, id); err != nil || n != 1 {
		t.Fatalf("token stamps = %d, %v; want 1, nil", n, err)
	}
	if err := st.DeleteUserType(ctx, id); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("delete with a live token = %v, want ErrConflict", err)
	}

	if _, err := st.RevokeAPIToken(ctx, tok.ID, "", time.Now().UTC()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if n, err := st.UserTypeTokenStamps(ctx, id); err != nil || n != 0 {
		t.Fatalf("token stamps after revoke = %d, %v; want 0, nil", n, err)
	}
	if err := st.DeleteUserType(ctx, id); err != nil {
		t.Fatalf("delete after revoke: %v", err)
	}
}
