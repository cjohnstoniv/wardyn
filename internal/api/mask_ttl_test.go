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
		if w := f.capture(t, f.fake.Subject()); w.Code != http.StatusInternalServerError {
			t.Fatalf("capture: status %d body %q, want 500", w.Code, w.Body.String())
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
		// The capture's access token was retired by the redemption, on the wall
		// clock, so this fixed-clock sweep leaves it; only the redeemed one goes.
		check(t, f, access.ExpiresAt, blob.RefreshToken)
	})
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
		if ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true, true, nil, awsSSOScope{}); !ba.ready {
			t.Fatal("the renewed SSO lane is not ready")
		}
		check(t, s.cfg.MaskRegistry, awsSSOTestFixedNow.Add(time.Hour), "fresh-access-token-abcdefghij",
			"rotated-refresh-token-abcdefghij", blob.ClientSecret)
	})
	t.Run("dispatch of a still-valid token", func(t *testing.T) {
		s, _, _ := ssoRefreshServer(t)
		expiry := awsSSOTestFixedNow.Add(30 * time.Minute)
		blob := putAWSSSOBlob(t, s, expiry)
		if ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true, false, nil, awsSSOScope{}); !ba.ready {
			t.Fatal("the SSO lane with a valid token is not ready")
		}
		check(t, s.cfg.MaskRegistry, expiry, blob.AccessToken, blob.RefreshToken, blob.ClientSecret)
	})
}
