// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestAWSSSORegion_HostShapeIsValidatedBeforeTheURLIsBuilt: the stored region
// is concatenated into `oidc.<region>.amazonaws.com`, and the only guard it had
// passed was repoFieldSafe (control characters and whitespace), which admits
// `/` and `@` — so a region of `x.attacker.com/` yields the host
// `oidc.x.attacker.com` and POSTs the client secret and the refresh token to it.
//
// Not sandbox-reachable today (the F006 binding pins an uploaded blob's region
// to the operator's own boot config), so this is defence in depth for a
// pre-0.7.2 blob, a direct store write, or an operator typo.
func TestAWSSSORegion_HostShapeIsValidatedBeforeTheURLIsBuilt(t *testing.T) {
	for region, want := range map[string]bool{
		"us-east-1":              true,
		"eu-central-1":           true,
		"ap-southeast-4":         true,
		"us-gov-west-1":          true,
		"us-iso-east-1":          true,
		"us-isob-east-1":         true,
		"x.attacker.com/":        false, // host takeover via a path separator
		"us-east-1.attacker.com": false,
		"us-east-1/../..":        false,
		"evil@attacker.com":      false, // userinfo takeover
		"US-EAST-1":              false,
		"us-east":                false,
		"":                       false,
	} {
		if got := awsSSORegionPattern.MatchString(region); got != want {
			t.Errorf("region %q accepted = %v, want %v", region, got, want)
		}
	}
}

// TestAWSSSORefresh_BadRegionFailsVisiblyWithoutARequest: the guard sits BEFORE
// the URL is composed, so a blob whose region is a hostname never produces a
// request at all — it takes the existing visible-failure path (transient, so
// the credential is left intact and never dead-marked).
func TestAWSSSORefresh_BadRegionFailsVisiblyWithoutARequest(t *testing.T) {
	calls := fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		w.WriteHeader(http.StatusOK)
	})
	s, _, blob := ssoRefreshServer(t)
	blob.Region = "x.attacker.com/"
	if err := s.storeAWSSSOBlob(context.Background(), awsSSOScope{}, blob); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Well past expiry, so the lane would certainly try to renew.
	out, sentence := s.refreshAWSSSOBlob(context.Background(), awsSSOScope{}, blob)
	if sentence == "" {
		t.Error("a blob whose region is not a region must fail the lane visibly")
	}
	if calls.Load() != 0 {
		t.Errorf("the refresh made %d request(s) to a composed host", calls.Load())
	}
	if out.AccessToken != blob.AccessToken {
		t.Error("a rejected region must leave the stored credential untouched")
	}
	// Sanity: the SAME blob with a real region does reach the endpoint. Stored,
	// not just passed in — refreshAWSSSOBlob re-reads through the scope inside
	// the owner lock, so the STORED region is the one that decides.
	blob.Region = "us-east-1"
	blob.ExpiresAt = awsSSOTestFixedNow.Add(-time.Minute)
	if err := s.storeAWSSSOBlob(context.Background(), awsSSOScope{}, blob); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	_, _ = s.refreshAWSSSOBlob(context.Background(), awsSSOScope{}, blob)
	if calls.Load() == 0 {
		t.Error("the fixture never reaches the endpoint at all — the pin proves nothing")
	}
}
