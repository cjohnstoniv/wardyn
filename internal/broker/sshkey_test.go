// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/json"
	"errors"
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
	// The GitHub App credentials, SSH host key, and Bedrock bearer token
	// must also be refused by the ssh_key mint path (both as the key material AND
	// as known_hosts) even though they are NOT reserved at the api_key/injection
	// sink (sinkReservedSecret) — see reservedBrokerSecretNames' doc comment.
	secrets.m["github-app-key"] = []byte("-----BEGIN RSA PRIVATE KEY-----\nfake-app-key-material\n-----END RSA PRIVATE KEY-----\n")
	secrets.m["github-app-id"] = []byte("123456")
	secrets.m["wardyn-ssh-host-key"] = []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nfake-host-key-material\n-----END OPENSSH PRIVATE KEY-----\n")
	secrets.m["wardyn-ui-session-key"] = []byte("0123456789abcdef0123456789abcdef")
	secrets.m["bedrock-api-key"] = []byte("bedrock-bearer-token-value")
	const cliSignIn = cliSecretPrefix + "6f0c1d2e-0000-4000-8000-0000000c0a05-sso"
	secrets.m[cliSignIn] = []byte(`{"access_token":"planted","refresh_token":"planted"}`)

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
		// An ssh_key grant must not resolve the GitHub App PEM private key (or its
		// sibling platform-internal value-returning secrets) into the sandbox,
		// whether named as the key material or as known_hosts:
		// reservedBrokerSecretNames covers these names.
		{"reserved-github-app-key-as-keyref", sshKeySpec("github.com", "github-app-key", "", "")},
		{"reserved-github-app-id-as-keyref", sshKeySpec("github.com", "github-app-id", "", "")},
		{"reserved-ssh-host-key-as-keyref", sshKeySpec("github.com", "wardyn-ssh-host-key", "", "")},
		{"reserved-ui-session-key-as-keyref", sshKeySpec("github.com", "wardyn-ui-session-key", "", "")},
		{"reserved-bedrock-api-key-as-keyref", sshKeySpec("github.com", "bedrock-api-key", "", "")},
		{"reserved-github-app-key-as-known-hosts", sshKeySpec("github.com", "real-key", "", "github-app-key")},
		{"reserved-cli-sign-in-as-keyref", sshKeySpec("github.com", cliSignIn, "", "")},
		{"reserved-cli-sign-in-as-known-hosts", sshKeySpec("github.com", "real-key", "", cliSignIn)},
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

// TestMintSSHKey_RegistersMask asserts the mint() path registers the private
// key value in the mask registry (so it is redacted from PTY/asciicast streams
// — the residual-key-exposure mitigation for the resident-key exception), and
// registers the known_hosts material too, so no mint output skips mask
// registration.
func TestMintSSHKey_RegistersMask(t *testing.T) {
	const key = "-----BEGIN OPENSSH PRIVATE KEY-----\nsecretkeybytes\n-----END OPENSSH PRIVATE KEY-----\n"
	const kh = "github.com ssh-ed25519 AAAAsecretknownhostsmaterial"
	secrets := newMemSecrets()
	secrets.m["gh-ssh-key"] = []byte(key)
	secrets.m["gh-known-hosts"] = []byte(kh)
	reg := secretmask.NewRegistry()
	db := newFakeDB()
	b := New(db, secrets, &fakeAudit{}, nil, &FakeGitHubMinter{}).WithMaskRegistry(reg)
	runID := uuid.New()
	gid := seedGrant(db, runID, sshKeySpec("github.com", "gh-ssh-key", "", "gh-known-hosts"))

	if _, err := b.MintForGrant(context.Background(), callerFor(runID), gid); err != nil {
		t.Fatalf("MintForGrant: %v", err)
	}
	var foundKey, foundKnownHosts bool
	for _, s := range reg.Snapshot(runID) {
		switch string(s) {
		case key:
			foundKey = true
		case kh:
			foundKnownHosts = true
		}
	}
	if !foundKey {
		t.Fatal("minted SSH key was not registered in the mask registry")
	}
	if !foundKnownHosts {
		t.Fatal("minted known_hosts material was not registered in the mask registry (W12-B-2)")
	}
}

// TestMintSSHKey_AutoMintOncePerRun: an auto-mint ssh_key is issued once per
// run. The second ask is ErrAlreadyMinted with one denied audit row and no
// second success; a failed first preparation does not count, and a grant of
// another run is unaffected.
func TestMintSSHKey_AutoMintOncePerRun(t *testing.T) {
	const key = "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----\n"
	secrets := newMemSecrets()
	db := newFakeDB()
	aud := &fakeAudit{}
	b := New(db, secrets, aud, nil, &FakeGitHubMinter{})
	runID := uuid.New()
	gid := seedGrant(db, runID, sshKeySpec("github.com", "gh-ssh-key", "", ""))
	ctx := context.Background()

	// The key secret is not there yet: the preparation fails and records no success.
	if _, err := b.MintForGrant(ctx, callerFor(runID), gid); err == nil {
		t.Fatal("mint with no stored key: want an error")
	}
	secrets.m["gh-ssh-key"] = []byte(key)
	if _, err := b.MintForGrant(ctx, callerFor(runID), gid); err != nil {
		t.Fatalf("first mint after a failed preparation: %v", err)
	}
	denied := func() int {
		aud.mu.Lock()
		defer aud.mu.Unlock()
		n := 0
		for _, ev := range aud.events {
			if ev.Action == "credential.mint" && ev.Outcome == "denied" && ev.Target == gid.String() {
				n++
			}
		}
		return n
	}
	minted, err := b.MintForGrant(ctx, callerFor(runID), gid)
	if !errors.Is(err, ErrAlreadyMinted) || minted.Token != "" {
		t.Fatalf("second mint = (%q, %v), want ErrAlreadyMinted and no value", minted.Token, err)
	}
	if n := denied(); n != 1 {
		t.Fatalf("denied credential.mint rows = %d, want 1", n)
	}
	if n := len(db.mintAudits()); n != 1 {
		t.Fatalf("in-tx credential.mint rows = %d, want the one success", n)
	}

	other := uuid.New()
	ogid := seedGrant(db, other, sshKeySpec("github.com", "gh-ssh-key", "", ""))
	if _, err := b.MintForGrant(ctx, callerFor(other), ogid); err != nil {
		t.Fatalf("another run's grant: %v", err)
	}

	// A winner that commits while this mint is prepared is seen by the second
	// transaction: refused there, with one denied row, not two.
	raceDB := newFakeDB()
	raceDB.sshWinnerAfter = 1
	raceAud := &fakeAudit{}
	rb := New(raceDB, secrets, raceAud, nil, &FakeGitHubMinter{})
	rgid := seedGrant(raceDB, runID, sshKeySpec("github.com", "gh-ssh-key", "", ""))
	if minted, err := rb.MintForGrant(ctx, callerFor(runID), rgid); !errors.Is(err, ErrAlreadyMinted) || minted.Token != "" {
		t.Fatalf("mint losing the race = (%q, %v), want ErrAlreadyMinted", minted.Token, err)
	}
	n := 0
	for _, ev := range raceAud.events {
		if ev.Action == "credential.mint" && ev.Outcome == "denied" {
			n++
		}
	}
	if n != 1 || len(raceDB.mintAudits()) != 0 {
		t.Fatalf("losing the race wrote %d denied rows and %d successes, want 1 and 0", n, len(raceDB.mintAudits()))
	}
}
