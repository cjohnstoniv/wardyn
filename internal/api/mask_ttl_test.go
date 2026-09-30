// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

// #151: every site that masks a short-lived access token process-wide bounds
// it by its expiry (secretmask.Registry.AddGlobalUntil), so a credential nobody
// refreshes again still lets go of it one sweep grace later. The refresh token
// and AWS's client secret carry no expiry of their own and stay.

// sweptAt sweeps reg's process-wide values at cutoff and returns the ones it let go.
func sweptAt(reg *secretmask.Registry, cutoff time.Time) []string {
	before := reg.Snapshot(uuid.Nil)
	reg.SweepGlobals(cutoff)
	after := reg.Snapshot(uuid.Nil)
	var gone []string
	for _, v := range before {
		if !slices.ContainsFunc(after, func(a []byte) bool { return bytes.Equal(a, v) }) {
			gone = append(gone, string(v))
		}
	}
	return gone
}

func masksValue(reg *secretmask.Registry, v string) bool {
	return slices.ContainsFunc(reg.Snapshot(uuid.Nil), func(b []byte) bool { return string(b) == v })
}

func TestADOMintSites_LetTheAccessTokenGoAfterItsExpiry(t *testing.T) {
	isAccess := func(f *adoFixture, v string) bool { _, ok := f.fake.ScopesForAccessToken(v); return ok }
	check := func(t *testing.T, f *adoFixture, expiry time.Time, refresh string) {
		t.Helper()
		reg := f.srv.cfg.MaskRegistry
		if gone := sweptAt(reg, expiry.Add(-time.Second)); len(gone) != 0 {
			t.Fatalf("a sweep before the access token's expiry let go of %d values", len(gone))
		}
		gone := sweptAt(reg, expiry.Add(time.Second))
		if len(gone) != 1 || !isAccess(f, gone[0]) {
			t.Fatalf("the sweep after the access token's expiry let go of %d values, want exactly the access token", len(gone))
		}
		if !masksValue(reg, refresh) {
			t.Fatal("the refresh token was let go with the access token")
		}
	}
	t.Run("sign-in callback", func(t *testing.T) {
		f := newADOFixture(t)
		if w := f.capture(t, f.fake.Subject()); w.Code != http.StatusFound {
			t.Fatalf("capture: status %d body %q", w.Code, w.Body.String())
		}
		blob, _ := f.stored(t, f.fake.Subject())
		check(t, f, blob.ExpiresAt, blob.RefreshToken)
	})
	t.Run("sign-in callback whose store write is lost", func(t *testing.T) {
		f := newADOFixture(t)
		f.srv.cfg.Secrets = lostWriteSecrets{f.srv.cfg.Secrets.(*memSecrets)}
		// #659 Q2: redirects to the console's error path now, not a bare 500.
		if w := f.capture(t, f.fake.Subject()); w.Code != http.StatusFound {
			t.Fatalf("capture: status %d body %q, want a 302 back to the console", w.Code, w.Body.String())
		}
		snap := f.srv.cfg.MaskRegistry.Snapshot(uuid.Nil)
		i := slices.IndexFunc(snap, func(v []byte) bool { return !isAccess(f, string(v)) })
		if i < 0 {
			t.Fatal("the exchanged refresh token was never masked")
		}
		check(t, f, adoTestNow.Add(time.Hour), string(snap[i]))
	})
	t.Run("redemption", func(t *testing.T) {
		f := newADOFixture(t)
		subject := f.fake.Subject()
		if w := f.capture(t, subject); w.Code != http.StatusFound {
			t.Fatalf("capture: status %d body %q", w.Code, w.Body.String())
		}
		access, err := f.srv.RedeemADOEntraAccess(context.Background(), f.cfg, subject, f.cfg.Scopes)
		if err != nil {
			t.Fatalf("redeem: %v", err)
		}
		blob, _ := f.stored(t, subject)
		// Checked by value: the redemption also retired the capture's access
		// token (at its expiry) and the refresh token it rotated away (at once).
		reg := f.srv.cfg.MaskRegistry
		reg.SweepGlobals(access.ExpiresAt.Add(-time.Second))
		if !masksValue(reg, access.AccessToken) {
			t.Fatal("a sweep before the redeemed access token's expiry let go of it")
		}
		reg.SweepGlobals(access.ExpiresAt.Add(time.Second))
		if masksValue(reg, access.AccessToken) {
			t.Fatal("the redeemed access token is still held after a sweep past its expiry")
		}
		if !masksValue(reg, blob.RefreshToken) {
			t.Fatal("the refresh token was let go with the access token")
		}
	})
}

// A re-redemption retires the access token the injection cache may still be
// serving (adoEntraAccessCache, until shortly before its expiry). That token
// stays masked for its own expiry plus grace, not one grace after the
// re-redemption. One clock, the server's, drives the redemptions and the sweep.
func TestADORedeem_AReplacedAccessTokenStaysMaskedUntilItsExpiryPlusGrace(t *testing.T) {
	f := newADOFixture(t)
	base := time.Now()
	clock := base
	f.srv.cfg.Now = func() time.Time { return clock }
	subject := f.fake.Subject()
	if w := f.capture(t, subject); w.Code != http.StatusFound {
		t.Fatalf("capture: status %d body %q", w.Code, w.Body.String())
	}
	first, err := f.srv.RedeemADOEntraAccess(context.Background(), f.cfg, subject, f.cfg.Scopes)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	clock = base.Add(5 * time.Minute)
	second, err := f.srv.RedeemADOEntraAccess(context.Background(), f.cfg, subject, f.cfg.Scopes)
	if err != nil {
		t.Fatalf("re-redeem: %v", err)
	}
	if second.AccessToken == first.AccessToken {
		t.Fatal("the re-redemption returned the same access token; nothing was replaced")
	}

	clock = base.Add(5*time.Minute + RunSecretGrace + time.Minute)
	f.srv.SweepRunSecrets(context.Background())
	if !masksValue(f.srv.cfg.MaskRegistry, first.AccessToken) {
		t.Fatal("the replaced access token was let go one grace after the re-redemption, before its own expiry plus grace")
	}
	clock = first.ExpiresAt.Add(RunSecretGrace + time.Minute)
	f.srv.SweepRunSecrets(context.Background())
	if masksValue(f.srv.cfg.MaskRegistry, first.AccessToken) {
		t.Error("the replaced access token is still held after its expiry plus grace")
	}
}

func TestAWSSSOMintSites_LetTheAccessTokenGoAfterItsExpiry(t *testing.T) {
	check := func(t *testing.T, reg *secretmask.Registry, expiry time.Time, access string, lasting ...string) {
		t.Helper()
		if gone := sweptAt(reg, expiry.Add(-time.Second)); len(gone) != 0 {
			t.Fatalf("a sweep before the access token's expiry let go of %d values", len(gone))
		}
		if gone := sweptAt(reg, expiry.Add(time.Second)); !slices.Equal(gone, []string{access}) {
			t.Fatalf("the sweep after the access token's expiry let go of %d values, want exactly the access token", len(gone))
		}
		for _, v := range lasting {
			if !masksValue(reg, v) {
				t.Errorf("a value with no expiry of its own (%d bytes) was let go with the access token", len(v))
			}
		}
	}
	t.Run("renewal", func(t *testing.T) {
		s, _, blob := ssoRefreshServer(t)
		fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"accessToken": "fresh-access-token-abcdefghij", "expiresIn": 3600,
				"refreshToken": "rotated-refresh-token-abcdefghij",
			})
		})
		if _, msg := ssoDispatch(s); msg != "" {
			t.Fatalf("the renewed SSO lane is not ready: %s", msg)
		}
		check(t, s.cfg.MaskRegistry, awsSSOTestFixedNow.Add(time.Hour), "fresh-access-token-abcdefghij",
			"rotated-refresh-token-abcdefghij", blob.ClientSecret)
	})
	t.Run("dispatch of a still-valid token", func(t *testing.T) {
		s, _, _ := ssoRefreshServer(t)
		expiry := awsSSOTestFixedNow.Add(30 * time.Minute)
		blob := putAWSSSOBlob(t, s, expiry)
		if _, msg := ssoDispatch(s); msg != "" {
			t.Fatalf("the SSO lane with a valid token is not ready: %s", msg)
		}
		check(t, s.cfg.MaskRegistry, expiry, blob.AccessToken, blob.RefreshToken, blob.ClientSecret)
	})
}

// TestAWSSSORefresh_ProviderMaskKeyDoesNotClobberAnotherProvider pins
// 1b61b04ce: a provider-scoped refresh must key its mask-set entry on
// scope.ssoSecret() (wardyn-provider-<uid>-sso), never on a name another
// credential of the SAME person shares. A shared key would collide
// (AddGlobalUntil replaces a key's whole current set) and retire the other
// provider's live refresh token and client secret at once — SweepGlobals then
// drops them, unmasking a credential still in use.
func TestAWSSSORefresh_ProviderMaskKeyDoesNotClobberAnotherProvider(t *testing.T) {
	ctx := context.Background()
	reg := secretmask.NewRegistry()
	s := &Server{cfg: Config{
		Secrets: &memSecrets{}, MaskRegistry: reg, Now: func() time.Time { return awsSSOTestFixedNow }, Audit: &memAudit{},
	}}
	const owner = "alice@example.com"
	otherScope := awsSSOScope{perUser: true, owner: owner, provider: uuid.NewString()}
	providerScope := awsSSOScope{perUser: true, owner: owner, provider: uuid.NewString()}

	// The other provider's OWN mask entries, as its own earlier capture
	// registered them: a live refresh token and client secret with no expiry of
	// their own, and an access token expiring far in the future.
	otherAccess, otherRefresh, otherSecret := "other-access-token-12345678901", "other-refresh-token-12345678901", "other-client-secret-12345678901"
	otherExpiry := awsSSOTestFixedNow.Add(24 * time.Hour)
	reg.AddGlobalUntil(otherScope.rowOwner(), otherScope.ssoSecret(), awsSSOTestFixedNow, otherExpiry,
		[]byte(otherAccess), []byte(otherRefresh), []byte(otherSecret))

	// An expired provider-scoped blob due for renewal.
	blob := awsSSOBlob{
		AccessToken: "provider-access-token-123456789", RefreshToken: "provider-refresh-token-123456789",
		ClientID: "provider-client-id", ClientSecret: "provider-client-secret-1234567890",
		StartURL: "https://acme.awsapps.com/start", Region: "us-east-1",
		AccountID: "123456789012", RoleName: "WardynBedrockRole",
		ExpiresAt: awsSSOTestFixedNow.Add(-time.Minute), RegistrationExpiresAt: awsSSOTestFixedNow.Add(90 * 24 * time.Hour),
	}
	if err := s.storeAWSSSOBlob(ctx, providerScope, blob); err != nil {
		t.Fatalf("seed provider row: %v", err)
	}
	fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": "fresh-provider-access-token-abcd", "expiresIn": 3600,
			"refreshToken": "rotated-provider-refresh-token-ab",
		})
	})

	next, failure := s.refreshAWSSSOBlob(ctx, providerScope, blob)
	if failure != "" || next.AccessToken != "fresh-provider-access-token-abcd" {
		t.Fatalf("refresh failure=%q next=%+v; want a clean renewal", failure, next)
	}

	// The provider's OWN new values are masked under the provider key.
	if !masksValue(reg, next.AccessToken) || !masksValue(reg, next.RefreshToken) {
		t.Fatal("the provider's refreshed values were not masked")
	}

	// A sweep shortly after the refresh, well before the other access token's
	// own (far-future) expiry, must not touch ANY of the other provider's values —
	// keying both AddGlobalUntil calls on one name would have
	// retired them all at the refresh, and this sweep would drop them.
	reg.SweepGlobals(awsSSOTestFixedNow.Add(time.Second))
	if !masksValue(reg, otherAccess) || !masksValue(reg, otherRefresh) || !masksValue(reg, otherSecret) {
		t.Fatal("a provider-scoped refresh unmasked another provider's still-current values")
	}
}
