// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// memSecrets is a minimal secretstore.Store for git_pat mint tests.
type memSecrets struct{ m map[string][]byte }

func newMemSecrets() *memSecrets { return &memSecrets{m: map[string][]byte{}} }

func (s *memSecrets) Name() string                                    { return "mem" }
func (s *memSecrets) Put(_ context.Context, n string, v []byte) error { s.m[n] = v; return nil }
func (s *memSecrets) Delete(_ context.Context, n string) error        { delete(s.m, n); return nil }
func (s *memSecrets) Get(_ context.Context, n string) ([]byte, error) {
	if v, ok := s.m[n]; ok {
		return v, nil
	}
	return nil, secretstore.ErrNotFound
}
func (s *memSecrets) List(_ context.Context) ([]string, error) {
	out := make([]string, 0, len(s.m))
	for k := range s.m {
		out = append(out, k)
	}
	return out, nil
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

	cases := []struct {
		name string
		spec types.GrantSpec
	}{
		{"missing-secret", gitPATSpec("gitlab.com", "does-not-exist", "")},
		{"empty-host", types.GrantSpec{Kind: types.GrantGitPAT, Scope: json.RawMessage(`{"secret_name":"real-pat"}`)}},
		{"empty-secret-name", types.GrantSpec{Kind: types.GrantGitPAT, Scope: json.RawMessage(`{"host":"gitlab.com"}`)}},
		{"reserved-secret", gitPATSpec("gitlab.com", "wardyn-signing-key", "")},
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
