// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// PushContentMaxPaths caps the review-matched paths a PushContentScope names.
// PathsTotal carries the exact count; the full list goes to the sidecar's
// structured log, never to a stored row or a refusal body.
const PushContentMaxPaths = 10

// PushContentScope is the requested_scope of an ApprovalPushContent row — the
// bytes the console renders on the push card, stored verbatim.
//
// The dedup key is the whole scope: approval.RequestApproval and the partial
// unique index from migration 0022 both compare it, so two raises of the same
// push collapse into one PENDING row. What makes it the right key is Commits
// plus PathsDigest. Commits are content addresses, so a repack of the same
// commits — git's retry after an approval — yields the same scope where a pack
// digest would not. PathsDigest covers EVERY review-matched path, not just the
// ten Paths names, so two pushes whose first ten matches agree are still two
// questions.
type PushContentScope struct {
	// Repo is the repository as the run's grant names it: "github.com/<owner>/<repo>"
	// on the GitHub App lane, "<host>/<path>" on the git_pat lane (for Azure
	// DevOps, "dev.azure.com/<org>/<project>/_git/<repo>").
	Repo string `json:"repo"`
	// Branch is the ref the push updates, "refs/heads/…". A push that updates
	// several names them all, sorted and joined by ", ".
	Branch string `json:"branch"`
	// ActsAs is the credential the forwarded push authenticates with, as
	// "<grant kind>:<grant id>" — "github_token:<uuid>" or "git_pat:<uuid>".
	ActsAs string `json:"acts_as"`
	// Paths are the first PushContentMaxPaths review-matched paths, sorted. A
	// path ending in "/" is a directory the push does not carry.
	Paths []string `json:"paths"`
	// PathsTotal is how many paths matched a review pattern in all.
	PathsTotal int `json:"paths_total"`
	// Commits are the object ids the push sets its refs to, sorted.
	Commits []string `json:"commits"`
	// PathsDigest is the lower-case hex SHA-256 over every review-matched path,
	// sorted, each followed by a NUL byte (a git path cannot contain one).
	PathsDigest string `json:"paths_digest"`
}

// Validate checks the shape the proxy writes, so a row the console renders
// can be trusted to be one: every bound here is one the sidecar already
// honours.
func (s PushContentScope) Validate() error {
	if s.Repo == "" || s.ActsAs == "" {
		return errors.New("repo and acts_as are required")
	}
	for _, ref := range strings.Split(s.Branch, ", ") {
		if !strings.HasPrefix(ref, "refs/") {
			return fmt.Errorf("branch %q does not name refs", s.Branch)
		}
	}
	if len(s.Paths) != min(s.PathsTotal, PushContentMaxPaths) || len(s.Paths) == 0 {
		return fmt.Errorf("paths names %d of %d, want the first %d", len(s.Paths), s.PathsTotal, PushContentMaxPaths)
	}
	if !sortedUnique(s.Paths) || slices.Contains(s.Paths, "") {
		return errors.New("paths must be non-empty, sorted and distinct")
	}
	if len(s.Commits) == 0 || !sortedUnique(s.Commits) {
		return errors.New("commits must be non-empty, sorted and distinct")
	}
	for _, c := range s.Commits {
		if (len(c) != 40 && len(c) != 64) || !lowerHex(c) {
			return fmt.Errorf("commit %q is not an object id", c)
		}
	}
	if len(s.PathsDigest) != 64 || !lowerHex(s.PathsDigest) {
		return errors.New("paths_digest is not a SHA-256")
	}
	return nil
}

func sortedUnique(ss []string) bool {
	for i := 1; i < len(ss); i++ {
		if ss[i-1] >= ss[i] {
			return false
		}
	}
	return true
}

func lowerHex(s string) bool {
	return strings.Trim(s, "0123456789abcdef") == ""
}
