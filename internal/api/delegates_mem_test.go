// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// memDelegateServer is a server over the in-memory delegate store: the
// register, list and revoke routes run without Postgres.
type memDelegateServer struct {
	srv *Server
	h   *harness
	ds  *fakeDelegateStore
}

func newMemDelegateServer(t *testing.T) memDelegateServer {
	t.Helper()
	h := newHarness(t)
	ds := newFakeDelegateStore()
	srv := New(baseTestConfig(h, struct {
		store.Store
		*fakeDelegateStore
	}{nil, ds}))
	return memDelegateServer{srv: srv, h: h, ds: ds}
}

func TestRegisterDelegateRefusalsWriteNothing(t *testing.T) {
	m := newMemDelegateServer(t)
	for _, tc := range []struct {
		name, body, reason string
	}{
		{"no name", `{"name":"  ","idp_client_id":"c","group":"portal-users"}`, reasonDelegateNameInvalid},
		{"name with a control character", `{"name":"a\u0007b","idp_client_id":"c","group":"portal-users"}`, reasonDelegateNameInvalid},
		{"name too long", `{"name":"` + strings.Repeat("n", apiTokenNameMaxLen+1) + `","idp_client_id":"c","group":"g"}`, reasonDelegateNameInvalid},
		{"no client id", `{"name":"portal","idp_client_id":"","group":"portal-users"}`, reasonDelegateClientIDInvalid},
		{"client id too long", `{"name":"portal","idp_client_id":"` + strings.Repeat("c", apiTokenNameMaxLen+1) + `","group":"g"}`, reasonDelegateClientIDInvalid},
		{"no group", `{"name":"portal","idp_client_id":"c","group":""}`, reasonDelegateGroupInvalid},
		{"group that is not ASCII", `{"name":"portal","idp_client_id":"c","group":"grüppe"}`, reasonDelegateGroupInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, m.srv, http.MethodPost, "/api/v1/admin/delegates", adminToken, tc.body)
			if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != tc.reason {
				t.Errorf("= %d %s, want 422 %s", w.Code, w.Body.String(), tc.reason)
			}
		})
	}
	if w := do(t, m.srv, http.MethodPost, "/api/v1/admin/delegates", adminToken, `{"name":"p","surprise":1}`); w.Code != http.StatusBadRequest {
		t.Errorf("unknown field = %d, want 400", w.Code)
	}
	if list, _ := m.ds.ListDelegates(context.Background()); len(list) != 0 {
		t.Errorf("refused registrations stored %d portal(s)", len(list))
	}
}

func TestDelegateLifecycleRegisterListRevoke(t *testing.T) {
	m := newMemDelegateServer(t)

	w := do(t, m.srv, http.MethodPost, "/api/v1/admin/delegates", adminToken,
		`{"name":" Support portal ","idp_client_id":" portal-client ","group":"Portal-Users"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("register = %d %s, want 201", w.Code, w.Body.String())
	}
	var d types.Delegate
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Name != "Support portal" || d.IdPClientID != "portal-client" || d.Group != "portal-users" {
		t.Errorf("registered %+v, want trimmed name and client id and a lower-cased group", d)
	}
	if !strings.HasPrefix(d.Credential, delegateCredentialPrefix) || d.RegisteredBy == "" {
		t.Errorf("credential %q / registered_by %q, want a %s credential and the registrar", d.Credential, d.RegisteredBy, delegateCredentialPrefix)
	}
	// The credential returned once is the one the store resolves the portal by.
	if got, err := m.ds.GetDelegateByRaw(context.Background(), d.Credential); err != nil || got.ID != d.ID {
		t.Errorf("lookup by the returned credential = %+v, %v, want the registered portal", got, err)
	}
	if got := m.h.auditOutcomes("delegate.create"); len(got) != 1 || got[0] != "success" {
		t.Errorf("delegate.create audit outcomes = %v, want [success]", got)
	}

	w = do(t, m.srv, http.MethodGet, "/api/v1/admin/delegates", adminToken, "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), d.Credential) || strings.Contains(w.Body.String(), "credential") {
		t.Errorf("list = %d %s, want 200 and no credential", w.Code, w.Body.String())
	}
	var listed []types.Delegate
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil || len(listed) != 1 || listed[0].ID != d.ID {
		t.Errorf("list body %s, want the one portal (err %v)", w.Body.String(), err)
	}

	if w := do(t, m.srv, http.MethodDelete, "/api/v1/admin/delegates/not-a-uuid", adminToken, ""); w.Code != http.StatusBadRequest || reasonOf(t, w) != reasonInvalidIDParam {
		t.Errorf("revoke with a malformed id = %d %s, want 400 %s", w.Code, w.Body.String(), reasonInvalidIDParam)
	}
	if w := do(t, m.srv, http.MethodDelete, "/api/v1/admin/delegates/"+uuid.NewString(), adminToken, ""); w.Code != http.StatusNotFound || reasonOf(t, w) != reasonDelegateNotFound {
		t.Errorf("revoke of an unknown portal = %d %s, want 404 %s", w.Code, w.Body.String(), reasonDelegateNotFound)
	}
	if w := do(t, m.srv, http.MethodDelete, "/api/v1/admin/delegates/"+d.ID.String(), adminToken, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d %s, want 204", w.Code, w.Body.String())
	}
	if w := do(t, m.srv, http.MethodDelete, "/api/v1/admin/delegates/"+d.ID.String(), adminToken, ""); w.Code != http.StatusNotFound {
		t.Errorf("second revoke = %d, want 404 (already revoked)", w.Code)
	}
	if got := m.h.auditOutcomes("delegate.revoke"); len(got) != 1 {
		t.Errorf("delegate.revoke audit rows = %d, want 1 (the repeat writes none)", len(got))
	}
	if _, err := m.ds.GetDelegateByRaw(context.Background(), d.Credential); err == nil {
		t.Error("a revoked portal's credential still resolves")
	}
}

func TestIsOwnClientID(t *testing.T) {
	for _, tc := range []struct {
		id, own string
		want    bool
	}{
		{"11111111-aaaa", "11111111-aaaa", true},
		{"11111111-AAAA", "11111111-aaaa", true},
		{"api://11111111-aaaa", "11111111-aaaa", true},
		{"API://11111111-AAAA", "11111111-aaaa", true},
		{"22222222-bbbb", "11111111-aaaa", false},
		{"api://22222222-bbbb", "11111111-aaaa", false},
		{"11111111-aaaa-suffix", "11111111-aaaa", false},
	} {
		if got := isOwnClientID(tc.id, tc.own); got != tc.want {
			t.Errorf("isOwnClientID(%q, %q) = %v, want %v", tc.id, tc.own, got, tc.want)
		}
	}
}
