// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// The git remotes of an uploaded scan are untrusted like every other fact:
// unsafe or oversized entries are dropped, the lists are bounded, and what the
// deterministic scan produces passes unchanged (#2290).
func TestDeriveProfile_HostileGitRemotesValidated(t *testing.T) {
	// Pinned literals: the test must build against a tree that has no such constants.
	const (
		maxGitRemotes   = 512
		maxGitRemoteLen = 256
	)
	var facts ScanFacts
	facts.GitRemotes.GitHub = []string{"acme/web", "acme/web", "ac\x00me/x", "ac me/x", "", "o/" + strings.Repeat("r", maxGitRemoteLen)}
	facts.GitRemotes.OtherHosts = []string{"gitlab.com", "evil.example\n", "[2001:db8::1]"}
	for i := range 2 * maxGitRemotes {
		facts.GitRemotes.OtherHosts = append(facts.GitRemotes.OtherHosts, "h"+strconv.Itoa(i)+".example")
	}
	got := DeriveProfile(facts).GitRemotes
	if !reflect.DeepEqual(got.GitHub, []string{"acme/web"}) {
		t.Errorf("GitHub = %q, want only the one clean entry", got.GitHub)
	}
	if len(got.OtherHosts) != maxGitRemotes {
		t.Errorf("OtherHosts = %d entries, want %d", len(got.OtherHosts), maxGitRemotes)
	}
	for _, h := range got.OtherHosts {
		if strings.ContainsAny(h, "\n\x00 ") {
			t.Fatalf("host %q carries a control character", h)
		}
	}
}
