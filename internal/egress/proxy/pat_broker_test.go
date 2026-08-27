// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"testing"

	"github.com/google/uuid"
)

// The path split IS the authorization surface: whatever comes out of it is
// looked up in the per-host allowlist, and anything that is not an exact
// granted host 403s before an upstream URL is formed. So the cases that matter
// are the ones a hostile path could produce.
func TestParsePATBrokerPath(t *testing.T) {
	for _, tc := range []struct {
		in         string
		host, rest string
		ok         bool
	}{
		{"/wardyn/git/gitlab.com/org/repo.git/info/refs", "gitlab.com", "/org/repo.git/info/refs", true},
		{"/wardyn/git/dev.azure.com/o/p/_git/r", "dev.azure.com", "/o/p/_git/r", true},
		// Host matching is case-insensitive, so the key is lowered once here
		// rather than at every lookup.
		{"/wardyn/git/GitLab.COM/o/r", "gitlab.com", "/o/r", true},
		// Not the broker prefix at all.
		{"/wardyn/gh/org/repo", "", "", false},
		{"/wardyn/git/", "", "", false},
		// A host segment with no trailing path: there is nothing to forward.
		{"/wardyn/git/gitlab.com", "", "", false},
	} {
		host, rest, ok := parsePATBrokerPath(tc.in)
		if ok != tc.ok || host != tc.host || rest != tc.rest {
			t.Errorf("parsePATBrokerPath(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.in, host, rest, ok, tc.host, tc.rest, tc.ok)
		}
	}
}

// A traversal must not reach the network. It does not need its own rejection
// branch — it simply cannot BE a granted host — but that is a property worth
// pinning, because someone could later "fix" the parse to be more permissive.
func TestParsePATBrokerPath_TraversalIsNotAGrantedHost(t *testing.T) {
	granted := map[string]PATGrant{"gitlab.com": {GrantID: uuid.New()}}
	for _, in := range []string{
		"/wardyn/git/../gh/org/repo",
		"/wardyn/git/..%2f..%2fetc/passwd",
		"/wardyn/git/evil.example.com/o/r",
		"/wardyn/git/gitlab.com.evil.example.com/o/r",
		// userinfo smuggled into the host segment
		"/wardyn/git/user@evil.example.com/o/r",
	} {
		host, _, ok := parsePATBrokerPath(in)
		if !ok {
			continue // rejected outright, fine
		}
		if _, isGranted := granted[host]; isGranted {
			t.Errorf("parsePATBrokerPath(%q) produced the GRANTED host %q — a path that is not a plain granted host must never reach the allowlist", in, host)
		}
	}
}

// The lane is off unless dispatch populates it, and off means the route 403s
// rather than falling back to anything.
func TestPATGrants_EmptyMeansNoHostBrokered(t *testing.T) {
	p := &Proxy{}
	if len(p.patGrants) != 0 {
		t.Fatal("a proxy built with no PAT grants must broker nothing")
	}
	if _, ok := p.patGrants["gitlab.com"]; ok {
		t.Error("an ungranted host resolved a grant")
	}
}
