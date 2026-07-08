// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"strings"
	"testing"

	gh "github.com/google/go-github/v74/github"
)

// These are PURE unit tests for the github.go helpers that the DB-backed broker
// tests never reach directly: splitRepos (owner/name validation + single-owner
// invariant) and toInstallationPermissions (the JSON round-trip that maps a
// clamped string map onto go-github's typed InstallationPermissions, failing
// closed on any permission GitHub would not recognize). No network, no App key.

// splitRepos must accept "owner/name" repos that all share one owner, returning
// the owner plus bare names, and reject anything malformed or cross-owner. The
// single-owner rule matters because a GitHub installation token is per-owner:
// allowing mixed owners would silently widen the token across installations.
func TestSplitRepos(t *testing.T) {
	tests := []struct {
		name      string
		repos     []string
		wantOwner string
		wantNames []string
		wantErr   string // substring; "" means expect success
	}{
		{
			name:      "single_repo",
			repos:     []string{"acme/widgets"},
			wantOwner: "acme",
			wantNames: []string{"widgets"},
		},
		{
			name:      "multiple_same_owner",
			repos:     []string{"acme/widgets", "acme/gadgets"},
			wantOwner: "acme",
			wantNames: []string{"widgets", "gadgets"},
		},
		{
			name:    "cross_owner_rejected",
			repos:   []string{"acme/widgets", "globex/secrets"},
			wantErr: "share an owner",
		},
		{
			name:    "missing_slash_rejected",
			repos:   []string{"justaname"},
			wantErr: "owner/name form",
		},
		{
			name:    "empty_owner_rejected",
			repos:   []string{"/widgets"},
			wantErr: "owner/name form",
		},
		{
			name:    "empty_name_rejected",
			repos:   []string{"acme/"},
			wantErr: "owner/name form",
		},
		{
			name:    "empty_string_rejected",
			repos:   []string{""},
			wantErr: "owner/name form",
		},
		{
			name:      "name_with_extra_slash_kept_intact",
			repos:     []string{"acme/widgets/extra"},
			wantOwner: "acme",
			// SplitN(_, 2) keeps everything after the first slash as the name.
			wantNames: []string{"widgets/extra"},
		},
		{
			name:    "second_repo_malformed_rejected",
			repos:   []string{"acme/widgets", "broken"},
			wantErr: "owner/name form",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			owner, names, err := splitRepos(tc.repos)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("splitRepos(%v) = (%q,%v,nil), want error containing %q", tc.repos, owner, names, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("splitRepos error = %q, want substring %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitRepos(%v) unexpected error: %v", tc.repos, err)
			}
			if owner != tc.wantOwner {
				t.Fatalf("owner = %q, want %q", owner, tc.wantOwner)
			}
			if len(names) != len(tc.wantNames) {
				t.Fatalf("names = %v, want %v", names, tc.wantNames)
			}
			for i := range tc.wantNames {
				if names[i] != tc.wantNames[i] {
					t.Fatalf("names[%d] = %q, want %q", i, names[i], tc.wantNames[i])
				}
			}
		})
	}
}

// toInstallationPermissions maps an empty map to a nil struct (so the token
// request omits permissions and GitHub uses the installation default).
func TestToInstallationPermissions_EmptyIsNil(t *testing.T) {
	ip, err := toInstallationPermissions(map[string]string{})
	if err != nil {
		t.Fatalf("empty perms: unexpected error %v", err)
	}
	if ip != nil {
		t.Fatalf("empty perms should map to nil InstallationPermissions, got %+v", ip)
	}
	ip, err = toInstallationPermissions(nil)
	if err != nil {
		t.Fatalf("nil perms: unexpected error %v", err)
	}
	if ip != nil {
		t.Fatalf("nil perms should map to nil InstallationPermissions, got %+v", ip)
	}
}

// toInstallationPermissions round-trips the clamped ceiling permission set onto
// the typed struct using go-github's json tags. This asserts the ACTUAL,
// currently-implemented permission shaping: contents:write + pull_requests:write
// + metadata:read. (Branch-push confinement is advisory metadata only and is
// NOT enforced as a permission clamp today — see broker.go branchNamespaceFormat
// — so we do not assert any push-ref restriction here.)
func TestToInstallationPermissions_CeilingRoundTrip(t *testing.T) {
	// Feed the clamp output so the test tracks the documented permission set.
	clamped := clampGitHubPermissions(map[string]string{
		"contents":      "write",
		"pull_requests": "write",
	})
	ip, err := toInstallationPermissions(clamped)
	if err != nil {
		t.Fatalf("round-trip ceiling perms: %v", err)
	}
	if ip == nil {
		t.Fatal("expected non-nil InstallationPermissions for non-empty perms")
	}
	if ip.GetContents() != "write" {
		t.Fatalf("Contents = %q, want write", ip.GetContents())
	}
	if ip.GetPullRequests() != "write" {
		t.Fatalf("PullRequests = %q, want write", ip.GetPullRequests())
	}
	if ip.GetMetadata() != "read" {
		t.Fatalf("Metadata = %q, want read", ip.GetMetadata())
	}
	// Nothing outside the ceiling should have been set.
	if ip.GetAdministration() != "" {
		t.Fatalf("Administration unexpectedly set to %q (must be unset)", ip.GetAdministration())
	}
	if ip.GetWorkflows() != "" {
		t.Fatalf("Workflows unexpectedly set to %q (must be unset)", ip.GetWorkflows())
	}
}

// toInstallationPermissions must FAIL CLOSED on any permission name go-github
// (hence GitHub) does not recognize: DisallowUnknownFields rejects the decode
// rather than silently dropping it, so a typo or injected perm cannot slip a
// token request through with an unintended (or no-op-but-misleading) scope.
func TestToInstallationPermissions_UnknownPermissionRejected(t *testing.T) {
	tests := []struct {
		name  string
		perms map[string]string
	}{
		{"bogus_key", map[string]string{"not_a_real_perm": "write"}},
		{"typo_contents", map[string]string{"content": "write"}},
		{"valid_plus_bogus", map[string]string{"contents": "write", "totally_made_up": "read"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ip, err := toInstallationPermissions(tc.perms)
			if err == nil {
				t.Fatalf("expected fail-closed error for %v, got %+v", tc.perms, ip)
			}
			if !strings.Contains(err.Error(), "unknown github permission") {
				t.Fatalf("error = %q, want it to mention an unknown github permission", err.Error())
			}
		})
	}
}

// A known go-github permission outside our clamp ceiling (e.g. administration)
// is still a VALID json field, so toInstallationPermissions itself accepts it.
// This documents the layering: toInstallationPermissions only rejects names
// GitHub does not know; the no-widening ceiling is enforced earlier by
// clampGitHubPermissions (which would have dropped this key). Guards against a
// future refactor that conflates the two responsibilities.
func TestToInstallationPermissions_KnownButOutOfCeilingIsValidJSON(t *testing.T) {
	ip, err := toInstallationPermissions(map[string]string{"administration": "write"})
	if err != nil {
		t.Fatalf("known permission administration should decode without error, got %v", err)
	}
	if ip.GetAdministration() != "write" {
		t.Fatalf("Administration = %q, want write (decode is faithful)", ip.GetAdministration())
	}
	// Sanity: the clamp ceiling, the actual enforcement point, drops it.
	if _, present := clampGitHubPermissions(map[string]string{"administration": "write"})["administration"]; present {
		t.Fatal("clamp ceiling must drop administration (no-widening); decode-level acceptance is not the enforcement point")
	}
}

// Compile-time assurance the round-trip uses the real go-github type so the
// json-tag coupling under test is the production one, not a local stand-in.
var _ *gh.InstallationPermissions = (*gh.InstallationPermissions)(nil)
