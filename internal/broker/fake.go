// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"errors"
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

	// Revoked counts Revoke calls and RevokedTokens records what was handed
	// back, so a test can assert BOTH that every discarded token was revoked
	// (Revoked == Calls-1 on a lost race) and that the RETURNED one was not.
	Revoked       int
	RevokedTokens []string
	// RevokeErr, if set, is returned by Revoke — the best-effort contract says
	// a failed revoke must not change the caller's own error.
	RevokeErr error
}

func (f *FakeGitHubMinter) MintInstallationToken(_ context.Context, repos []string, permissions map[string]string, ttl time.Duration) (string, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	f.LastRepos = repos
	f.LastPermissions = permissions
	f.LastTTL = ttl
	// Reproduce the ONE precondition the real minter enforces before it talks to
	// GitHub (githubMinter.MintInstallationToken): an installation token is
	// per-installation and the owner is derived from the first repo, so an empty
	// repo list cannot mint. A fake that accepted it made every caller with an
	// empty scope.repos look healthy in tests while 502-ing in production.
	if len(repos) == 0 {
		return "", time.Time{}, errors.New("broker: github token requires at least one repo")
	}
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

// Revoke records the hand-back. An empty token is a no-op here exactly as it is
// in the real minter, so a fake that is asked to revoke "nothing" does not
// inflate the count a test is asserting on.
func (f *FakeGitHubMinter) Revoke(_ context.Context, token string) error {
	if token == "" {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Revoked++
	f.RevokedTokens = append(f.RevokedTokens, token)
	return f.RevokeErr
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
