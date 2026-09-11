// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// W31-S1-7: /me used to say nothing about when an SSO session would die, so
// the console had no way to warn ahead of the silent 401 the expiry causes.
// handleMe now includes session_expires_at when (and only when) a verified
// OIDC session is on the context.
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
		ctx = withOIDCRole(ctx, oidc.RoleMember)
		return ctx, sub
	}

	t.Run("operator: always null", func(t *testing.T) {
		s := &Server{cfg: Config{MemberMounts: runner.MemberMountPolicy{Roots: []string{"/home/agent-projects"}}}}
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
		s := &Server{cfg: Config{MemberMounts: runner.MemberMountPolicy{Roots: []string{"/home/agent-projects"}}}}
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
		s := &Server{cfg: Config{MemberMounts: runner.MemberMountPolicy{
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
		ctx = withOIDCRole(ctx, oidc.RoleMember)
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
