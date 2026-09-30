// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"testing"
	"time"
)

// TestAWSSSOCredentialState_TheStates walks the vocabulary, REGISTRATION
// first. The ordering is the point: the access token lives an hour on the
// reporting estate, so a probe keyed on it would make `expiring` permanent and
// `live` unreachable.
func TestAWSSSOCredentialState_TheStates(t *testing.T) {
	now := awsSSOTestFixedNow
	live := func(mut func(*awsSSOBlob)) awsSSOBlob {
		b := awsSSOBlob{
			AccessToken: "a", RefreshToken: "r", StartURL: "https://x.awsapps.com/start",
			Region: "us-east-1", AccountID: "1", RoleName: "R",
			ExpiresAt:             now.Add(time.Hour),
			RegistrationExpiresAt: now.Add(90 * 24 * time.Hour),
		}
		if mut != nil {
			mut(&b)
		}
		return b
	}
	cases := []struct {
		name  string
		blob  awsSSOBlob
		found bool
		spent bool // every row here is !spent — see TestAWSSSOCredentialState_SpentBoundary
		want  string
	}{
		{"renewable with a long registration", live(nil), true, false, modelAccessLive},
		{"expired ACCESS token but renewable folds into live",
			live(func(b *awsSSOBlob) { b.ExpiresAt = now.Add(-time.Minute) }), true, false, modelAccessLive},
		{"a zero registration timestamp is live (the helper saw none)",
			live(func(b *awsSSOBlob) { b.RegistrationExpiresAt = time.Time{} }), true, false, modelAccessLive},
		{"the registration lapses within a day",
			live(func(b *awsSSOBlob) { b.RegistrationExpiresAt = now.Add(6 * time.Hour) }), true, false, modelAccessExpiring},
		{"no refresh token, access token within a day",
			live(func(b *awsSSOBlob) { b.RefreshToken = ""; b.ExpiresAt = now.Add(2 * time.Hour) }), true, false, modelAccessExpiring},
		{"no refresh token, access token expired",
			live(func(b *awsSSOBlob) { b.RefreshToken = ""; b.ExpiresAt = now.Add(-time.Minute) }), true, false, modelAccessExpiredSignin},
		{"a lapsed registration cannot be renewed",
			live(func(b *awsSSOBlob) {
				b.RegistrationExpiresAt = now.Add(-time.Hour)
				b.ExpiresAt = now.Add(-time.Minute)
			}), true, false, modelAccessExpiredSignin},
		{"nothing captured", awsSSOBlob{}, false, false, modelAccessNotConfigured},
	}
	for _, c := range cases {
		if got := awsSSOCredentialState(c.blob, c.found, c.spent, now); got != c.want {
			t.Errorf("%s: state = %q, want %q", c.name, got, c.want)
		}
	}

	// Every state that asks for something must SAY what — an unactionable
	// warning chip is the thing this replaced.
	for _, st := range []string{modelAccessExpiring, modelAccessExpiredSignin, modelAccessNotConfigured} {
		if modelAccessAction(st, "2026-01-01T00:00:00Z") == "" {
			t.Errorf("state %q carries no action line", st)
		}
	}
	if modelAccessAction(modelAccessLive, "") != "" {
		t.Error("live must ask for nothing — that is what folding expired_renewable into it is for")
	}
	if modelAccessAction(modelAccessNotApplicable, "") != "" {
		t.Error("not_applicable must ask for nothing — the caller is a mechanism, not a person")
	}
	// The deadline the expiring line names is the REGISTRATION's while the blob
	// can be renewed: the access token's own expiry is not what runs out.
	reg := live(func(b *awsSSOBlob) { b.RegistrationExpiresAt = now.Add(6 * time.Hour) })
	if got := modelAccessDeadline(reg, true, false, now); got != reg.RegistrationExpiresAt.UTC().Format(time.RFC3339) {
		t.Errorf("deadline = %q, want the registration's lapse", got)
	}
}

// TestAWSSSOCredentialState_FixtureBlobReadsLive pins the bug the review
// found: ssoBlobFor/ssoBlobBody carry no refresh_token, so their blob's state
// depends entirely on ExpiresAt clearing modelAccessExpiringWindow (24h,
// see awssoCredentialState's default arm). A fixture minted only
// testutil.FutureRFC3339(24) ahead is exactly AT that window and grades
// `expiring`, not `live` — the fixture READ "not yet expired", not "live",
// which is why every AWS SSO blob site now mints 24*30 hours out (see
// internal/testutil/clock.go's doc comment). This parses the real blob JSON
// ssoBlobFor produces, the same way production code would.
func TestAWSSSOCredentialState_FixtureBlobReadsLive(t *testing.T) {
	var b awsSSOBlob
	if err := json.Unmarshal([]byte(ssoBlobFor("123456789012", "WardynBedrockRole")), &b); err != nil {
		t.Fatalf("unmarshal fixture blob: %v", err)
	}
	if got := awsSSOCredentialState(b, true, false, time.Now()); got != modelAccessLive {
		t.Errorf("state = %q, want %q — the fixture must clear modelAccessExpiringWindow", got, modelAccessLive)
	}
}

// TestAWSSSOCredentialState_SpentBoundary: a spent refresh token must grade
// dead once the access token is inside the refresh skew (dispatch would
// already refuse this run), and `expiring` — never `live` — while it is still
// comfortably outside it. Grading it `live` until the client registration
// lapses would show a live session for days while every dispatch refuses the
// person's runs, and grading `expiring` inside the skew would promise a
// launch dispatch does not honour, so the boundary is needsRefresh(now), not
// the registration.
func TestAWSSSOCredentialState_SpentBoundary(t *testing.T) {
	now := awsSSOTestFixedNow
	base := func(mut func(*awsSSOBlob)) awsSSOBlob {
		b := awsSSOBlob{
			AccessToken: "a", RefreshToken: "r", StartURL: "https://x.awsapps.com/start",
			Region: "us-east-1", AccountID: "1", RoleName: "R",
			ExpiresAt:             now.Add(time.Hour),
			RegistrationExpiresAt: now.Add(90 * 24 * time.Hour),
		}
		if mut != nil {
			mut(&b)
		}
		return b
	}
	cases := []struct {
		name string
		blob awsSSOBlob
		want string
	}{
		// The boundary table: just before / exactly at / just after ExpiresAt-skew.
		{"just before the skew boundary: still outside it, expiring",
			base(func(b *awsSSOBlob) { b.ExpiresAt = now.Add(awsSSORefreshSkew + time.Second) }), modelAccessExpiring},
		{"exactly at the skew boundary: needsRefresh fires, dead",
			base(func(b *awsSSOBlob) { b.ExpiresAt = now.Add(awsSSORefreshSkew) }), modelAccessExpiredSignin},
		{"just after (already inside the skew): dead",
			base(func(b *awsSSOBlob) { b.ExpiresAt = now.Add(awsSSORefreshSkew - time.Second) }), modelAccessExpiredSignin},
		// Nominal expiry: the access token has already lapsed outright.
		{"nominal expiry: already lapsed, dead",
			base(func(b *awsSSOBlob) { b.ExpiresAt = now.Add(-time.Minute) }), modelAccessExpiredSignin},
	}
	for _, c := range cases {
		if got := awsSSOCredentialState(c.blob, true, true, now); got != c.want {
			t.Errorf("%s: state = %q, want %q", c.name, got, c.want)
		}
	}

	// spent SKIPS the renewable arm entirely: a lapsed registration changes
	// nothing for a spent credential — only needsRefresh decides — which a
	// !spent blob with the SAME shape would grade differently (renewable's own
	// registrationLapsed check never fires because renewable() is false, so it
	// falls through to the plain-expiry arms and reads live, since the access
	// token has 48h of its own headroom left).
	lapsedRegBlob := base(func(b *awsSSOBlob) {
		b.ExpiresAt = now.Add(48 * time.Hour)
		b.RegistrationExpiresAt = now.Add(-time.Hour)
	})
	if got := awsSSOCredentialState(lapsedRegBlob, true, true, now); got != modelAccessExpiring {
		t.Errorf("spent + lapsed registration + 48h access-token headroom = %q, want expiring (needsRefresh alone decides)", got)
	}
	if got := awsSSOCredentialState(lapsedRegBlob, true, false, now); got != modelAccessLive {
		t.Errorf("sanity: the SAME blob !spent should read live (unchanged behaviour), got %q", got)
	}

	// The transient-refresh-with-a-usable-token path is UNCHANGED: !spent with a
	// token near expiry but still renewable and far from its registration lapse
	// folds into `live` exactly as before — dispatch renews it, so a near
	// expiry here is not yet the person's problem. This is the same fixture the
	// spent boundary cases above grade `expiring`/`dead`, which is the whole
	// point of the boundary: `spent` is what turns "renewable" off.
	transient := base(func(b *awsSSOBlob) { b.ExpiresAt = now.Add(time.Minute) })
	if got := awsSSOCredentialState(transient, true, false, now); got != modelAccessLive {
		t.Errorf("transient (!spent) near-expiry state = %q, want live (renewable, registration far out — unchanged)", got)
	}

	// modelAccessDeadline: the spent arm returns ExpiresAt-skew explicitly,
	// never the registration's lapse a spent credential can no longer redeem.
	spentBlob := base(func(b *awsSSOBlob) { b.ExpiresAt = now.Add(20 * time.Minute) })
	wantDeadline := spentBlob.ExpiresAt.Add(-awsSSORefreshSkew).UTC().Format(time.RFC3339)
	if got := modelAccessDeadline(spentBlob, true, true, now); got != wantDeadline {
		t.Errorf("spent deadline = %q, want ExpiresAt-skew %q", got, wantDeadline)
	}
	if got := modelAccessDeadline(spentBlob, true, false, now); got != spentBlob.RegistrationExpiresAt.UTC().Format(time.RFC3339) {
		t.Errorf("non-spent deadline changed: got %q, want the registration's own lapse (unaffected by this lane)", got)
	}
}
