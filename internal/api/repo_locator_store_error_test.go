// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestRepoLocator_ADOServerEscapedNameUnderStoreError: whether an escaped name
// on a self-hosted host is an Azure DevOps Server address comes from the
// provider rows, so with the site config unreadable neither write door can
// tell. The 400 must name the store, not blame the address's shape (#724). An
// address wrong on any host still gets the shape sentence.
func TestRepoLocator_ADOServerEscapedNameUnderStoreError(t *testing.T) {
	const (
		escaped = "https://tfs.corp.example/tfs/DefaultCollection/Payments%20Platform/_git/Card%20Auth"
		dotted  = "https://tfs.corp.example/tfs/DefaultCollection/Payments%20Platform/../_git/Card%20Auth"
	)
	doors := []struct {
		name, path, field string
		body              func(repo string) string
	}{
		{"workspace", "/api/v1/workspaces", "source", func(repo string) string {
			return `{"name":"ws","sources":[{"type":"repo","source":` + quote(repo) + `}]}`
		}},
		{"library", "/api/v1/sources", "locator", func(repo string) string {
			return `{"kind":"repo","locator":` + quote(repo) + `}`
		}},
	}
	for _, door := range doors {
		for _, tc := range []struct {
			name, repo string
			getErr     error
			wantStore  bool
		}{
			{"escaped name, store unreadable", escaped, errors.New("site config: connection refused"), true},
			{"escaped name, no row names the host", escaped, nil, false},
			{"dot segment, store unreadable", dotted, errors.New("site config: connection refused"), false},
		} {
			t.Run(door.name+"/"+tc.name, func(t *testing.T) {
				srv, _ := newSiteConfigHarness(t, &fakeSiteConfigStore{getErr: tc.getErr})
				w := do(t, srv, http.MethodPost, door.path, adminToken, door.body(tc.repo))
				if w.Code != http.StatusBadRequest {
					t.Fatalf("POST %s = %d %s, want 400", door.path, w.Code, w.Body.String())
				}
				body := w.Body.String()
				namesStore := strings.Contains(body, "site configuration") && strings.Contains(body, "could not be read")
				namesShape := strings.Contains(body, "is not a repository address")
				if namesStore != tc.wantStore || namesShape == tc.wantStore {
					t.Errorf("body = %s, want the store named = %v", body, tc.wantStore)
				}
				if strings.Contains(body, "connection refused") {
					t.Errorf("body carries the store error's own text: %s", body)
				}
			})
		}
	}
}
