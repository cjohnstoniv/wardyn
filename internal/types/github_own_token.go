// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"strings"
	"time"
)

// A person's own GitHub token on a GitHub row that takes one (internal/api
// gh_own_pat.go). Shared here because two packages read the one stored shape:
// the API door that writes it and the broker that hands it to the run's proxy.

const (
	gitHubOwnTokenPrefix = "wardyn-harness-ghown-"
	gitHubOwnTokenSuffix = "-oauth"
)

// GitHubOwnTokenSecretName is the sealed store name holding one person's
// token for one row. It matches the reserved wardyn-harness-*-oauth pattern,
// so the generic secrets API and every other sink refuse it. The caller must
// have validated rowID.
func GitHubOwnTokenSecretName(rowID string) string {
	return gitHubOwnTokenPrefix + rowID + gitHubOwnTokenSuffix
}

// IsGitHubOwnTokenSecret reports whether name is a GitHubOwnTokenSecretName.
func IsGitHubOwnTokenSecret(name string) bool {
	return strings.HasPrefix(name, gitHubOwnTokenPrefix) && strings.HasSuffix(name, gitHubOwnTokenSuffix) &&
		len(name) > len(gitHubOwnTokenPrefix)+len(gitHubOwnTokenSuffix)
}

// GitHubOwnToken is the stored value: the token and the GitHub account it is
// bound to. AccountID is GitHub's numeric user id from GET /user (a login can
// change hands; the id cannot). ExpiresOn is the start (00:00 UTC) of the day
// the person said the token expires, which Wardyn has not checked with GitHub.
type GitHubOwnToken struct {
	Token     string    `json:"token"`
	AccountID int64     `json:"account_id"`
	Login     string    `json:"login"`
	ExpiresOn time.Time `json:"expires_on"`
	StoredAt  time.Time `json:"stored_at"`
}

// Valid reports whether every field a use needs is present.
func (t GitHubOwnToken) Valid() bool {
	return t.Token != "" && t.AccountID > 0 && !t.ExpiresOn.IsZero()
}

// Expired reports whether now is on or past the expiry the person entered.
func (t GitHubOwnToken) Expired(now time.Time) bool { return !now.Before(t.ExpiresOn) }
