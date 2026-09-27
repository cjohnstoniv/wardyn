// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestAdminDeleteSSHKeys_ResolvesPrincipalAndAudits(t *testing.T) {
	for _, target := range []string{"sub-alice", "alice@example.com", "ALICE@EXAMPLE.COM"} {
		t.Run(target, func(t *testing.T) {
			st := sshOffboardingStore()
			srv, cutoffs, audit := sshOffboardingServer(t, st)
			admin := ssoSession(t, "responder", "responder@example.com", oidc.RoleSecurityAdmin)
			w := doSSO(t, srv, http.MethodDelete, "/api/v1/people/"+url.PathEscape(target)+"/ssh-keys", admin, "")
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			var result struct {
				Count int `json:"count"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Count != 2 || len(st.keys) != 1 || st.keys[0].Principal != "sub-bob" {
				t.Fatalf("count=%d remaining=%v", result.Count, st.keys)
			}
			if len(cutoffs.revokedSubs)+cutoffs.revokedAll != 0 || len(st.revoked) != 0 {
				t.Fatal("SSH-only removal revoked other credentials")
			}
			assertSSHDeletionAudit(t, audit, "sub-alice", "success", 2)
		})
	}
}

func TestAdminDeleteSSHKeys_RequiresSecurityTier(t *testing.T) {
	for _, role := range []string{oidc.RoleAdmin, oidc.RoleSecurityAdmin, oidc.RoleUser, ""} {
		t.Run(role, func(t *testing.T) {
			st := sshOffboardingStore()
			srv, _, _ := sshOffboardingServer(t, st)
			caller := ssoSession(t, "responder", "responder@example.com", role)
			want := http.StatusOK
			if role == oidc.RoleUser {
				want = http.StatusForbidden
			}
			if role == "" {
				caller = nil
				want = http.StatusUnauthorized
			}
			w := doSSO(t, srv, http.MethodDelete, "/api/v1/people/sub-alice/ssh-keys", caller, "")
			if w.Code != want {
				t.Fatalf("status=%d want=%d body=%s", w.Code, want, w.Body.String())
			}
			if want != http.StatusOK && len(st.keys) != 3 {
				t.Fatal("refused caller deleted SSH keys")
			}
		})
	}
}

func TestAdminDeleteSSHKeys_ExactSSHPrincipalWinsOverEmailAlias(t *testing.T) {
	st := sshOffboardingStore()
	st.keys = append(st.keys, types.SSHPublicKey{Fingerprint: "email-owner", Principal: "alice@example.com"})
	srv, _, audit := sshOffboardingServer(t, st)
	admin := ssoSession(t, "responder", "responder@example.com", oidc.RoleAdmin)
	w := doSSO(t, srv, http.MethodDelete, "/api/v1/people/alice@example.com/ssh-keys", admin, "")
	if w.Code != http.StatusOK || len(st.keys) != 3 {
		t.Fatalf("status=%d keys=%+v body=%s", w.Code, st.keys, w.Body.String())
	}
	assertSSHDeletionAudit(t, audit, "alice@example.com", "success", 1)
}

func TestAdminDeleteSSHKeys_RefusesUnresolvedAmbiguousOrUnreadablePrincipal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*sessionSSHStore)
		target  string
		want    int
	}{
		{"unresolved", func(*sessionSSHStore) {}, "nobody@example.com", http.StatusUnprocessableEntity},
		{"ambiguous", func(st *sessionSSHStore) {
			st.toks = append(st.toks, types.APIToken{Principal: "other-alice", Email: "alice@example.com"})
		}, "alice@example.com", http.StatusUnprocessableEntity},
		{"unreadable", func(st *sessionSSHStore) { st.directoryErr = errors.New("directory unavailable") }, "alice@example.com", http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := sshOffboardingStore()
			tc.prepare(st)
			srv, _, audit := sshOffboardingServer(t, st)
			admin := ssoSession(t, "responder", "responder@example.com", oidc.RoleSecurityAdmin)
			w := doSSO(t, srv, http.MethodDelete, "/api/v1/people/"+tc.target+"/ssh-keys", admin, "")
			if w.Code != tc.want || len(st.keys) != 3 {
				t.Fatalf("status=%d want=%d keys=%+v body=%s", w.Code, tc.want, st.keys, w.Body.String())
			}
			assertSSHDeletionAudit(t, audit, tc.target, "failure", 0)
		})
	}
}
