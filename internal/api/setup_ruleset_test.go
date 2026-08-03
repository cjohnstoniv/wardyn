// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fakeRefRulesetVerifier counts calls so the cache can be asserted on.
type fakeRefRulesetVerifier struct {
	confined bool
	detail   string
	err      error
	calls    int
	lastRepo string
}

func (f *fakeRefRulesetVerifier) VerifyRefRuleset(_ context.Context, repo string) (bool, string, error) {
	f.calls++
	f.lastRepo = repo
	return f.confined, f.detail, f.err
}

// refRulesetCheck must NEVER grade "fail" — it is advisory, and an outbound call
// that errors is UNKNOWN, not a security regression.
func TestRefRulesetCheck_Grading(t *testing.T) {
	cases := []struct {
		name       string
		confined   bool
		detail     string
		err        error
		wantStatus string
		wantIn     []string
	}{
		// A green row must not read as "the App is confined": the checklist
		// probes ONE repo while the mint gate grades every repo in the grant.
		{"confined is ok, and says it graded one repo", true,
			"acme/widgets: creation, update and deletion are restricted.", nil, "ok",
			[]string{"acme/widgets only", "installation token"}},
		{"unconfined warns", false, "acme/widgets: no ruleset.", nil, "warn",
			[]string{"receive-pack parser"}},
		{"timeout is unknown, not a regression", false, "", errors.New("i/o timeout"), "info",
			[]string{"UNKNOWN"}},
		// Both outbound reads (branch rules, ruleset bypass mode) are documented
		// under the same permission, so the unknown row must not name just one.
		{"403 on the permission is unknown too", false, "", errors.New("403 Resource not accessible"), "info",
			[]string{"Metadata: read", "bypass mode"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chk := refRulesetCheck("acme/widgets", tc.confined, tc.detail, tc.err)
			if chk.ID != "github_ref_ruleset" {
				t.Fatalf("ID = %q", chk.ID)
			}
			if chk.Status == "fail" {
				t.Fatal("refRulesetCheck must never grade fail")
			}
			if chk.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (detail: %s)", chk.Status, tc.wantStatus, chk.Detail)
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(chk.Detail, want) {
					t.Fatalf("detail = %q, want it to contain %q", chk.Detail, want)
				}
			}
		})
	}
}

func githubPolicy(t *testing.T, repos ...string) types.RunPolicySpec {
	t.Helper()
	scope, err := json.Marshal(map[string]any{"repos": repos})
	if err != nil {
		t.Fatal(err)
	}
	return types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantGitHubToken, Scope: scope}}}
}

// The gating that keeps the ONE outbound call in /setup/status from costing
// anything: no App, no verifier, or no concrete repo => no call and no row.
func TestGithubRefRulesetCheck_GatedAndCached(t *testing.T) {
	ctx := context.Background()

	t.Run("no github app: no call, no row", func(t *testing.T) {
		v := &fakeRefRulesetVerifier{confined: true}
		s := New(Config{GitHubRulesets: v, DefaultPolicy: githubPolicy(t, "acme/widgets")})
		if _, ok := s.githubRefRulesetCheck(ctx, false); ok {
			t.Fatal("row must be omitted when no GitHub App is configured")
		}
		if v.calls != 0 {
			t.Fatalf("calls = %d, want 0", v.calls)
		}
	})

	t.Run("no verifier wired: no row", func(t *testing.T) {
		s := New(Config{DefaultPolicy: githubPolicy(t, "acme/widgets")})
		if _, ok := s.githubRefRulesetCheck(ctx, true); ok {
			t.Fatal("row must be omitted when no verifier is wired")
		}
	})

	t.Run("template policy names no repo: no call, no row", func(t *testing.T) {
		v := &fakeRefRulesetVerifier{confined: true}
		s := New(Config{GitHubRulesets: v, DefaultPolicy: githubPolicy(t)})
		if _, ok := s.githubRefRulesetCheck(ctx, true); ok {
			t.Fatal("row must be omitted when nothing names a concrete repo")
		}
		if v.calls != 0 {
			t.Fatalf("calls = %d, want 0 — nothing to probe", v.calls)
		}
	})

	t.Run("repo present: probes once, then serves the cache", func(t *testing.T) {
		v := &fakeRefRulesetVerifier{confined: true, detail: "acme/widgets: confined."}
		now := time.Now()
		s := New(Config{
			GitHubRulesets: v,
			DefaultPolicy:  githubPolicy(t, "acme/widgets"),
			Now:            func() time.Time { return now },
		})
		chk, ok := s.githubRefRulesetCheck(ctx, true)
		if !ok || chk.Status != "ok" {
			t.Fatalf("first probe: ok=%v status=%q", ok, chk.Status)
		}
		if v.lastRepo != "acme/widgets" {
			t.Fatalf("probed %q, want the repo named in the grant scope", v.lastRepo)
		}
		for range 5 {
			if _, ok := s.githubRefRulesetCheck(ctx, true); !ok {
				t.Fatal("cached row must still render")
			}
		}
		if v.calls != 1 {
			t.Fatalf("calls = %d, want 1 — polling /setup/status must not amplify the call", v.calls)
		}
		now = now.Add(refRulesetTTL + time.Second)
		if _, ok := s.githubRefRulesetCheck(ctx, true); !ok {
			t.Fatal("row must still render after the TTL")
		}
		if v.calls != 2 {
			t.Fatalf("calls = %d, want 2 — the cache must expire", v.calls)
		}
	})
}
