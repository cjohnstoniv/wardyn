// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const pushWithheldSentence = "don't include code_write for "

// A run whose capabilities withhold GitHub pushes is refused at both doors of a
// push, before any mint or forge call, in its own words and rule source. Fetch
// and clone still pass. The host key is matched case-insensitively.
func TestGitBrokerRefusesWithheldPush(t *testing.T) {
	const repo = "/wardyn/gh/octocat/Hello-World.git"
	for _, tc := range []struct {
		name, method, path string
		refused            bool
	}{
		{"push advertisement", http.MethodGet, repo + "/info/refs?service=git-receive-pack", true},
		{"push", http.MethodPost, repo + "/git-receive-pack", true},
		{"fetch advertisement", http.MethodGet, repo + "/info/refs?service=git-upload-pack", false},
		{"fetch", http.MethodPost, repo + "/git-upload-pack", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newGitBrokerUpstream(t, "gh-inst-token")
			p, sink := newGitBrokerProxy(t, map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv))
			p.gitPushWithheld = canonicalHostKeys(map[string]string{"GitHub.com": "GitHub"})
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, tc.method, tc.path, strings.NewReader("0000")))

			if !tc.refused {
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d body=%q, want 200: a withheld push still fetches", rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), pushWithheldSentence+"GitHub, so this run can't push") {
				t.Fatalf("status = %d body=%q, want 403 with the capability sentence", rec.Code, rec.Body.String())
			}
			if up.gitHits != 0 || up.mintCalls != 0 {
				t.Fatalf("forge saw %d request(s) and %d mint(s), want none of either", up.gitHits, up.mintCalls)
			}
			if d := lastDecision(t, sink); d.Decision != egress.Deny || d.RuleSource != ruleSourceGitPushCapability {
				t.Fatalf("decision = %v / %q, want a deny from %q", d.Decision, d.RuleSource, ruleSourceGitPushCapability)
			}
		})
	}
}

// No withheld host: a push takes the path it always took.
func TestGitBrokerPushUnwithheldIsUnchanged(t *testing.T) {
	up := newGitBrokerUpstream(t, "gh-inst-token")
	p, _ := newGitBrokerProxyWithSpec(t, map[string]uuid.UUID{"octocat/hello-world": uuid.New()},
		upstreamAddr(up.srv), types.RunPolicySpec{})
	p.gitPushWithheld = canonicalHostKeys(map[string]string{"dev.azure.com": "Azure DevOps"})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, "/wardyn/gh/octocat/hello-world/info/refs?service=git-receive-pack", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q, want 200", rec.Code, rec.Body.String())
	}
}

// The git_pat lane refuses a withheld Azure DevOps push the same way, ahead of
// the grant's own read-only axis, and still fetches.
func TestPATBrokerRefusesWithheldPush(t *testing.T) {
	const repo = "/wardyn/git/dev.azure.com/org/proj/_git/app"
	for _, tc := range []struct {
		name, method, path string
		refused            bool
	}{
		{"push advertisement", http.MethodGet, repo + "/info/refs?service=git-receive-pack", true},
		{"push", http.MethodPost, repo + "/git-receive-pack", true},
		{"fetch", http.MethodPost, repo + "/git-upload-pack", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, up, last := patScopeProxy(t, "dev.azure.com", PATGrant{Access: types.PATAccessRead})
			p.gitPushWithheld = canonicalHostKeys(map[string]string{"dev.azure.com": "Azure DevOps"})
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, tc.method, tc.path, strings.NewReader("0000")))

			if !tc.refused {
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d body=%q, want 200", rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), pushWithheldSentence+"Azure DevOps") {
				t.Fatalf("status = %d body=%q, want 403 with the capability sentence", rec.Code, rec.Body.String())
			}
			if up.gitHits != 0 || up.mintCalls != 0 {
				t.Fatalf("forge saw %d request(s) and %d mint(s), want none of either", up.gitHits, up.mintCalls)
			}
			if d := last(); d.RuleSource != ruleSourceGitPushCapability || d.Decision != egress.Deny {
				t.Fatalf("decision = %v / %q, want a deny from %q", d.Decision, d.RuleSource, ruleSourceGitPushCapability)
			}
		})
	}
}

// The flag reaches the broker from the proxy's options, as the sidecar builds it.
func TestGitPushWithheldOptionReachesTheBroker(t *testing.T) {
	p := newProxy(Options{RunID: uuid.New(), GitPushWithheld: map[string]string{" GitHub.com ": "GitHub"}})
	if got := p.gitPushWithheld["github.com"]; got != "GitHub" {
		t.Fatalf("gitPushWithheld = %v, want github.com -> GitHub", p.gitPushWithheld)
	}
}
