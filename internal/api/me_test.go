// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// countingSiteStore is the login fixture's store with ONE observable: how many
// times the roster was read. /me is the most-polled route in the console, so
// "did this answer cost a store read" is a property worth pinning rather than
// re-deriving from a profiler.
type countingSiteStore struct {
	*integStore
	siteReads atomic.Int64
}

func (s *countingSiteStore) GetSiteConfig(ctx context.Context) (types.SiteConfig, error) {
	s.siteReads.Add(1)
	return s.integStore.GetSiteConfig(ctx)
}

// TestHandleMe_MemberPollPerformsNoSiteConfigRead pins the SHORT-CIRCUIT in
// user_preview_available.
//
// The key ANDs "this deployment keeps a credential per person" (a roster read)
// with "this caller is an admin" (a context read). Evaluating the roster first
// put a SECOND GetSiteConfig on every /me — members included, who poll it on a
// timer and whose answer is unconditionally false. Nothing leaks either way;
// the order is the whole finding.
//
// The count is ONE, not zero: /me has read the site config since 0.7 for the
// user-drive org switch (userDriveProvider), and that read is not this key's to
// remove. One is what a member's poll costs; two is the operands the wrong way
// round, which is what this case fails on.
func TestHandleMe_MemberPollPerformsNoSiteConfigRead(t *testing.T) {
	newSrv := func(t *testing.T) (*Server, *countingSiteStore) {
		t.Helper()
		h := newHarness(t)
		st := &countingSiteStore{integStore: &integStore{
			govEscapeStore: newGovEscapeStore(&capStore{}),
			site:           awsSSOTestSite(),
		}}
		cfg := baseTestConfig(h, st)
		cfg.OIDC = &oidc.Authenticator{}
		return New(cfg), st
	}

	t.Run("a member's poll reads no roster", func(t *testing.T) {
		srv, st := newSrv(t)
		w := doSSO(t, srv, http.MethodGet, "/api/v1/me",
			ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser), "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET /me = %d, want 200: %s", w.Code, w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode /me: %v", err)
		}
		if body["user_preview_available"] != false {
			t.Errorf("user_preview_available = %#v, want false for a member", body["user_preview_available"])
		}
		if n := st.siteReads.Load(); n != 1 {
			t.Errorf("a member's /me made %d site-config read(s), want 1 (the user-drive org switch alone) — "+
				"the role check has to short-circuit BEFORE the roster read on the console's most-polled route", n)
		}
	})

	// The control: the answer itself is unchanged for the tier the key is FOR.
	// A short-circuit that also stopped answering the admin would "fix" the cost
	// by deleting the feature.
	t.Run("an admin still gets the availability answer", func(t *testing.T) {
		srv, st := newSrv(t)
		w := doSSO(t, srv, http.MethodGet, "/api/v1/me",
			ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin), "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET /me = %d, want 200: %s", w.Code, w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode /me: %v", err)
		}
		if body["user_preview_available"] != true {
			t.Errorf("user_preview_available = %#v, want true on a per_user roster", body["user_preview_available"])
		}
		if n := st.siteReads.Load(); n < 2 {
			t.Errorf("the admin's /me made %d site-config read(s) — the availability answer "+
				"cannot have been resolved without a roster read beside the drive one", n)
		}
	})
}

// handleMe includes session_expires_at when (and only when) a verified OIDC
// session is on the context, so the console can warn ahead of the silent
// 401 an SSO session's expiry causes.
func TestHandleMe_SessionExpiry(t *testing.T) {
	s := &Server{}

	t.Run("SSO session publishes session_expires_at", func(t *testing.T) {
		exp := time.Now().Add(45 * time.Minute).UTC().Truncate(time.Second)
		r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		ctx := withOIDCHuman(r.Context(), "sub-alice")
		ctx = withOIDCExpiry(ctx, exp)
		r = r.WithContext(ctx)

		w := httptest.NewRecorder()
		s.handleMe(w, r)

		var body map[string]any
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		got, ok := body["session_expires_at"].(string)
		if !ok {
			t.Fatalf("session_expires_at missing or not a string: %#v", body["session_expires_at"])
		}
		gotT, err := time.Parse(time.RFC3339, got)
		if err != nil {
			t.Fatalf("session_expires_at not RFC3339: %v", err)
		}
		if !gotT.Equal(exp) {
			t.Fatalf("session_expires_at = %v, want %v", gotT, exp)
		}
	})

	t.Run("no SSO session (admin token / local mode) omits the field", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		w := httptest.NewRecorder()
		s.handleMe(w, r)

		var body map[string]any
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if _, present := body["session_expires_at"]; present {
			t.Fatalf("session_expires_at present with no OIDC session: %#v", body["session_expires_at"])
		}
	})
}

// M3: AddWorkspaceDialog's local_dir root hint reads GET /me's
// member_local_dir_root — it must be null for an operator and for a
// rootless member, and the operator's own prose (not a raw path dump) for a
// member with a configured root.
func TestHandleMe_MemberLocalDirRoot(t *testing.T) {
	memberCtx := func() (context.Context, string) {
		sub := "sub-bob"
		ctx := withOIDCHuman(context.Background(), sub)
		ctx = withOIDCRole(ctx, oidc.RoleUser)
		return ctx, sub
	}

	t.Run("operator: always null", func(t *testing.T) {
		s := &Server{cfg: Config{UserMounts: runner.UserMountPolicy{Roots: []string{"/home/agent-projects"}}}}
		r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		w := httptest.NewRecorder()
		s.handleMe(w, r)
		var body map[string]any
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body["member_local_dir_root"] != nil {
			t.Fatalf("member_local_dir_root = %#v, want nil for an operator", body["member_local_dir_root"])
		}
	})

	t.Run("member with no configured root: null", func(t *testing.T) {
		s := &Server{}
		ctx, _ := memberCtx()
		r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil).WithContext(ctx)
		w := httptest.NewRecorder()
		s.handleMe(w, r)
		var body map[string]any
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body["member_local_dir_root"] != nil {
			t.Fatalf("member_local_dir_root = %#v, want nil with no roots configured", body["member_local_dir_root"])
		}
	})

	t.Run("member with a configured root: hint string", func(t *testing.T) {
		s := &Server{cfg: Config{UserMounts: runner.UserMountPolicy{Roots: []string{"/home/agent-projects"}}}}
		ctx, _ := memberCtx()
		r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil).WithContext(ctx)
		w := httptest.NewRecorder()
		s.handleMe(w, r)
		var body map[string]any
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		got, ok := body["member_local_dir_root"].(string)
		if !ok || got != "/home/agent-projects" {
			t.Fatalf("member_local_dir_root = %#v, want %q", body["member_local_dir_root"], "/home/agent-projects")
		}
	})

	t.Run("member with a per-principal root REPLACING the shared list", func(t *testing.T) {
		s := &Server{cfg: Config{UserMounts: runner.UserMountPolicy{
			Roots:            []string{"/shared/root"},
			RootsByPrincipal: map[string][]string{"sub-bob": {"/bob/only"}},
		}}}
		ctx, _ := memberCtx()
		r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil).WithContext(ctx)
		w := httptest.NewRecorder()
		s.handleMe(w, r)
		var body map[string]any
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		got, ok := body["member_local_dir_root"].(string)
		if !ok || got != "/bob/only" {
			t.Fatalf("member_local_dir_root = %#v, want %q (per-principal replaces shared)", body["member_local_dir_root"], "/bob/only")
		}
	})
}

// 0.7.1: the console header shows the person, not the IdP's object id. /me
// must publish the display name and the email BESIDE the principal — the
// principal stays the ownership key the console compares against, the other
// two are what it renders — and say "" for both when there is no SSO session.
func TestHandleMe_PublishesNameAndEmailBesidePrincipal(t *testing.T) {
	s := &Server{}

	t.Run("SSO session: principal, email and name are three distinct keys", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		ctx := withOIDCHuman(r.Context(), "gsv-member-0001")
		ctx = withOIDCEmail(ctx, "alice.smith@corp.example")
		ctx = withOIDCName(ctx, "Alice Smith")
		ctx = withOIDCRole(ctx, oidc.RoleUser)
		r = r.WithContext(ctx)

		w := httptest.NewRecorder()
		s.handleMe(w, r)

		var body map[string]any
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		for key, want := range map[string]string{
			"principal": "gsv-member-0001",
			"email":     "alice.smith@corp.example",
			"name":      "Alice Smith",
		} {
			if got, _ := body[key].(string); got != want {
				t.Errorf("%s = %q, want %q", key, got, want)
			}
		}
	})

	t.Run("no SSO session (admin token / local mode): name and email are empty, never absent", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		w := httptest.NewRecorder()
		s.handleMe(w, r)

		var body map[string]any
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		for _, key := range []string{"email", "name"} {
			got, ok := body[key].(string)
			if !ok || got != "" {
				t.Errorf("%s = %#v, want the empty string (present, empty)", key, body[key])
			}
		}
	})
}

// #1335: /me's user_view_super_admin is the ONE new bit the console needs to
// decide whether the user view may offer the link to the admin-only recap.
// It reads the role STAMPED on the signed cookie (every other role field is
// clamped), and it must never be true for anyone but a stamped admin inside
// the view.
func TestHandleMe_UserViewSuperAdmin(t *testing.T) {
	const key = "user_view_super_admin"
	cases := []struct {
		name    string
		role    string
		inView  bool
		want    any // true, false, or nil for "key absent"
		clamped bool
	}{
		{"stamped admin in the view", oidc.RoleAdmin, true, true, true},
		{"stamped security admin in the view", oidc.RoleSecurityAdmin, true, false, true},
		{"stamped admin outside the view", oidc.RoleAdmin, false, nil, false},
		{"stamped security admin outside the view", oidc.RoleSecurityAdmin, false, nil, false},
		{"real user", oidc.RoleUser, false, nil, false},
		{"hand-built user cookie carrying the view flag", oidc.RoleUser, true, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := memberModeServer(t)
			body := meBody(t, srv, memberModeSSOSession(t, "sub-usa-1", "usa@corp.example", tc.role, tc.inView))
			got, present := body[key]
			if tc.want == nil {
				if present {
					t.Fatalf("%s = %v, want the key absent", key, got)
				}
			} else if got != tc.want {
				t.Fatalf("%s = %v (present=%v), want %v", key, got, present, tc.want)
			}
			if tc.clamped {
				// The clamped fields stay clamped: this bit is additive.
				if body["role"] != oidc.RoleUser || body["operator"] != false || body["security_operator"] != false {
					t.Errorf("clamped fields = role:%v operator:%v security_operator:%v, want user/false/false",
						body["role"], body["operator"], body["security_operator"])
				}
			}
		})
	}

	t.Run("a non-super-admin never gets true", func(t *testing.T) {
		for _, role := range []string{oidc.RoleSecurityAdmin, oidc.RoleUser} {
			for _, inView := range []bool{true, false} {
				srv, _ := memberModeServer(t)
				body := meBody(t, srv, memberModeSSOSession(t, "sub-usa-2", "usa2@corp.example", role, inView))
				if body[key] == true {
					t.Errorf("role %q inView=%v: %s = true", role, inView, key)
				}
			}
		}
	})

	t.Run("the request cannot ask for it", func(t *testing.T) {
		srv, _ := memberModeServer(t)
		r := httptest.NewRequest(http.MethodGet, "/api/v1/me?"+key+"=true", nil)
		r.Header.Set("X-"+key, "true")
		r.AddCookie(memberModeSSOSession(t, "sub-usa-3", "usa3@corp.example", oidc.RoleSecurityAdmin, false))
		w := httptest.NewRecorder()
		panicFails(t, srv.Handler()).ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /me = %d: %s", w.Code, w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if _, present := body[key]; present {
			t.Errorf("%s present for a security admin outside the view: %v", key, body[key])
		}
	})

	t.Run("a hand-built cookie signed with another key is refused", func(t *testing.T) {
		srv, _ := memberModeServer(t)
		payload, err := json.Marshal(oidc.Session{
			V: oidc.SessionCodecVersion, Sub: "sub-usa-4", Role: oidc.RoleAdmin, UserType: "standard",
			MemberMode: true, Expiry: time.Now().UTC().Add(time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		mac := hmac.New(sha256.New, []byte("not the session key"))
		mac.Write(payload)
		forged := &http.Cookie{
			Name:  "wardyn_session",
			Value: base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)),
		}
		w := doSSO(t, srv, http.MethodGet, "/api/v1/me", forged, "")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("forged cookie: GET /me = %d, want 401: %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), key) {
			t.Errorf("forged-cookie body carries %s: %s", key, w.Body.String())
		}
	})

	t.Run("admin token and local mode have no stamped role", func(t *testing.T) {
		srv, _ := memberModeServer(t)
		w := do(t, srv, http.MethodGet, "/api/v1/me", adminToken, "")
		if w.Code != http.StatusOK {
			t.Fatalf("token GET /me = %d: %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), key) {
			t.Errorf("admin-token /me carries %s: %s", key, w.Body.String())
		}

		lsrv, _ := memberModeServer(t)
		cfg := lsrv.cfg
		cfg.OIDC = nil
		cfg.LocalMode = true
		cfg.LocalOperator = "local:test"
		cfg.LocalLoopback = true
		w = do(t, New(cfg), http.MethodGet, "/api/v1/me", "", "")
		if w.Code != http.StatusOK {
			t.Fatalf("local GET /me = %d: %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), key) {
			t.Errorf("local-mode /me carries %s: %s", key, w.Body.String())
		}
	})
}
