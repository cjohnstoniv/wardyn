// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Review stays a dry check (it never spends a refresh token), and says so when
// the session it passed is expired: launch will renew it.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"
)

func TestCreateRenewal_ReviewSaysTheSignInWillBeRenewedAtLaunch(t *testing.T) {
	note := fmt.Sprintf(mpBRRenewAtLaunch, "bedrock-sso")
	live := createRenewalBlob()
	live.ExpiresAt = time.Now().Add(2 * time.Hour)
	for name, tc := range map[string]struct {
		blob awsSSOBlob
		want bool
	}{
		"expired, renewable": {createRenewalBlob(), true},
		"live":               {live, false},
	} {
		t.Run(name, func(t *testing.T) {
			srv := createRenewalFixture(t)
			storeSSOBlobFor(t, srv, createRenewalOwner, tc.blob)
			calls := renewedOIDC(t)
			w := do(t, srv, http.MethodPost, "/api/v1/runs/preflight", providerAdminToken(srv, createRenewalOwner), createRenewalBody)
			var resp preflightResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || w.Code != http.StatusOK {
				t.Fatalf("preflight = %d %s", w.Code, w.Body.String())
			}
			if got := slices.Contains(resp.Warnings, note); got != tc.want {
				t.Errorf("warnings = %q, want the renew-at-launch note: %v", resp.Warnings, tc.want)
			}
			if calls.Load() != 0 {
				t.Errorf("CreateToken calls = %d, want none: Review is a dry check", calls.Load())
			}
		})
	}
}
