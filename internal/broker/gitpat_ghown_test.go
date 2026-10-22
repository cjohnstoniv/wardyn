// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A person's own GitHub token is minted for the run's proxy from the run
// owner's own sealed value: the token, the person's login as the git user
// name, and a lease that never outlives the expiry the person entered. Every
// grant shape create never writes is refused.
func TestMintGitHubOwnToken(t *testing.T) {
	name := types.GitHubOwnTokenSecretName("gh-own")
	soon := time.Now().Add(2 * time.Minute).UTC()
	blob := func(expires time.Time) []byte {
		raw, _ := json.Marshal(types.GitHubOwnToken{Token: "github_pat_alice", AccountID: 42, Login: "alice-gh", ExpiresOn: expires})
		return raw
	}
	spec := func(host string, ownerOnly bool) types.GrantSpec {
		s := gitPATSpec(host, name, "")
		s.OwnerOnly = ownerOnly
		return s
	}
	mint := func(t *testing.T, stored []byte, s types.GrantSpec, operatorOwned bool) (Minted, error) {
		t.Helper()
		secrets := newMemSecrets()
		if stored != nil {
			if err := secrets.For("alice").Put(context.Background(), name, stored); err != nil {
				t.Fatal(err)
			}
		}
		db := newFakeDB()
		b := New(db, secrets, &fakeAudit{}, nil, &FakeGitHubMinter{})
		runID := uuid.New()
		gid := seedGrant(db, runID, s)
		caller := &identity.Claims{RunID: runID, SPIFFEID: spiffeForRun(defaultTrustDomain, runID), Sub: "alice", OperatorOwned: operatorOwned}
		return b.MintForGrant(context.Background(), caller, gid)
	}

	t.Run("the owner's token, login and capped lease", func(t *testing.T) {
		minted, err := mint(t, blob(soon), spec("github.com", true), false)
		if err != nil {
			t.Fatalf("MintForGrant: %v", err)
		}
		if minted.Token != "github_pat_alice" || minted.Username != "alice-gh" {
			t.Fatalf("minted token %q user %q, want the stored token and the login", minted.Token, minted.Username)
		}
		if minted.ExpiresAt.After(soon) {
			t.Fatalf("lease %v outlives the entered expiry %v", minted.ExpiresAt, soon)
		}
	})

	refused := map[string]struct {
		stored        []byte
		spec          types.GrantSpec
		operatorOwned bool
	}{
		"expired":                    {blob(time.Now().Add(-time.Minute)), spec("github.com", true), false},
		"not owner_only":             {blob(soon), spec("github.com", false), false},
		"another host":               {blob(soon), spec("gitlab.com", true), false},
		"an operator-owned run":      {blob(soon), spec("github.com", true), true},
		"a value Wardyn never wrote": {[]byte("github_pat_raw"), spec("github.com", true), false},
		"nothing stored":             {nil, spec("github.com", true), false},
	}
	for label, c := range refused {
		t.Run(label, func(t *testing.T) {
			minted, err := mint(t, c.stored, c.spec, c.operatorOwned)
			if err == nil {
				t.Fatalf("minted %+v, want a refusal", minted)
			}
			if strings.Contains(err.Error(), "github_pat_") {
				t.Fatalf("error %q carries the stored value", err)
			}
		})
	}
}
