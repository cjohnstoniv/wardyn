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
// The stale-snapshot guard fires on any caller whose stamped claim snapshot
// cannot reproduce the admin role they hold, and apiTokenAuth installs exactly
// such a snapshot: api_tokens.groups is stamped at MINT and on every OnLogin
// (store.RefreshAPITokenIdentity) and read verbatim, and a NULL
// groups_truncated (a pre-0.7 token) reads as truncated by PF-26. A
// wdn_-token admin is refused every POST/DELETE /access/mappings until the
// stamp catches up — but unlike before #152, the owner's own next sign-in now
// re-stamps role AND groups together, so "sign in again" is a real remedy for
// this lane too. It is just not the SAME remedy as the cookie lane's: the
// token-authenticated request itself cannot sign in, only its owner can,
// separately, after which retrying the write succeeds. Re-minting the token
// remains the fallback for an owner who cannot or will not sign in again.
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
			err := srv.accessLockoutErr(r, nil, nil, nil)
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
		if err := srv.accessLockoutErr(r, nil, []oidc.RoleMapping{{Value: "chart-admin", Role: oidc.RoleAdmin}}, nil); err != nil {
			t.Errorf("a token caller whose snapshot reproduces admin was refused: %v — the lane split changes the "+
				"remedy, never the decision", err)
		}
	})
}
