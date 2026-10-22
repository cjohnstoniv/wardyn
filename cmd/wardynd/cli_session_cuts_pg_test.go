// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Every session cut ends the person's CLI sessions and their tokens in the cut's own transaction, on the
// real writer: CutSessions (Sign out everywhere, the console's Sign out, back-channel logout, a SCIM group
// cut), RevokeSub (a person revoke, SCIM suspension) and RevokeAll. The next request with the access
// token is refused at once, before any refresh. Another person's CLI session is untouched.
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.
func TestPGSessionRevocations_CutsEndCLISessions(t *testing.T) {
	// A database of its own: RevokeAll writes the global row, which would revoke every other test's
	// sessions on a shared one.
	empty := throwawayDB(t)
	ctx := t.Context()
	pool, err := connectAndMigrate(ctx, empty.Config().ConnString(), "", 30*time.Second, 60*time.Second, false)
	if err != nil {
		t.Fatalf("connectAndMigrate: %v", err)
	}
	defer pool.Close()
	rev := &pgSessionRevocations{pool: pool}
	st := store.NewPG(pool)
	seed := func(principal, email string) (uuid.UUID, string) {
		t.Helper()
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		now := time.Now().UTC()
		id := uuid.New()
		exp := now.Add(time.Hour)
		raw := "wdn_" + uuid.NewString()
		if _, err := st.CreateCLISession(context.Background(), store.CLISessionRow{ID: id, Identity: store.CLIIdentity{Principal: principal, Email: email, Role: "user",
			UserType: types.UserTypeStandard, RevSeq: 1}, IdentityStampedAt: now, Machine: "laptop", Store: types.CLIStoreKeychain, PublicKey: pub,
			CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}, "wdc_"+uuid.NewString(),
			types.APIToken{ID: uuid.New(), Principal: principal, Email: email, Role: "user", UserType: types.UserTypeStandard, Name: "CLI: laptop",
				CreatedAt: now, ExpiresAt: &exp, Kind: types.APITokenKindCLIAccess, CLISessionID: &id}, raw); err != nil {
			t.Fatal(err)
		}
		return id, raw
	}
	run := fmt.Sprintf("%d", time.Now().UnixNano())
	for _, c := range []struct {
		name, reason string
		cut          func(sub, email string) error
		byEmail      bool
	}{
		{"CutSessions", types.CLISessionEndSessionsCut, func(sub, _ string) error { return rev.CutSessions(ctx, sub) }, false},
		{"CutSessions by email", types.CLISessionEndSessionsCut, func(_, email string) error { return rev.CutSessions(ctx, email) }, true},
		{"RevokeSub", types.CLISessionEndRevoked, func(sub, _ string) error { return rev.RevokeSub(ctx, sub) }, false},
		{"RevokeAll", types.CLISessionEndRevoked, func(string, string) error { return rev.RevokeAll(ctx) }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			sub, email := "sub-cli-"+run+"-"+uuid.NewString()[:8], "Person-"+uuid.NewString()[:8]+"@corp.example"
			id, raw := seed(sub, email)
			if _, err := st.GetAPITokenByRaw(ctx, raw); err != nil {
				t.Fatalf("access token before the cut: %v", err)
			}
			otherID, otherRaw := seed("sub-other-"+uuid.NewString()[:8], "other@corp.example")
			if err := c.cut(sub, email); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			got, _ := st.GetCLISession(ctx, id)
			if got.EndedAt == nil || got.EndReason != c.reason {
				t.Fatalf("CLI session after %s = %+v, want ended as %s", c.name, got, c.reason)
			}
			if _, err := st.GetAPITokenByRaw(ctx, raw); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("access token after %s = %v, want refused at once", c.name, err)
			}
			var live int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM api_tokens WHERE cli_session_id = $1 AND revoked_at IS NULL`, id).Scan(&live); err != nil || live != 0 {
				t.Errorf("%d unrevoked token rows (%v), want 0", live, err)
			}
			other, _ := st.GetCLISession(ctx, otherID)
			if c.name == "RevokeAll" {
				if other.EndedAt == nil {
					t.Errorf("RevokeAll left another person's CLI session live")
				}
				return
			}
			if other.EndedAt != nil {
				t.Errorf("%s ended another person's CLI session: %+v", c.name, other)
			}
			if _, err := st.GetAPITokenByRaw(ctx, otherRaw); err != nil {
				t.Errorf("another person's access token after %s: %v", c.name, err)
			}
		})
	}
}
