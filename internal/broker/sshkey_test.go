// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sshKeySpec builds an auto-mint ssh_key grant spec. Reuses memSecrets/newFakeDB/
// seedGrant/callerFor/fakeAudit/FakeGitHubMinter from the broker test package.
func sshKeySpec(host, keyRef, username, khRef string) types.GrantSpec {
	sc := map[string]string{"host": host, "key_secret_ref": keyRef}
	if username != "" {
		sc["username"] = username
	}
	if khRef != "" {
		sc["known_hosts_secret_ref"] = khRef
	}
	raw, _ := json.Marshal(sc)
	return types.GrantSpec{Kind: types.GrantSSHKey, Scope: raw, RequiresApproval: false, TTLSeconds: 600}
}

// TestMintSSHKey_ReturnsKeyMaterial asserts the private key VALUE is returned in
// Token, the username defaults to "git" (override wins), and known_hosts material
// is returned only when a known_hosts_secret_ref is named.
func TestMintSSHKey_ReturnsKeyMaterial(t *testing.T) {
	const key = "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----\n"
	const kh = "ssh.github.com ssh-ed25519 AAAAexamplehostkey"

	t.Run("default-user-no-known-hosts", func(t *testing.T) {
		secrets := newMemSecrets()
		secrets.m["gh-ssh-key"] = []byte(key)
		db := newFakeDB()
		b := New(db, secrets, &fakeAudit{}, nil, &FakeGitHubMinter{})
		runID := uuid.New()
		gid := seedGrant(db, runID, sshKeySpec("github.com", "gh-ssh-key", "", ""))

		minted, err := b.MintForGrant(context.Background(), callerFor(runID), gid)
		if err != nil {
			t.Fatalf("MintForGrant: %v", err)
		}
		if minted.Kind != types.GrantSSHKey {
			t.Fatalf("kind = %q, want ssh_key", minted.Kind)
		}
		if minted.Token != key {
			t.Fatalf("token = %q, want the stored key material", minted.Token)
		}
		if minted.Username != "git" {
			t.Fatalf("username = %q, want git (default)", minted.Username)
		}
		if minted.KnownHosts != "" {
			t.Fatalf("known_hosts = %q, want empty (no ref)", minted.KnownHosts)
		}
		if minted.JTI == "" {
			t.Fatal("expected a non-empty jti")
		}
	})

	t.Run("override-user-and-known-hosts", func(t *testing.T) {
		secrets := newMemSecrets()
		secrets.m["ado-ssh-key"] = []byte(key)
		secrets.m["ado-known-hosts"] = []byte(kh)
		db := newFakeDB()
		b := New(db, secrets, &fakeAudit{}, nil, &FakeGitHubMinter{})
		runID := uuid.New()
		gid := seedGrant(db, runID, sshKeySpec("dev.azure.com", "ado-ssh-key", "custom", "ado-known-hosts"))

		minted, err := b.MintForGrant(context.Background(), callerFor(runID), gid)
		if err != nil {
			t.Fatalf("MintForGrant: %v", err)
		}
		if minted.Username != "custom" {
			t.Fatalf("username = %q, want custom (override)", minted.Username)
		}
		if minted.KnownHosts != kh {
			t.Fatalf("known_hosts = %q, want the stored material", minted.KnownHosts)
		}
	})
}

// TestMintSSHKey_FailsClosed asserts fail-closed on a missing key secret, empty
// host, empty key_secret_ref, a reserved key secret name, a reserved known_hosts
// secret name, and a missing known_hosts secret that was named.
func TestMintSSHKey_FailsClosed(t *testing.T) {
	secrets := newMemSecrets()
	secrets.m["real-key"] = []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nx\n-----END OPENSSH PRIVATE KEY-----\n")
	secrets.m["wardyn-signing-key"] = []byte("super-secret-signing-key")
	// Seeded so the reserved-name check (not missing-secret) is what fails closed.
	secrets.m["wardyn-harness-anthropic-oauth"] = []byte("resident-oauth-blob")

	cases := []struct {
		name string
		spec types.GrantSpec
	}{
		{"missing-key-secret", sshKeySpec("github.com", "does-not-exist", "", "")},
		{"empty-host", types.GrantSpec{Kind: types.GrantSSHKey, Scope: json.RawMessage(`{"key_secret_ref":"real-key"}`)}},
		{"empty-key-ref", types.GrantSpec{Kind: types.GrantSSHKey, Scope: json.RawMessage(`{"host":"github.com"}`)}},
		{"reserved-key-secret", sshKeySpec("github.com", "wardyn-signing-key", "", "")},
		{"reserved-known-hosts", sshKeySpec("github.com", "real-key", "", "wardyn-signing-key")},
		{"reserved-harness-oauth-key", sshKeySpec("github.com", "wardyn-harness-anthropic-oauth", "", "")},
		{"missing-known-hosts", sshKeySpec("github.com", "real-key", "", "no-such-known-hosts")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := newFakeDB()
			b := New(db, secrets, &fakeAudit{}, nil, &FakeGitHubMinter{})
			runID := uuid.New()
			gid := seedGrant(db, runID, c.spec)
			if _, err := b.MintForGrant(context.Background(), callerFor(runID), gid); err == nil {
				t.Fatalf("%s: expected a fail-closed error, got nil", c.name)
			}
		})
	}
}

// TestMintSSHKey_RegistersMask asserts the mint() path registers the private key
// VALUE in the mask registry (so it is redacted from PTY/asciicast streams — the
// residual-key-exposure mitigation for the resident-key exception).
func TestMintSSHKey_RegistersMask(t *testing.T) {
	const key = "-----BEGIN OPENSSH PRIVATE KEY-----\nsecretkeybytes\n-----END OPENSSH PRIVATE KEY-----\n"
	secrets := newMemSecrets()
	secrets.m["gh-ssh-key"] = []byte(key)
	reg := secretmask.NewRegistry()
	db := newFakeDB()
	b := New(db, secrets, &fakeAudit{}, nil, &FakeGitHubMinter{}).WithMaskRegistry(reg)
	runID := uuid.New()
	gid := seedGrant(db, runID, sshKeySpec("github.com", "gh-ssh-key", "", ""))

	if _, err := b.MintForGrant(context.Background(), callerFor(runID), gid); err != nil {
		t.Fatalf("MintForGrant: %v", err)
	}
	found := false
	for _, s := range reg.Snapshot(runID) {
		if string(s) == key {
			found = true
		}
	}
	if !found {
		t.Fatal("minted SSH key was not registered in the mask registry")
	}
}
