// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// TestAccessStaleSnapshotNamesTheCallerRemedy is F213.
//
// The stale-snapshot guard fires on any caller whose frozen claim snapshot
// cannot reproduce the admin role they hold, and apiTokenAuth installs exactly
// such a snapshot: api_tokens.groups is stamped at MINT and read verbatim, and a
// NULL groups_truncated (a pre-0.7 token) reads as truncated by PF-26. So a
// wdn_-token admin was refused every POST/DELETE /access/mappings and told to
// "sign in again" — which changes nothing they hold. RefreshAPITokenRoles
// re-stamps the ROLE column and provably does not touch groups, so no sign-in
// clears it: the only exit is re-minting the token, and the refusal never said
// so.
//
// The guard already distinguishes lanes once (the admin-token/local-mode
// exemption), so this pins the same distinction applied to the REMEDY. The
// refusal itself is unchanged in both lanes.
func TestAccessStaleSnapshotNamesTheCallerRemedy(t *testing.T) {
	auth := newAccessAuth(t, map[string]string{"chart-admin": oidc.RoleAdmin}, "", nil, nil)
	srv := accessServer(t, auth, nil)

	// The exact shape apitokens.go installs: an admin whose frozen group
	// snapshot is nil and whose truncation bit reads true.
	tokenCtx := withAPITokenID(
		withOIDCGroupsTruncated(
			withOIDCGroups(operatorCtx("sub-admin", "admin@corp.example", oidc.RoleAdmin), nil), true),
		uuid.New())
	cookieCtx := withOIDCGroupsTruncated(
		withOIDCGroups(operatorCtx("sub-admin", "admin@corp.example", oidc.RoleAdmin), nil), true)

	for _, tc := range []struct {
		name     string
		req      func() *httptest.ResponseRecorder
		ctxLabel string
		want     string
		notWant  string
	}{
		{
			name:     "the api-token lane",
			ctxLabel: "token",
			want:     "re-mint the token",
			notWant:  "sign in again before changing role mappings",
		},
		{
			name:     "the cookie lane",
			ctxLabel: "cookie",
			want:     "sign in again before changing role mappings",
			notWant:  "re-mint the token",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/v1/access/mappings", nil)
			if tc.ctxLabel == "token" {
				r = r.WithContext(tokenCtx)
			} else {
				r = r.WithContext(cookieCtx)
			}
			err := srv.accessLockoutErr(r, nil, nil)
			if err == nil {
				t.Fatalf("%s: no refusal — a snapshot too stale to verify a no-op write is too stale to verify a "+
					"real demotion either, so this must still refuse", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s refusal = %q, want it to name %q — a refusal whose only named remedy provably does "+
					"not work is a dead end", tc.name, err.Error(), tc.want)
			}
			if strings.Contains(err.Error(), tc.notWant) {
				t.Errorf("%s refusal = %q, must not name the OTHER lane's remedy", tc.name, err.Error())
			}
		})
	}

	// The control that keeps the lane split from becoming a bypass: a token
	// caller whose snapshot DOES carry the admin group is allowed, exactly as
	// the cookie lane is. The split is about the sentence, never the decision.
	t.Run("a token lane with a live admin group is still allowed", func(t *testing.T) {
		ctx := withAPITokenID(
			withOIDCGroupsTruncated(
				withOIDCGroups(operatorCtx("sub-admin", "admin@corp.example", oidc.RoleAdmin),
					[]string{"chart-admin"}), false),
			uuid.New())
		r := httptest.NewRequest("POST", "/api/v1/access/mappings", nil).WithContext(ctx)
		if err := srv.accessLockoutErr(r, nil, []oidc.RoleMapping{{Value: "chart-admin", Role: oidc.RoleAdmin}}); err != nil {
			t.Errorf("a token caller whose snapshot reproduces admin was refused: %v — the lane split changes the "+
				"remedy, never the decision", err)
		}
	})
}
