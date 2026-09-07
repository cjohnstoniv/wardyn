// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// sandboxCredentialHeaders is what an in-sandbox client can put on a request
// Wardyn is about to inject an operator-brokered credential into. Every one of
// these is a credential header on some upstream Wardyn brokers for — see
// stripSandboxCredentials (inject.go) for the per-vendor citations — so if any
// of them survives, WHICH credential the upstream honours is the upstream's
// choice, not Wardyn's, while the decision row still reads as brokered egress.
var sandboxCredentialHeaders = map[string]string{
	"Authorization":        "Bearer SANDBOX-OWN-BEARER",
	"X-Api-Key":            "SANDBOX-OWN-XAPIKEY",
	"Api-Key":              "SANDBOX-OWN-APIKEY",
	"X-Auth-Token":         "SANDBOX-OWN-XAUTH",
	"Anthropic-Api-Key":    "SANDBOX-OWN-ANTHROPIC",
	"Cookie":               "session=SANDBOX-OWN-COOKIE",
	"Private-Token":        "SANDBOX-OWN-GITLAB-PAT",
	"X-Access-Token":       "SANDBOX-OWN-ACCESS",
	"X-Amz-Security-Token": "SANDBOX-OWN-AWS-SESSION",
	"X-Functions-Key":      "SANDBOX-OWN-AZURE-KEY",
	"X-Goog-Api-Key":       "SANDBOX-OWN-GOOGLE-KEY",
}

// assertNoSandboxCredentials fails if any sandbox-set credential VALUE reached
// the upstream. It compares values, not presence: the brokered credential is
// itself set under one of these names (Authorization on the broker lanes,
// X-Api-Key on the vendor lanes), and that one is the whole point.
// `where` names the injecting path under test.
func assertNoSandboxCredentials(t *testing.T, got http.Header, where string) {
	t.Helper()
	for name, value := range sandboxCredentialHeaders {
		if !slices.Contains(got.Values(name), value) {
			continue
		}
		t.Errorf("%s: the upstream received the SANDBOX's %s = %q alongside the brokered credential — "+
			"which credential wins is then the upstream's choice, not Wardyn's, and the audit row "+
			"still reads as brokered egress", where, name, value)
	}
}

// TestBrokerLanesStripSandboxCredentials pins the F104 fix-up: the two BROKER
// lanes strip the sandbox's credential headers through stripSandboxCredentials
// — the one definition inject.go's own comment says must not be re-spelled at a
// call site — rather than through a local Header.Del("Authorization").
//
// The narrower local spelling was live on both lanes: a clone with
// `Private-Token: <sandbox PAT>` reached GitLab carrying BOTH the brokered
// Basic auth and the sandbox's own first-class GitLab credential, on exactly
// the forge kind the PAT lane exists for.
func TestBrokerLanesStripSandboxCredentials(t *testing.T) {
	t.Run("git_pat lane", func(t *testing.T) {
		up := newPATBrokerUpstream(t, "T", "oauth2")
		p, _ := newPATBrokerProxy(t,
			map[string]PATGrant{"gitlab.com": {GrantID: uuid.New()}}, upstreamAddr(up.srv))

		req := mustLocalReq(t, http.MethodGet,
			"/wardyn/git/gitlab.com/org/repo.git/info/refs?service=git-upload-pack", nil)
		for name, value := range sandboxCredentialHeaders {
			req.Header.Set(name, value)
		}
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("info/refs status = %d body=%q", rec.Code, rec.Body.String())
		}
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:T"))
		if up.gitAuth != want {
			t.Fatalf("upstream Authorization = %q, want the brokered mint %q", up.gitAuth, want)
		}
		assertNoSandboxCredentials(t, up.gitHeaders, "the git_pat broker lane")
	})

	t.Run("github_token lane", func(t *testing.T) {
		up := newGitBrokerUpstream(t, "ghs-tok")
		grant := uuid.New()
		p, _ := newGitBrokerProxy(t, map[string]uuid.UUID{"org/repo": grant}, upstreamAddr(up.srv))

		req := mustLocalReq(t, http.MethodGet,
			"/wardyn/gh/org/repo.git/info/refs?service=git-upload-pack", nil)
		for name, value := range sandboxCredentialHeaders {
			req.Header.Set(name, value)
		}
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("info/refs status = %d body=%q", rec.Code, rec.Body.String())
		}
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte(gitBrokerUsername+":ghs-tok"))
		if up.gitAuth != want {
			t.Fatalf("upstream Authorization = %q, want the brokered installation token %q", up.gitAuth, want)
		}
		assertNoSandboxCredentials(t, up.gitHeaders, "the github_token broker lane")
	})
}
