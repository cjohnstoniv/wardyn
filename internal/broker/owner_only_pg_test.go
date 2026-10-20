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
	// Operator ownership is the signed claim, never the subject's spelling.
	impostor := &identity.Claims{RunID: uuid.New(), Sub: "admin-token"}
	if m, err := b.mintSSHKey(ctx, impostor, strict); !errors.Is(err, secretstore.ErrNotFound) || m.Token != "" {
		t.Fatalf("owner_only, sub \"admin-token\" without the claim: (%q, %v), want a not-found refusal", m.Token, err)
	}
	operator := &identity.Claims{RunID: uuid.New(), Sub: "admin-token", OperatorOwned: true}
	if m, err := b.mintSSHKey(ctx, operator, strict); err != nil || m.Token != "operator-key" || m.Metadata["secret_scope"] != "operator" {
		t.Fatalf("owner_only, operator-owned run: (%q, %v, %v), want the operator row, its own", m.Token, m.Metadata, err)
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

// TestPG_MintSSHKey_OwnerOnlyRefsDecidePerRef: owner_only_refs binds only the
// refs it names. With the owner's own key and only the operator's known_hosts,
// a grant binding the key alone mints both; binding the known_hosts instead
// refuses it; and the key it binds is never served from another row: not the
// operator's of that name, not another person's.
func TestPG_MintSSHKey_OwnerOnlyRefsDecidePerRef(t *testing.T) {
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
	key, kh := "ssh-key-"+uuid.NewString(), "known-hosts-"+uuid.NewString()
	alice, bob := "alice-"+uuid.NewString(), "bob-"+uuid.NewString()
	t.Cleanup(func() {
		_, _ = sec.DeleteEverywhere(ctx, []string{key, kh})
	})
	if err := sec.Put(ctx, kh, []byte("operator-kh")); err != nil {
		t.Fatal(err)
	}
	if err := sec.For(bob).Put(ctx, key, []byte("bob-key")); err != nil {
		t.Fatal(err)
	}
	b := New(NewPgxStore(pool), sec, &fakeAudit{}, nil, nil)
	caller := &identity.Claims{RunID: uuid.New(), Sub: alice}
	keyOnly := sshKeySpec("github.com", key, "", kh)
	keyOnly.OwnerOnlyRefs = []string{key}

	if m, err := b.mintSSHKey(ctx, caller, keyOnly); !errors.Is(err, secretstore.ErrNotFound) || m.Token != "" {
		t.Fatalf("bound key held only by another person: (%q, %v), want a not-found refusal", m.Token, err)
	}
	if err := sec.Put(ctx, key, []byte("operator-key")); err != nil {
		t.Fatal(err)
	}
	if m, err := b.mintSSHKey(ctx, caller, keyOnly); !errors.Is(err, secretstore.ErrNotFound) || m.Token != "" {
		t.Fatalf("bound key with only the operator's row: (%q, %v), want a not-found refusal and never the operator key", m.Token, err)
	}
	if err := sec.For(alice).Put(ctx, key, []byte("alice-key")); err != nil {
		t.Fatal(err)
	}
	if m, err := b.mintSSHKey(ctx, caller, keyOnly); err != nil || m.Token != "alice-key" || m.KnownHosts != "operator-kh" || m.Metadata["secret_scope"] != "own" {
		t.Fatalf("own key bound, operator known_hosts unbound: (%q, %q, %v, %v), want both", m.Token, m.KnownHosts, m.Metadata, err)
	}
	khOnly := sshKeySpec("github.com", key, "", kh)
	khOnly.OwnerOnlyRefs = []string{kh}
	if m, err := b.mintSSHKey(ctx, caller, khOnly); !errors.Is(err, secretstore.ErrNotFound) || m.KnownHosts != "" {
		t.Fatalf("known_hosts bound with only the operator's row: (%q, %v), want a not-found refusal", m.KnownHosts, err)
	}
}
