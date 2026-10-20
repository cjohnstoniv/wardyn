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

// The token call's write after a sessions-only cut its late check did not see: the cut's sweep ran
// before the new row existed, so CreateCLISession has to read the cut against the approval's own
// sequence (the issue the session records). The session from before the cut is the control the sweep
// reaches; a personal API token written after the same cut keeps the sessions-only exemption.
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.
func TestPG_CLISession_CreateAfterUnseenSessionsCut(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	const sub, email = "sub-cut", "cut@corp.example"
	old, _, oldRaw := seedCLISession(t, st, sub, email)
	approved, err := store.ReadRevocationSeq(ctx, pool) // the approval's issue (DecideCLISignIn's rev_seq)
	if err != nil {
		t.Fatal(err)
	}
	// CutSessions's write, committed after the token call's late check answered "not cut".
	if err := store.ExecRevocationCut(ctx, pool, `INSERT INTO oidc_session_cuts (sub, cut_at) VALUES ($1, clock_timestamp())
		ON CONFLICT (sub) DO UPDATE SET cut_at = EXCLUDED.cut_at`, sub, types.CLISessionEndSessionsCut); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetCLISession(ctx, old.ID); got.EndedAt == nil {
		t.Errorf("the session from before the cut = %+v, want ended", got)
	}
	if _, err := st.GetAPITokenByRaw(ctx, oldRaw); err == nil {
		t.Errorf("the access token of the session from before the cut still authenticates")
	}

	// The token call's write, guarded as createCLISession guards it (guardOwner with the approval's epoch).
	guarded := store.WithIdentityGuard(ctx, store.IdentityGuard{Principal: sub, Epoch: 0})
	now := time.Now().UTC()
	sess := store.CLISessionRow{ID: uuid.New(), Identity: store.CLIIdentity{Principal: sub, Email: email, Role: "user",
		UserType: types.UserTypeStandard, RevSeq: approved}, IdentityStampedAt: now, Machine: "laptop", Store: types.CLIStoreKeychain,
		PublicKey: cliTestKey(t), CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	exp := now.Add(time.Hour)
	raw := "wdn_" + uuid.NewString()
	if _, err := st.CreateCLISession(guarded, sess, "wdc_"+uuid.NewString(), types.APIToken{ID: uuid.New(), Principal: sub, Email: email,
		Role: "user", UserType: types.UserTypeStandard, Name: "CLI: laptop", CreatedAt: now, ExpiresAt: &exp, RevSeq: approved,
		Kind: types.APITokenKindCLIAccess, CLISessionID: &sess.ID}, raw); err == nil {
		if got, _ := st.GetCLISession(ctx, sess.ID); got.EndedAt == nil {
			t.Errorf("CreateCLISession after an unseen sessions-only cut wrote a live session: %+v", got)
		}
		if _, err := st.GetAPITokenByRaw(ctx, raw); err == nil {
			t.Errorf("CreateCLISession after an unseen sessions-only cut wrote an access token that passes apiTokenLive")
		}
	}

	personal := "wdn_" + uuid.NewString()
	if _, err := st.CreateAPIToken(store.WithIdentityGuard(ctx, store.IdentityGuard{Principal: sub, Epoch: -1}), types.APIToken{ID: uuid.New(),
		Principal: sub, Email: email, Role: "user", UserType: types.UserTypeStandard, Name: "ci", CreatedAt: now, RevSeq: approved}, personal); err != nil {
		t.Errorf("a personal API token after a sessions-only cut = %v, want written (the exemption)", err)
	}
	if _, err := st.GetAPITokenByRaw(ctx, personal); err != nil {
		t.Errorf("the personal API token after a sessions-only cut = %v, want it to authenticate", err)
	}
}
