// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

// The local-mode platform-key split (design §2.13 c) against a real Postgres.
// Each test takes its own throwaway database (rekeyDatabase): the boot keys are
// fixed operator-namespace names, and Rewrap rewrites every row.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
)

const signingKey = "wardyn-signing-key"

// putUnder writes a v1 row for (owner, name) sealed under k exactly as a
// store holding k would: what a pre-split wardynd wrote, or what anyone
// holding k can forge with write access to the table.
func putUnder(t *testing.T, pool *pgxpool.Pool, k kek.KEK, owner, name, value string) {
	t.Helper()
	ctx := context.Background()
	wrapped, ct, err := seal(ctx, k, owner, name, []byte(value))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO secrets (owned_by, name, enc_version, kek_id, wrapped_dek, ciphertext) VALUES ($1, $2, 1, $3, $4, $5)
		ON CONFLICT (owned_by, name) DO UPDATE SET enc_version=1, kek_id=$3, wrapped_dek=$4, ciphertext=$5`,
		owner, name, k.ID(), wrapped, ct); err != nil {
		t.Fatal(err)
	}
}

func splitStore(t *testing.T, pool *pgxpool.Pool, id, platform *age.X25519Identity) *Store {
	t.Helper()
	s := &Store{pool: pool}
	var p age.Identity
	if platform != nil {
		p = platform
	}
	if err := s.setLocalKeys(id, p); err != nil {
		t.Fatal(err)
	}
	return s
}

func kekIDOf(t *testing.T, pool *pgxpool.Pool, owner, name string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `SELECT kek_id FROM secrets WHERE owned_by=$1 AND name=$2`, owner, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestPlatformSplit_EachPurposeWritesUnderItsOwnKEK: a boot key is wrapped
// under the platform KEK, every other row (an operator credential of any other
// name, a person's row even when it borrows a boot key's name) under the
// credential KEK.
func TestPlatformSplit_EachPurposeWritesUnderItsOwnKEK(t *testing.T) {
	pool := rekeyDatabase(t)
	ctx := context.Background()
	id, platform := mustIdentity(t), mustIdentity(t)
	for _, p := range []*age.X25519Identity{nil, platform} {
		s := splitStore(t, pool, id, p)
		for _, w := range []struct{ owner, name, want string }{
			{"", signingKey, s.platform.ID()},
			{"", "github-app-key", s.kek.ID()},
			{"alice", signingKey, s.kek.ID()},
		} {
			if err := s.For(w.owner).Put(ctx, w.name, []byte("v")); err != nil {
				t.Fatal(err)
			}
			if got := kekIDOf(t, pool, w.owner, w.name); got != w.want {
				t.Errorf("platform key %v: %s kek_id = %q, want %q", p != nil, rowRef(w.owner, w.name), got, w.want)
			}
		}
	}
	shared, separate := splitStore(t, pool, id, nil), splitStore(t, pool, id, platform)
	if !strings.HasPrefix(shared.platform.ID(), "local/platform:") || !strings.HasPrefix(shared.kek.ID(), "local/cred:") {
		t.Errorf("kek_ids %q / %q do not name their purpose", shared.platform.ID(), shared.kek.ID())
	}
	if shared.platform.ID() == separate.platform.ID() || shared.kek.ID() != separate.kek.ID() {
		t.Error("the platform key file must change the platform KEK and only it")
	}
}

// TestPlatformSplit_StolenCredentialKeyForgesNoBootKey is the property the
// split exists for: with WARDYN_PLATFORM_KEY_FILE set, someone holding the age
// key and write access to the table cannot plant a signing key. Every KEK the
// age key derives — the pre-split one, the credential one, and its own
// platform one — seals a well-formed row that is refused, never read as
// missing (so boot fails closed instead of minting over it). Without the
// platform key the pre-split row still opens: that is the residual the setup
// check names, and the control that shows the refusal is the split's doing.
func TestPlatformSplit_StolenCredentialKeyForgesNoBootKey(t *testing.T) {
	pool := rekeyDatabase(t)
	ctx := context.Background()
	id, platform := mustIdentity(t), mustIdentity(t)
	legacy, _ := kek.NewLocal(id)
	cred, _ := kek.NewLocalPurpose(id, kek.PurposeCred)
	agePlatform, _ := kek.NewLocalPurpose(id, kek.PurposePlatform)
	const forged = "forged-signing-key"

	separate := splitStore(t, pool, id, platform)
	for label, k := range map[string]kek.KEK{"pre-split KEK": legacy, "credential KEK": cred, "age key's platform KEK": agePlatform} {
		putUnder(t, pool, k, "", signingKey, forged)
		got, err := separate.Get(ctx, signingKey)
		assertRefused(t, label, got, err, "", signingKey, forged)
		// A member's view falls back to the operator's row: refused there too.
		got, err = separate.For("alice").Get(ctx, signingKey)
		assertRefused(t, label+", read by fallback", got, err, "", signingKey, forged)
		if err != nil && !strings.Contains(err.Error(), "wardynd -rewrap") {
			t.Errorf("%s: refusal %q does not name the way out", label, err)
		}
	}

	putUnder(t, pool, legacy, "", signingKey, "pre-split")
	if got, err := splitStore(t, pool, id, nil).Get(ctx, signingKey); err != nil || string(got) != "pre-split" {
		t.Fatalf("without a platform key a pre-split boot key must still open: (%q, %v)", got, err)
	}
	// The credential rows the age key protects are unaffected by the split.
	putUnder(t, pool, legacy, "", "github-app-key", "cred")
	if got, err := separate.Get(ctx, "github-app-key"); err != nil || string(got) != "cred" {
		t.Fatalf("a pre-split credential row under the split = (%q, %v), want it read", got, err)
	}
}

// TestRewrap_MovesEveryRowOntoItsPurposeKey is the upgrade path: rows a
// pre-split wardynd wrote, and the boot keys under the age key, move onto the
// KEK their purpose writes with — the payload untouched — and a second run
// moves nothing.
func TestRewrap_MovesEveryRowOntoItsPurposeKey(t *testing.T) {
	pool := rekeyDatabase(t)
	ctx := context.Background()
	id, platform := mustIdentity(t), mustIdentity(t)
	legacy, _ := kek.NewLocal(id)
	putUnder(t, pool, legacy, "", signingKey, "sign")
	putUnder(t, pool, legacy, "", "github-app-key", "app")
	putUnder(t, pool, legacy, "alice", "anthropic-api-key", "alice")
	if err := splitStore(t, pool, id, nil).Put(ctx, "wardyn-session-key", []byte("sess")); err != nil {
		t.Fatal(err)
	}
	before := rawRows(t, pool)

	n, err := Rewrap(ctx, pool, id, platform)
	if err != nil || n != 4 {
		t.Fatalf("Rewrap = (%d, %v), want 4 rows", n, err)
	}
	s := splitStore(t, pool, id, platform)
	for ref, r := range rawRows(t, pool) {
		was := before[ref]
		if !bytes.Equal(r.ct, was.ct) || bytes.Equal(r.wrapped, was.wrapped) {
			t.Errorf("%s: a rewrap must rewrap the data key and leave the sealed value alone", ref)
		}
	}
	for _, c := range []struct{ owner, name, value, kekID string }{
		{"", signingKey, "sign", s.platform.ID()},
		{"", "wardyn-session-key", "sess", s.platform.ID()},
		{"", "github-app-key", "app", s.kek.ID()},
		{"alice", "anthropic-api-key", "alice", s.kek.ID()},
	} {
		if got := kekIDOf(t, pool, c.owner, c.name); got != c.kekID {
			t.Errorf("%s kek_id = %q, want %q", rowRef(c.owner, c.name), got, c.kekID)
		}
		if got, err := s.For(c.owner).Get(ctx, c.name); err != nil || string(got) != c.value {
			t.Errorf("%s after the rewrap = (%q, %v)", rowRef(c.owner, c.name), got, err)
		}
	}
	if _, err := splitStore(t, pool, id, nil).Get(ctx, signingKey); err == nil {
		t.Error("a wardynd without the platform key still opens a boot key moved onto it")
	}
	if n, err := Rewrap(ctx, pool, id, platform); err != nil || n != 0 {
		t.Errorf("second Rewrap = (%d, %v), want (0, nil)", n, err)
	}
}

// TestRewrap_AbortsOnARowUnderAnotherKey: a row no key of this configuration
// wrapped aborts the whole rewrap, naming it, and commits nothing.
func TestRewrap_AbortsOnARowUnderAnotherKey(t *testing.T) {
	pool := rekeyDatabase(t)
	ctx := context.Background()
	id := mustIdentity(t)
	stray, _ := kek.NewLocal(mustIdentity(t))
	legacy, _ := kek.NewLocal(id)
	putUnder(t, pool, legacy, "", "github-app-key", "app")
	putUnder(t, pool, stray, "", "zz-stray", "stray")
	before := rawRows(t, pool)

	n, err := Rewrap(ctx, pool, id, mustIdentity(t))
	if err == nil || n != 0 || !strings.Contains(err.Error(), rowRef("", "zz-stray")) || !strings.Contains(err.Error(), "nothing committed") {
		t.Fatalf("Rewrap over a stray row = (%d, %v), want an abort naming it", n, err)
	}
	for ref, r := range rawRows(t, pool) {
		if r.kekID != before[ref].kekID || !bytes.Equal(r.wrapped, before[ref].wrapped) {
			t.Errorf("%s changed although the rewrap aborted", ref)
		}
	}
}

// TestRekey_LeavesTheSeparatePlatformKeyAlone: rotating the age key moves the
// credential rows and leaves the boot keys under the platform key as they are.
func TestRekey_LeavesTheSeparatePlatformKeyAlone(t *testing.T) {
	pool := rekeyDatabase(t)
	ctx := context.Background()
	oldID, newID, platform := mustIdentity(t), mustIdentity(t), mustIdentity(t)
	old := splitStore(t, pool, oldID, platform)
	for name, v := range map[string]string{signingKey: "sign", "github-app-key": "app"} {
		if err := old.Put(ctx, name, []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	signWas := kekIDOf(t, pool, "", signingKey)

	n, err := Rekey(ctx, pool, oldID, newID, platform)
	if err != nil || n != 1 {
		t.Fatalf("Rekey = (%d, %v), want only the credential row", n, err)
	}
	if got := kekIDOf(t, pool, "", signingKey); got != signWas {
		t.Errorf("the boot key moved from %q to %q", signWas, got)
	}
	s := splitStore(t, pool, newID, platform)
	for name, v := range map[string]string{signingKey: "sign", "github-app-key": "app"} {
		if got, err := s.Get(ctx, name); err != nil || string(got) != v {
			t.Errorf("%s after the rotation = (%q, %v)", name, got, err)
		}
	}
}
