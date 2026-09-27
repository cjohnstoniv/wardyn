// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"errors"
	"testing"

	"filippo.io/age"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
)

// TestPG_MintSSHKey_OwnerOnlyNeverServesTheOperatorRow pins #1106 on the
// ssh_key mint against the real pg store: with operator rows for the key and
// its known_hosts and none of the owner's, an owner_only grant is refused, the
// same grant unflagged keeps the fallback, and with the owner's own key (but
// still only the operator's known_hosts) the known_hosts read is refused too.
func TestPG_MintSSHKey_OwnerOnlyNeverServesTheOperatorRow(t *testing.T) {
	pool := pgPool(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	sec, err := secretspg.New(pool, id)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key, kh, sub := "ssh-key-"+uuid.NewString(), "known-hosts-"+uuid.NewString(), "alice-"+uuid.NewString()
	t.Cleanup(func() {
		_, _ = sec.DeleteEverywhere(ctx, []string{key, kh})
	})
	for name, v := range map[string]string{key: "operator-key", kh: "operator-kh"} {
		if err := sec.Put(ctx, name, []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	b := New(NewPgxStore(pool), sec, &fakeAudit{}, nil, nil)
	caller := &identity.Claims{RunID: uuid.New(), Sub: sub}

	strict := sshKeySpec("github.com", key, "", "")
	strict.OwnerOnly = true
	if m, err := b.mintSSHKey(ctx, caller, strict); !errors.Is(err, secretstore.ErrNotFound) || m.Token != "" {
		t.Fatalf("owner_only, no own row: (%q, %v), want a not-found refusal and never the operator key", m.Token, err)
	}
	if m, err := b.mintSSHKey(ctx, caller, sshKeySpec("github.com", key, "", "")); err != nil ||
		m.Token != "operator-key" || m.Metadata["secret_scope"] != "operator" {
		t.Fatalf("unflagged: (%q, %v, %v), want today's operator fallback", m.Token, m.Metadata, err)
	}

	if err := sec.For(sub).Put(ctx, key, []byte("alice-key")); err != nil {
		t.Fatal(err)
	}
	if m, err := b.mintSSHKey(ctx, caller, strict); err != nil || m.Token != "alice-key" || m.Metadata["secret_scope"] != "own" {
		t.Fatalf("owner_only, own key: (%q, %v, %v), want the owner's key", m.Token, m.Metadata, err)
	}
	withKH := sshKeySpec("github.com", key, "", kh)
	withKH.OwnerOnly = true
	if m, err := b.mintSSHKey(ctx, caller, withKH); !errors.Is(err, secretstore.ErrNotFound) || m.KnownHosts != "" {
		t.Fatalf("owner_only, operator-only known_hosts: (%q, %v), want a not-found refusal", m.KnownHosts, err)
	}
}
