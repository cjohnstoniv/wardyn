// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// TestBrokeredCredentialsAreMaskRegistered (F100, F123) pins Requirement 8's
// proxy-side masking net over BOTH broker lanes at once.
//
// procRegistry is what Proxy.httpError and the decision-log sink (maskDecisionBytes)
// run every sandbox-visible error string and every emitted decision row through.
// Three of the four proxy-side credential sources fed it — the injector, the
// GitHub App lane, and the LLM-inspection secrets — and the git_pat lane, which
// mints a RAW operator PAT server-side on the same request path, did not: it
// re-implemented the mint instead of sharing the GitHub lane's, so the
// AddGlobal beside that mint never ran for it and a `glpat-…` passed
// maskDecisionBytes verbatim. The GitHub lane's own comment states the standard
// this holds both lanes to: "no path today" is not a property of the token, it
// is a property of the current call sites.
//
// The two subtests are deliberately the same assertion against the two lanes:
// the github_token case is the CONTROL that passed before and must keep passing,
// so a regression that unregisters both is not read as "the pin moved".
func TestBrokeredCredentialsAreMaskRegistered(t *testing.T) {
	// A decision row shaped like the ones both sinks actually carry, so this
	// asserts the real masking path rather than a bare registry lookup.
	row := func(secret string) []byte {
		return []byte(`{"rule_source":"brokered:git","error":"mint: ` + secret + `"}`)
	}

	t.Run("gitPAT", func(t *testing.T) {
		// Unique per lane and per run so the process-global registry another test
		// in this package populated can never make this pass by accident.
		const pat = "glpat-F123-ONLY-THIS-TEST-MINTS-THIS"
		if !bytes.Contains(maskDecisionBytes(row(pat)), []byte(pat)) {
			t.Fatal("precondition failed: this value is registered before the broker ever minted it")
		}

		up := newPATBrokerUpstream(t, pat, "oauth2")
		p, _ := newPATBrokerProxy(t,
			map[string]PATGrant{"gitlab.com": {GrantID: uuid.New()}}, upstreamAddr(up.srv))

		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet,
			"/wardyn/git/gitlab.com/org/repo.git/info/refs?service=git-upload-pack", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("clone status = %d body=%q, want 200 (the mint must actually have happened)", rec.Code, rec.Body.String())
		}

		if got := maskDecisionBytes(row(pat)); bytes.Contains(got, []byte(pat)) {
			t.Errorf("after a git_pat mint, maskDecisionBytes left the PAT verbatim: %s\n"+
				"the minted PAT is not in procRegistry, so every sandbox-facing httpError and every decision row "+
				"carrying it goes out unmasked — the GitHub lane registers its installation token for exactly this reason", got)
		}
	})

	t.Run("githubToken", func(t *testing.T) {
		const tok = "ghs_F100_ONLY_THIS_TEST_MINTS_THIS"
		if !bytes.Contains(maskDecisionBytes(row(tok)), []byte(tok)) {
			t.Fatal("precondition failed: this value is registered before the broker ever minted it")
		}

		up := newGitBrokerUpstream(t, tok)
		p, _ := newGitBrokerProxy(t,
			map[string]uuid.UUID{"org/repo": uuid.New()}, upstreamAddr(up.srv))

		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet,
			"/wardyn/gh/org/repo.git/info/refs?service=git-upload-pack", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("clone status = %d body=%q, want 200 (the mint must actually have happened)", rec.Code, rec.Body.String())
		}

		if got := maskDecisionBytes(row(tok)); bytes.Contains(got, []byte(tok)) {
			t.Errorf("after a github_token mint, maskDecisionBytes left the token verbatim: %s", got)
		}
	})
}
