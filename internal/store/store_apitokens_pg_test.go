// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for the per-user API token store (migration 0045).
// Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
// Run with: WARDYN_TEST_PG=postgres://... go test ./internal/store/...
//
// These exist because the in-memory double in internal/api cannot catch a SQL
// typo, and three of this table's properties live ENTIRELY in SQL: the
// revoked_at filter that makes a revoked token indistinguishable from an unknown
// one, the empty-principal-means-any scoping that separates the self-service
// revoke from the admin one, and the JSONB round trip that has to preserve nil
// versus empty groups. They also pin the one property no round trip can see:
// what the stored bytes ACTUALLY are (hash at rest, below).
package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func seedToken(t *testing.T, st store.PG, principal, raw string, groups []string) types.APIToken {
	t.Helper()
	created, err := st.CreateAPIToken(context.Background(), types.APIToken{
		ID:        uuid.New(),
		Principal: principal,
		Email:     principal + "@example.com",
		Role:      "member",
		Groups:    groups,
		Name:      "ci",
		CreatedAt: time.Now().UTC(),
	}, raw)
	if err != nil {
		t.Fatalf("create api token: %v", err)
	}
	return created
}

func TestPG_APITokens_LookupTouchRevoke(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	raw := "wdn_" + uuid.NewString()
	created := seedToken(t, st, "alice-"+uuid.NewString(), raw, []string{"eng", "oncall"})
	if created.LastUsedAt != nil || created.RevokedAt != nil {
		t.Errorf("created = %+v, want last_used_at and revoked_at NULL", created)
	}

	// HASH AT REST. Read the column back raw, because a Create/Get round trip
	// stays green even if hashToken (store_ephemeral.go) becomes the identity
	// function and the table starts holding usable bearer credentials. This is
	// the only assertion that looks at the stored bytes, and it pins the helper
	// BOTH credential tables share (attach_tickets.token_sha256 too).
	var atRest string
	if err := pool.QueryRow(ctx, `SELECT token_sha256 FROM api_tokens WHERE id = $1`, created.ID).Scan(&atRest); err != nil {
		t.Fatalf("read token_sha256: %v", err)
	}
	if sum := sha256.Sum256([]byte(raw)); atRest != hex.EncodeToString(sum[:]) {
		t.Errorf("token_sha256 = %q, want hex(sha256(raw)) — the raw token must never be at rest", atRest)
	}

	// The auth-time lookup takes the PLAINTEXT and hashes internally; the raw
	// value is never what is stored.
	got, err := st.GetAPITokenByRaw(ctx, raw)
	if err != nil {
		t.Fatalf("lookup by raw: %v", err)
	}
	if got.ID != created.ID || got.Email != created.Email || got.Role != "member" {
		t.Errorf("lookup = %+v, want the seeded row", got)
	}
	// The identity snapshot has to survive the JSONB round trip — a group grant
	// (including a DENY) that silently stops matching for token calls is a hole,
	// not a degradation.
	if len(got.Groups) != 2 || got.Groups[0] != "eng" || got.Groups[1] != "oncall" {
		t.Errorf("groups = %v, want [eng oncall] preserved in order", got.Groups)
	}
	if _, err := st.GetAPITokenByRaw(ctx, raw+"x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("lookup of a non-matching plaintext: err = %v, want ErrNotFound", err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := st.TouchAPIToken(ctx, created.ID, now); err != nil {
		t.Fatalf("touch: %v", err)
	}
	touched, err := st.GetAPITokenByRaw(ctx, raw)
	if err != nil {
		t.Fatalf("lookup after touch: %v", err)
	}
	if touched.LastUsedAt == nil || !touched.LastUsedAt.Equal(now) {
		t.Errorf("last_used_at = %v, want %v", touched.LastUsedAt, now)
	}

	revoked, err := st.RevokeAPIToken(ctx, created.ID, created.Principal, now)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if revoked.RevokedAt == nil {
		t.Fatalf("revoked = %+v, want revoked_at set", revoked)
	}
	// A revoked token authenticates nothing, and its refusal is the SAME
	// ErrNotFound an unknown token gets — no oracle for "this used to exist".
	if _, err := st.GetAPITokenByRaw(ctx, raw); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("lookup of a revoked token: err = %v, want ErrNotFound", err)
	}
	// Revoke is not repeatable: the second call changes nothing and reports
	// ErrNotFound, so the API layer emits no second audit row.
	if _, err := st.RevokeAPIToken(ctx, created.ID, created.Principal, now); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second revoke: err = %v, want ErrNotFound", err)
	}
	// The row SURVIVES revocation (soft delete) and stays visible to its owner.
	list, err := st.ListAPITokensByPrincipal(ctx, created.Principal)
	if err != nil {
		t.Fatalf("list by principal: %v", err)
	}
	if len(list) != 1 || list[0].RevokedAt == nil {
		t.Errorf("list = %+v, want the revoked row still present and marked", list)
	}
}

// TestPG_APITokens_RevokeScoping pins the one predicate that separates the two
// revoke routes: a non-empty principal may only ever reach that human's own
// rows, and an EMPTY principal (the admin lane) reaches anyone's.
func TestPG_APITokens_RevokeScoping(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	owner := "alice-" + uuid.NewString()
	raw := "wdn_" + uuid.NewString()
	created := seedToken(t, st, owner, raw, nil)
	now := time.Now().UTC()

	// Another human's revoke of it is ErrNotFound, not a distinguishable
	// refusal — no existence leak across principals.
	if _, err := st.RevokeAPIToken(ctx, created.ID, "mallory-"+uuid.NewString(), now); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("foreign revoke: err = %v, want ErrNotFound", err)
	}
	if _, err := st.GetAPITokenByRaw(ctx, raw); err != nil {
		t.Fatalf("the foreign revoke must not have taken effect: %v", err)
	}
	// The admin lane (empty principal) revokes it.
	if _, err := st.RevokeAPIToken(ctx, created.ID, "", now); err != nil {
		t.Fatalf("admin revoke-any: %v", err)
	}
	if _, err := st.GetAPITokenByRaw(ctx, raw); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("after admin revoke: err = %v, want ErrNotFound", err)
	}
}

// TestPG_APITokens_NilGroupsStayNull pins nil-vs-empty across the JSONB column.
// NULL means "the creating session had no answerable group identity" (the
// resolver must report snapshot-unavailable); '[]' means "the IdP sent none".
// Collapsing them would silently withhold every group grant a holder has.
func TestPG_APITokens_NilGroupsStayNull(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	nilRaw := "wdn_" + uuid.NewString()
	seedToken(t, st, "nil-"+uuid.NewString(), nilRaw, nil)
	emptyRaw := "wdn_" + uuid.NewString()
	seedToken(t, st, "empty-"+uuid.NewString(), emptyRaw, []string{})

	gotNil, err := st.GetAPITokenByRaw(ctx, nilRaw)
	if err != nil {
		t.Fatalf("lookup nil-groups token: %v", err)
	}
	if gotNil.Groups != nil {
		t.Errorf("groups = %v, want nil (snapshot unavailable)", gotNil.Groups)
	}
	gotEmpty, err := st.GetAPITokenByRaw(ctx, emptyRaw)
	if err != nil {
		t.Fatalf("lookup empty-groups token: %v", err)
	}
	if gotEmpty.Groups == nil || len(gotEmpty.Groups) != 0 {
		t.Errorf("groups = %v, want a non-nil empty slice (the IdP sent none)", gotEmpty.Groups)
	}
}

// TestPG_APITokens_RefreshRolesAtLogin pins the bound the token lane did not
// have. The role is stamped at mint and only two statements ever touched this
// table — last_used_at and revoked_at — so demoting a human from admin left
// every outstanding wdn_ token of theirs authenticating AS AN ADMIN until
// someone separately remembered to revoke it, and 0.7 widened that stamp to
// carry security_admin. The sibling credential got exactly this bound in
// migration 0046 (RefreshSSHKeyRoles, fired from the same OnLogin hook); this
// is its twin.
//
// Four properties, each a way the UPDATE could be wrong:
//   - it re-stamps EVERY token the principal holds, not just one;
//   - it does not touch anyone else's;
//   - it leaves a REVOKED token alone (revoking is terminal — re-stamping a
//     revoked row would resurrect nothing but would make the trail lie about
//     what that credential was);
//   - a principal with no tokens is not an error, which is the ordinary case
//     for most humans and would otherwise fail every login.
func TestPG_APITokens_RefreshRolesAtLogin(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	alice := "alice-" + uuid.NewString()
	bob := "bob-" + uuid.NewString()
	a1 := seedToken(t, st, alice, "wdn_"+uuid.NewString(), []string{"eng"})
	a2 := seedToken(t, st, alice, "wdn_"+uuid.NewString(), []string{"eng"})
	gone := seedToken(t, st, alice, "wdn_"+uuid.NewString(), nil)
	b1 := seedToken(t, st, bob, "wdn_"+uuid.NewString(), nil)

	// Alice is promoted, so her live tokens must follow at her next login.
	if err := st.RefreshAPITokenRoles(ctx, alice, "admin"); err != nil {
		t.Fatalf("RefreshAPITokenRoles: %v", err)
	}
	roleOf := func(id uuid.UUID) string {
		t.Helper()
		var role string
		if err := pool.QueryRow(ctx, `SELECT role FROM api_tokens WHERE id = $1`, id).Scan(&role); err != nil {
			t.Fatalf("read role: %v", err)
		}
		return role
	}
	if got := roleOf(a1.ID); got != "admin" {
		t.Errorf("a1 role = %q, want admin — the login hook did not reach every token", got)
	}
	if got := roleOf(a2.ID); got != "admin" {
		t.Errorf("a2 role = %q, want admin — the refresh stopped at the first row", got)
	}
	if got := roleOf(b1.ID); got != "member" {
		t.Errorf("bob's role = %q, want member — one human's login re-stamped ANOTHER human's token", got)
	}

	// And the demotion direction, which is the one the finding is about.
	if _, err := st.RevokeAPIToken(ctx, gone.ID, "", time.Now().UTC()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := st.RefreshAPITokenRoles(ctx, alice, "member"); err != nil {
		t.Fatalf("RefreshAPITokenRoles (demote): %v", err)
	}
	if got := roleOf(a1.ID); got != "member" {
		t.Errorf("a1 role = %q after demotion, want member — a demoted human kept admin on an outstanding token", got)
	}
	// gone was promoted with the rest while it was still live, THEN revoked,
	// THEN the demote ran. So it must still read admin: the
	// `WHERE revoked_at IS NULL` clause skipped it, and a revoked credential
	// keeps the stamp it carried at revocation rather than being rewritten
	// afterwards by an event it was no longer party to.
	if got := roleOf(gone.ID); got != "admin" {
		t.Errorf("revoked token role = %q, want the admin it held when it was revoked — the demote rewrote a "+
			"revoked row, so the trail no longer says what that credential actually was", got)
	}

	// A principal with no tokens at all: every login of every human without a
	// token takes this path.
	if err := st.RefreshAPITokenRoles(ctx, "nobody-"+uuid.NewString(), "admin"); err != nil {
		t.Errorf("refresh for a principal with no tokens = %v, want nil — this fires on EVERY login", err)
	}
}
