// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// These tests exercise the admin-gated workspace routes WITHOUT a Postgres
// pool, mirroring policies_test.go: only paths that fail closed BEFORE any
// store call (auth gating, body/source validation 400s, id parsing 400s) are
// covered here. A happy-path store round-trip needs a real Store and is out of
// scope for this pool-free harness.

func TestWorkspaceRoutesRequireAdminAuth(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/workspaces"},
		{http.MethodPost, "/api/v1/workspaces"},
		{http.MethodGet, "/api/v1/workspaces/" + uuid.New().String()},
		{http.MethodPut, "/api/v1/workspaces/" + uuid.New().String()},
		{http.MethodDelete, "/api/v1/workspaces/" + uuid.New().String()},
		{http.MethodPost, "/api/v1/workspaces/" + uuid.New().String() + "/scan"},
	}
	for _, c := range cases {
		if w := do(t, h.srv, c.method, c.path, "", ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s no token: code = %d, want 401", c.method, c.path, w.Code)
		}
		if w := do(t, h.srv, c.method, c.path, "wrong", ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s wrong token: code = %d, want 401", c.method, c.path, w.Code)
		}
	}
}

func TestCreateWorkspaceValidation(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name string
		body string
	}{
		{"invalid json", `{not json`},
		{"missing name", `{"kind":"local_dir","source":"/home/u/repo"}`},
		{"blank name", `{"name":"  ","kind":"local_dir","source":"/home/u/repo"}`},
		{"missing source", `{"name":"w","kind":"local_dir"}`},
		{"unknown kind", `{"name":"w","kind":"weird","source":"/home/u/repo"}`},
		{"unknown field (typo)", `{"name":"w","kind":"local_dir","sourc":"/home/u/repo"}`},
		{"local_dir denied source", `{"name":"w","kind":"local_dir","source":"/etc"}`},
		{"local_dir non-absolute source", `{"name":"w","kind":"local_dir","source":"relative"}`},
		{"repo source with whitespace", `{"name":"w","kind":"repo","source":"org/name; rm -rf"}`},
		{"repo source not a recognized slug/URL", `{"name":"w","kind":"repo","source":"not a repo"}`},
		{"bad default_target", `{"name":"w","kind":"local_dir","source":"/home/u/repo","default_target":"/etc"}`},
	}
	for _, c := range cases {
		w := do(t, h.srv, http.MethodPost, "/api/v1/workspaces", adminToken, c.body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("create %q: code = %d, want 400; body=%s", c.name, w.Code, w.Body.String())
		}
	}
}

func TestUpdateWorkspaceValidation(t *testing.T) {
	h := newHarness(t)
	id := uuid.New().String()
	if w := do(t, h.srv, http.MethodPut, "/api/v1/workspaces/"+id,
		adminToken, `{"name":"w","kind":"weird","source":"/home/u/repo"}`); w.Code != http.StatusBadRequest {
		t.Errorf("update invalid spec: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if w := do(t, h.srv, http.MethodPut, "/api/v1/workspaces/not-a-uuid",
		adminToken, `{"name":"w","kind":"local_dir","source":"/home/u/repo"}`); w.Code != http.StatusBadRequest {
		t.Errorf("update bad id: code = %d, want 400", w.Code)
	}
}

func TestGetDeleteScanWorkspaceBadID(t *testing.T) {
	h := newHarness(t)
	if w := do(t, h.srv, http.MethodGet, "/api/v1/workspaces/not-a-uuid", adminToken, ""); w.Code != http.StatusBadRequest {
		t.Errorf("get bad id: code = %d, want 400", w.Code)
	}
	if w := do(t, h.srv, http.MethodDelete, "/api/v1/workspaces/not-a-uuid", adminToken, ""); w.Code != http.StatusBadRequest {
		t.Errorf("delete bad id: code = %d, want 400", w.Code)
	}
	if w := do(t, h.srv, http.MethodPost, "/api/v1/workspaces/not-a-uuid/scan", adminToken, ""); w.Code != http.StatusBadRequest {
		t.Errorf("scan bad id: code = %d, want 400", w.Code)
	}
}
