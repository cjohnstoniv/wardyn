// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// FakeGitHubMinter is a deterministic GitHubMinter for tests. It records the
// last request so tests can assert the broker passed exactly the clamped
// scope, and returns a synthetic token.
type FakeGitHubMinter struct {
	mu sync.Mutex

	// Token/Expiry returned by MintInstallationToken (Expiry defaults to +1h).
	Token  string
	Expiry time.Time
	// Err, if set, is returned (to exercise the failure-audit path).
	Err error

	// Captured inputs from the last call.
	LastRepos       []string
	LastPermissions map[string]string
	LastTTL         time.Duration
	Calls           int

	// RefRulesetConfined/Detail/Err drive VerifyRefRuleset. The zero value
	// answers "not confined", which is the honest default for a fake with no
	// GitHub behind it — a test that wants the WARDYN_GITHUB_REQUIRE_REF_RULESET
	// gate to PASS has to say so.
	RefRulesetConfined bool
	RefRulesetDetail   string
	RefRulesetErr      error
	// LastVerifiedRepos records every repo VerifyRefRuleset was asked about.
	LastVerifiedRepos []string
}

func (f *FakeGitHubMinter) MintInstallationToken(_ context.Context, repos []string, permissions map[string]string, ttl time.Duration) (string, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	f.LastRepos = repos
	f.LastPermissions = permissions
	f.LastTTL = ttl
	if f.Err != nil {
		return "", time.Time{}, f.Err
	}
	tok := f.Token
	if tok == "" {
		tok = "ghs_faketoken_" + uuid.NewString()
	}
	exp := f.Expiry
	if exp.IsZero() {
		exp = time.Now().Add(time.Hour)
	}
	return tok, exp, nil
}

func (f *FakeGitHubMinter) VerifyRefRuleset(_ context.Context, repo string) (bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.LastVerifiedRepos = append(f.LastVerifiedRepos, repo)
	if f.RefRulesetErr != nil {
		return false, "", f.RefRulesetErr
	}
	detail := f.RefRulesetDetail
	if detail == "" {
		detail = repo + ": fake minter, no ruleset behind it."
	}
	return f.RefRulesetConfined, detail, nil
}
