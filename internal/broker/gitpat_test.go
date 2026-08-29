// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// memSecrets is a minimal secretstore.Store for git_pat/ssh_key mint tests. m
// is the OPERATOR namespace (owner == "") — every existing literal in this
// package keeps behaving exactly as before For existed. owned holds every
// non-"" owner's rows, lazily allocated by For so every view derived from the
// same root shares it.
type memSecrets struct {
	owner string
	m     map[string][]byte
	owned map[string]map[string][]byte
}

func newMemSecrets() *memSecrets { return &memSecrets{m: map[string][]byte{}} }

func (s *memSecrets) Name() string { return "mem" }
func (s *memSecrets) Put(_ context.Context, n string, v []byte) error {
	if s.owner == "" {
		s.m[n] = v
		return nil
	}
	if s.owned[s.owner] == nil {
		s.owned[s.owner] = map[string][]byte{}
	}
	s.owned[s.owner][n] = v
	return nil
}
func (s *memSecrets) Delete(_ context.Context, n string) error {
	if s.owner == "" {
		delete(s.m, n)
		return nil
	}
	delete(s.owned[s.owner], n)
	return nil
}
func (s *memSecrets) Get(_ context.Context, n string) ([]byte, error) {
	if s.owner != "" {
		if v, ok := s.owned[s.owner][n]; ok {
			return v, nil
		}
		// Fall through to the operator row below — the owner-view fallback.
	}
	if v, ok := s.m[n]; ok {
		return v, nil
	}
	return nil, secretstore.ErrNotFound
}
func (s *memSecrets) List(_ context.Context) ([]string, error) {
	src := s.m
	if s.owner != "" {
		src = s.owned[s.owner]
	}
	out := make([]string, 0, len(src))
	for k := range src {
		out = append(out, k)
	}
	return out, nil
}

// For returns an owner-scoped view sharing the same backing maps as s — see
// secretstore.Store.For's doc comment for the fallback/isolation contract
// this mirrors.
func (s *memSecrets) For(owner string) secretstore.Store {
	if s.owned == nil {
		s.owned = map[string]map[string][]byte{}
	}
	return &memSecrets{owner: owner, m: s.m, owned: s.owned}
}

func gitPATSpec(host, secret, username string) types.GrantSpec {
	sc := map[string]string{"host": host, "secret_name": secret}
	if username != "" {
		sc["username"] = username
	}
	raw, _ := json.Marshal(sc)
	// Auto-mint (RequiresApproval=false) so MintForGrant runs mint directly.
	return types.GrantSpec{Kind: types.GrantGitPAT, Scope: raw, RequiresApproval: false, TTLSeconds: 600}
}

// TestMintGitPAT_ReturnsStoredValueAndUsername asserts the PAT VALUE is returned
// and the git username is resolved by host (ADO=>pat, GitLab=>oauth2, override
// wins).
func TestMintGitPAT_ReturnsStoredValueAndUsername(t *testing.T) {
	const pat = "pat-value-1234567890"
	cases := []struct {
		name, host, override, wantUser string
	}{
		{"ado", "dev.azure.com", "", "pat"},
		{"ado-visualstudio", "myorg.visualstudio.com", "", "pat"},
		{"gitlab", "gitlab.com", "", "oauth2"},
		{"gitlab-selfmanaged", "gitlab.internal.corp", "", "oauth2"},
		{"override-wins", "dev.azure.com", "custom-user", "custom-user"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			secrets := newMemSecrets()
			secrets.m["the-pat"] = []byte(pat)
			db := newFakeDB()
			b := New(db, secrets, &fakeAudit{}, nil, &FakeGitHubMinter{})
			runID := uuid.New()
			gid := seedGrant(db, runID, gitPATSpec(c.host, "the-pat", c.override))

			minted, err := b.MintForGrant(context.Background(), callerFor(runID), gid)
			if err != nil {
				t.Fatalf("MintForGrant: %v", err)
			}
			if minted.Kind != types.GrantGitPAT {
				t.Fatalf("kind = %q, want git_pat", minted.Kind)
			}
			if minted.Token != pat {
				t.Fatalf("token = %q, want the stored PAT value", minted.Token)
			}
			if minted.Username != c.wantUser {
				t.Fatalf("username = %q, want %q", minted.Username, c.wantUser)
			}
			if minted.JTI == "" {
				t.Fatal("expected a non-empty jti")
			}
		})
	}
}

// TestMintGitPAT_FailsClosed asserts fail-closed on a missing secret, empty
// host, empty secret_name, and a reserved secret name.
func TestMintGitPAT_FailsClosed(t *testing.T) {
	secrets := newMemSecrets()
	secrets.m["real-pat"] = []byte("pat-value-1234567890")
	secrets.m["wardyn-signing-key"] = []byte("super-secret-signing-key")
	// Seeded so the reserved-name check (not missing-secret) is what fails closed.
	secrets.m["wardyn-harness-anthropic-oauth"] = []byte("resident-oauth-blob")
	// W12-B-1: the GitHub App credentials, SSH host key, and Bedrock bearer token
	// must also be refused by the git_pat mint path even though they are NOT
	// reserved at the api_key/injection sink (sinkReservedSecret) — see
	// reservedBrokerSecretNames' doc comment.
	secrets.m["github-app-key"] = []byte("-----BEGIN RSA PRIVATE KEY-----\nfake-app-key-material\n-----END RSA PRIVATE KEY-----\n")
	secrets.m["github-app-id"] = []byte("123456")
	secrets.m["wardyn-ssh-host-key"] = []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nfake-host-key-material\n-----END OPENSSH PRIVATE KEY-----\n")
	secrets.m["wardyn-ui-session-key"] = []byte("0123456789abcdef0123456789abcdef")
	secrets.m["bedrock-api-key"] = []byte("bedrock-bearer-token-value")

	cases := []struct {
		name string
		spec types.GrantSpec
	}{
		{"missing-secret", gitPATSpec("gitlab.com", "does-not-exist", "")},
		{"empty-host", types.GrantSpec{Kind: types.GrantGitPAT, Scope: json.RawMessage(`{"secret_name":"real-pat"}`)}},
		{"empty-secret-name", types.GrantSpec{Kind: types.GrantGitPAT, Scope: json.RawMessage(`{"host":"gitlab.com"}`)}},
		{"reserved-secret", gitPATSpec("gitlab.com", "wardyn-signing-key", "")},
		{"reserved-harness-oauth", gitPATSpec("gitlab.com", "wardyn-harness-anthropic-oauth", "")},
		// W12-B-1 regression: a git_pat grant must not resolve the GitHub App PEM
		// private key (or its sibling platform-internal value-returning secrets)
		// into the sandbox. Fails on base 6d76911 (reservedBrokerSecretNames
		// omitted these names); passes once the map is widened.
		{"reserved-github-app-key", gitPATSpec("github.com", "github-app-key", "")},
		{"reserved-github-app-id", gitPATSpec("github.com", "github-app-id", "")},
		{"reserved-ssh-host-key", gitPATSpec("github.com", "wardyn-ssh-host-key", "")},
		// Same class as the host key: the HMAC key signing the UI-relay cookie.
		// Handed into a sandbox, sandbox-authored JS on the shared UI origin can
		// forge wardyn_ui_sess for any run and any role.
		{"reserved-ui-session-key", gitPATSpec("github.com", "wardyn-ui-session-key", "")},
		{"reserved-bedrock-api-key", gitPATSpec("bedrock-runtime.us-east-1.amazonaws.com", "bedrock-api-key", "")},
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

// TestMintGitPAT_RegistersMask asserts the mint() path registers the PAT value
// in the mask registry (so it is redacted from PTY/asciicast streams).
func TestMintGitPAT_RegistersMask(t *testing.T) {
	const pat = "glpat-abcdefghijklmnopqrst"
	secrets := newMemSecrets()
	secrets.m["gl-pat"] = []byte(pat)
	reg := secretmask.NewRegistry()
	db := newFakeDB()
	b := New(db, secrets, &fakeAudit{}, nil, &FakeGitHubMinter{}).WithMaskRegistry(reg)
	runID := uuid.New()
	gid := seedGrant(db, runID, gitPATSpec("gitlab.com", "gl-pat", ""))

	if _, err := b.MintForGrant(context.Background(), callerFor(runID), gid); err != nil {
		t.Fatalf("MintForGrant: %v", err)
	}
	found := false
	for _, s := range reg.Snapshot(runID) {
		if string(s) == pat {
			found = true
		}
	}
	if !found {
		t.Fatal("minted PAT was not registered in the mask registry")
	}
}

// TestBrokerMint_GitPATAndSSHKey_OwnerScoped: both mintGitPAT and mintSSHKey
// resolve through the run's OWN owner (identity.Claims.Sub, threaded from
// mintKind), never the operator's row when the caller owns one under the
// SAME secret name (0.7, migration 0050, member BYOK). callerFor's bare
// claims (used by every other test in this file) carries no Sub and so
// resolves the operator row, exactly as before this change — see
// TestMintGitPAT_ReturnsStoredValueAndUsername /
// TestMintSSHKey_ReturnsKeyMaterial for that unaffected path.
func TestBrokerMint_GitPATAndSSHKey_OwnerScoped(t *testing.T) {
	t.Run("git_pat", func(t *testing.T) {
		secrets := newMemSecrets()
		secrets.m["shared-pat"] = []byte("operator-pat-value")
		if err := secrets.For("alice").Put(context.Background(), "shared-pat", []byte("alice-pat-value")); err != nil {
			t.Fatalf("seed alice's row: %v", err)
		}
		db := newFakeDB()
		b := New(db, secrets, &fakeAudit{}, nil, &FakeGitHubMinter{})
		runID := uuid.New()
		gid := seedGrant(db, runID, gitPATSpec("dev.azure.com", "shared-pat", ""))
		caller := &identity.Claims{RunID: runID, SPIFFEID: spiffeForRun(runID), Sub: "alice"}

		minted, err := b.MintForGrant(context.Background(), caller, gid)
		if err != nil {
			t.Fatalf("MintForGrant: %v", err)
		}
		if minted.Token != "alice-pat-value" {
			t.Fatalf("token = %q, want alice's own row, not the operator's", minted.Token)
		}
	})

	t.Run("ssh_key", func(t *testing.T) {
		secrets := newMemSecrets()
		secrets.m["shared-ssh-key"] = []byte("operator-key-material")
		if err := secrets.For("alice").Put(context.Background(), "shared-ssh-key", []byte("alice-key-material")); err != nil {
			t.Fatalf("seed alice's row: %v", err)
		}
		db := newFakeDB()
		b := New(db, secrets, &fakeAudit{}, nil, &FakeGitHubMinter{})
		runID := uuid.New()
		gid := seedGrant(db, runID, sshKeySpec("github.com", "shared-ssh-key", "", ""))
		caller := &identity.Claims{RunID: runID, SPIFFEID: spiffeForRun(runID), Sub: "alice"}

		minted, err := b.MintForGrant(context.Background(), caller, gid)
		if err != nil {
			t.Fatalf("MintForGrant: %v", err)
		}
		if minted.Token != "alice-key-material" {
			t.Fatalf("token = %q, want alice's own row, not the operator's", minted.Token)
		}
	})
}

// TestMintOnApproval_MemberRunResolvesOwnerNamespace: an approval-gated
// git_pat mint via MintOnApproval, with the run's CreatedBy passed as sub,
// resolves the MEMBER's own secret row — not the operator's — closing the
// residual ownerOf's doc comment used to name (a bare &identity.Claims{RunID}
// with no Sub always fell back to the operator namespace on this path).
func TestMintOnApproval_MemberRunResolvesOwnerNamespace(t *testing.T) {
	secrets := newMemSecrets()
	secrets.m["shared-pat"] = []byte("operator-pat-value")
	if err := secrets.For("alice").Put(context.Background(), "shared-pat", []byte("alice-pat-value")); err != nil {
		t.Fatalf("seed alice's row: %v", err)
	}
	db := newFakeDB()
	b := New(db, secrets, &fakeAudit{}, nil, &FakeGitHubMinter{})
	runID := uuid.New()
	spec := gitPATSpec("dev.azure.com", "shared-pat", "")
	spec.RequiresApproval = true
	gid := seedGrant(db, runID, spec)
	seedApproval(db, runID, gid, spec.Scope, types.ApprovalApproved)

	minted, err := b.MintOnApproval(context.Background(), runID, gid, "alice")
	if err != nil {
		t.Fatalf("MintOnApproval: %v", err)
	}
	if minted.Token != "alice-pat-value" {
		t.Fatalf("token = %q, want alice's own row (the run's CreatedBy), not the operator's", minted.Token)
	}
}

// TestMintOnApproval_OperatorRunResolvesOperatorNamespace is the negative
// control: an operator-created run (sub == "") still resolves the operator
// namespace exactly as before this fix.
func TestMintOnApproval_OperatorRunResolvesOperatorNamespace(t *testing.T) {
	secrets := newMemSecrets()
	secrets.m["shared-pat"] = []byte("operator-pat-value")
	db := newFakeDB()
	b := New(db, secrets, &fakeAudit{}, nil, &FakeGitHubMinter{})
	runID := uuid.New()
	spec := gitPATSpec("dev.azure.com", "shared-pat", "")
	spec.RequiresApproval = true
	gid := seedGrant(db, runID, spec)
	seedApproval(db, runID, gid, spec.Scope, types.ApprovalApproved)

	minted, err := b.MintOnApproval(context.Background(), runID, gid, "")
	if err != nil {
		t.Fatalf("MintOnApproval: %v", err)
	}
	if minted.Token != "operator-pat-value" {
		t.Fatalf("token = %q, want the operator's row (operator-created run)", minted.Token)
	}
}
