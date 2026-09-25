// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// PushContentMaxPaths caps the review-matched paths a PushContentScope names.
// PathsTotal carries the exact count; the complete list rides beside the scope
// as a PushPathList and is stored per approval, never in the scope (the dedup
// key) or a refusal body.
const PushContentMaxPaths = 10

// The bounds of a PushPathList: at most PushPathListMaxPaths paths and
// PushPathListMaxBytes bytes of path text, whichever cuts the list first.
const (
	PushPathListMaxPaths = 10_000
	PushPathListMaxBytes = 1 << 20
)

// PushPathList is a held push's complete review-matched path list, sorted and
// bounded: every path when Truncated is false, otherwise the first ones that
// fit the bounds. The sidecar sends it beside the push_content
// requested_scope; the control plane verifies it against that scope
// (VerifyAgainst) and keeps it immutable per approval.
type PushPathList struct {
	Paths     []string `json:"paths"`
	Truncated bool     `json:"truncated"`
}

// NewPushPathList bounds paths, which are sorted and distinct.
func NewPushPathList(paths []string) PushPathList {
	n, size := 0, 0
	for n < len(paths) && n < PushPathListMaxPaths && size+len(paths[n]) <= PushPathListMaxBytes {
		size += len(paths[n])
		n++
	}
	return PushPathList{Paths: paths[:n], Truncated: n < len(paths)}
}

// VerifyAgainst checks l is the list s was built from: sorted, distinct and
// within bounds, opening with s.Paths, and — unless Truncated — exactly
// PathsTotal paths whose digest is s.PathsDigest. Where a truncated list was
// cut is the sidecar's word: nothing the scope carries names the next path.
func (l PushPathList) VerifyAgainst(s PushContentScope) error {
	size := 0
	for _, p := range l.Paths {
		size += len(p)
	}
	head := min(len(l.Paths), len(s.Paths))
	switch {
	case len(l.Paths) > PushPathListMaxPaths || size > PushPathListMaxBytes:
		return fmt.Errorf("path list is over %d paths or %d bytes", PushPathListMaxPaths, PushPathListMaxBytes)
	case !sortedUnique(l.Paths) || slices.Contains(l.Paths, ""):
		return errors.New("path list must be non-empty paths, sorted and distinct")
	case !slices.Equal(l.Paths[:head], s.Paths[:head]):
		return errors.New("path list does not open with the scope's paths")
	case l.Truncated && len(l.Paths) >= s.PathsTotal:
		return fmt.Errorf("a truncated path list holds %d paths, want fewer than paths_total %d", len(l.Paths), s.PathsTotal)
	case !l.Truncated && len(l.Paths) != s.PathsTotal:
		return fmt.Errorf("path list holds %d paths, want paths_total %d", len(l.Paths), s.PathsTotal)
	case !l.Truncated && PushPathsDigest(l.Paths) != s.PathsDigest:
		return errors.New("path list does not match paths_digest")
	}
	return nil
}

// PushPathsDigest is PushContentScope.PathsDigest over paths.
func PushPathsDigest(paths []string) string {
	h := sha256.New()
	for _, p := range paths {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

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

	// ActsAsKind and ActsAsLabel are SERVER-SET: the control plane resolves
	// ActsAs against the run's own grants and stamps them before the row is
	// stored, and refuses a raise that carries either (a sidecar's word on who
	// a push acts as is not taken). ActsAsKind is one of the PushActsAs*
	// constants; ActsAsLabel is the principal the console names.
	ActsAsKind  string `json:"acts_as_kind,omitempty"`
	ActsAsLabel string `json:"acts_as_label,omitempty"`
}

// The PushContentScope.ActsAsKind values: which lane's credential a held push
// authenticates with, so the console can word the card.
const (
	PushActsAsGitHubApp = "github_app"
	PushActsAsGitPAT    = "git_pat"
	PushActsAsADOEntra  = "ado_entra"
)

// PushActsAsOperator is the ActsAsLabel of a git_pat whose secret is the
// operator's shared one rather than the run owner's own: no person owns it.
const PushActsAsOperator = "operator"

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
