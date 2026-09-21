// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

// families_untested_test.go is #174's client-family half: table tests for the
// eight *client.Client methods docs/TEST-GAPS.md listed as genuinely untested
// (no test anywhere in the module reached them, per the union coverage
// profile) — every one a thin c.do(...) wrapper, so the table only needs to
// pin method+path+request-body-in / response-body-out for each, the same
// shape client_test.go's individual tests already use (newTestClient,
// writeJSON, checkAuth).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestUntestedClientMethods_Table(t *testing.T) {
	wsID := uuid.New()

	cases := []struct {
		name string
		// handle serves the fake control-plane response and asserts the
		// request the client sent (method, path, body).
		handle func(t *testing.T) http.HandlerFunc
		// call invokes the method under test against c and asserts the
		// decoded result.
		call func(t *testing.T, c *client.Client)
	}{
		{
			name: "ConnectManagedSubscription",
			handle: func(t *testing.T) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPut || r.URL.Path != "/api/v1/setup/harness-credential/anthropic" {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					}
					checkAuth(t, r)
					var body map[string]string
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["token"] != "setup-token-xyz" {
						t.Errorf("unexpected body: %v", body)
					}
					w.WriteHeader(http.StatusNoContent)
				}
			},
			call: func(t *testing.T, c *client.Client) {
				if err := c.ConnectManagedSubscription(context.Background(), "anthropic", "setup-token-xyz"); err != nil {
					t.Fatalf("ConnectManagedSubscription: %v", err)
				}
			},
		},
		{
			name: "GetDefaultPolicy",
			handle: func(t *testing.T) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet || r.URL.Path != "/api/v1/policies/default" {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					}
					checkAuth(t, r)
					writeJSON(w, http.StatusOK, types.RunPolicySpec{AllowedDomains: []string{"example.com"}})
				}
			},
			call: func(t *testing.T, c *client.Client) {
				got, err := c.GetDefaultPolicy(context.Background())
				if err != nil {
					t.Fatalf("GetDefaultPolicy: %v", err)
				}
				if len(got.AllowedDomains) != 1 || got.AllowedDomains[0] != "example.com" {
					t.Errorf("got AllowedDomains %v", got.AllowedDomains)
				}
			},
		},
		{
			name: "GetSiteConfig",
			handle: func(t *testing.T) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet || r.URL.Path != "/api/v1/site-config" {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					}
					checkAuth(t, r)
					writeJSON(w, http.StatusOK, types.SiteConfig{UpstreamProxySecretRef: "proxy-secret"})
				}
			},
			call: func(t *testing.T, c *client.Client) {
				got, err := c.GetSiteConfig(context.Background())
				if err != nil {
					t.Fatalf("GetSiteConfig: %v", err)
				}
				if got.UpstreamProxySecretRef != "proxy-secret" {
					t.Errorf("got UpstreamProxySecretRef %q", got.UpstreamProxySecretRef)
				}
			},
		},
		{
			name: "Me",
			handle: func(t *testing.T) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet || r.URL.Path != "/api/v1/me" {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					}
					checkAuth(t, r)
					writeJSON(w, http.StatusOK, map[string]string{"sub": "admin"})
				}
			},
			call: func(t *testing.T, c *client.Client) {
				got, err := c.Me(context.Background())
				if err != nil {
					t.Fatalf("Me: %v", err)
				}
				var decoded map[string]string
				if err := json.Unmarshal(got, &decoded); err != nil || decoded["sub"] != "admin" {
					t.Errorf("got %s, want sub=admin", got)
				}
			},
		},
		{
			name: "RecordWorkspaceTask",
			handle: func(t *testing.T) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost || r.URL.Path != "/api/v1/workspaces/"+wsID.String()+"/record" {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					}
					checkAuth(t, r)
					var body map[string]string
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["task_key"] != "onboard" {
						t.Errorf("unexpected body: %v", body)
					}
					writeJSON(w, http.StatusAccepted, client.RecordTaskResult{
						RecordRunID: "run-1", TaskKey: "onboard", Mode: "open",
					})
				}
			},
			call: func(t *testing.T, c *client.Client) {
				got, err := c.RecordWorkspaceTask(context.Background(), wsID, "onboard")
				if err != nil {
					t.Fatalf("RecordWorkspaceTask: %v", err)
				}
				if got.RecordRunID != "run-1" || got.TaskKey != "onboard" {
					t.Errorf("got %+v", got)
				}
			},
		},
		{
			name: "RevokeSessions",
			handle: func(t *testing.T) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sessions/revoke" {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					}
					checkAuth(t, r)
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["sub"] != "user@example.com" || body["all"] != false {
						t.Errorf("unexpected body: %v", body)
					}
					w.WriteHeader(http.StatusNoContent)
				}
			},
			call: func(t *testing.T, c *client.Client) {
				if err := c.RevokeSessions(context.Background(), "user@example.com", false); err != nil {
					t.Fatalf("RevokeSessions: %v", err)
				}
			},
		},
		{
			name: "ScanWorkspace",
			handle: func(t *testing.T) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost || r.URL.Path != "/api/v1/workspaces/"+wsID.String()+"/scan" {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					}
					checkAuth(t, r)
					writeJSON(w, http.StatusOK, map[string]string{"status": "scanning"})
				}
			},
			call: func(t *testing.T, c *client.Client) {
				got, err := c.ScanWorkspace(context.Background(), wsID)
				if err != nil {
					t.Fatalf("ScanWorkspace: %v", err)
				}
				var decoded map[string]string
				if err := json.Unmarshal(got, &decoded); err != nil || decoded["status"] != "scanning" {
					t.Errorf("got %s", got)
				}
			},
		},
		{
			name: "UpdateWorkspace",
			handle: func(t *testing.T) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPut || r.URL.Path != "/api/v1/workspaces/"+wsID.String() {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					}
					checkAuth(t, r)
					var body client.WorkspaceRequest
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body.Name != "renamed" {
						t.Errorf("unexpected body: %+v", body)
					}
					writeJSON(w, http.StatusOK, types.Workspace{ID: wsID, Name: "renamed"})
				}
			},
			call: func(t *testing.T, c *client.Client) {
				got, err := c.UpdateWorkspace(context.Background(), wsID, client.WorkspaceRequest{Name: "renamed"})
				if err != nil {
					t.Fatalf("UpdateWorkspace: %v", err)
				}
				if got.Name != "renamed" {
					t.Errorf("got Name %q", got.Name)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handle(t))
			defer srv.Close()
			tc.call(t, newTestClient(srv))
		})
	}
}
