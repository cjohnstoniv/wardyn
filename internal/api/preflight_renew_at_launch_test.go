// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Review's preflight renews an expired, renewable session as launch does, so it
// no longer says the sign-in will be renewed at launch; a live session is not renewed.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"
)

func TestCreateRenewal_ReviewRenewsAnExpiredSignIn(t *testing.T) {
	note := fmt.Sprintf(mpBRRenewAtLaunch, "bedrock-sso")
	live := createRenewalBlob()
	live.ExpiresAt = time.Now().Add(2 * time.Hour)
	for name, tc := range map[string]struct {
		blob  awsSSOBlob
		calls int32
	}{
		"expired, renewable": {createRenewalBlob(), 1},
		"live":               {live, 0},
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
			if slices.Contains(resp.Warnings, note) {
				t.Errorf("warnings = %q, want no renew-at-launch note: Review renewed it", resp.Warnings)
			}
			if calls.Load() != tc.calls {
				t.Errorf("CreateToken calls = %d, want %d", calls.Load(), tc.calls)
			}
		})
	}
}
