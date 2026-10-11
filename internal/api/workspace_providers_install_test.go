// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestGitHubAppInstallURLWriteBoundary(t *testing.T) {
	const good = "https://github.com/apps/wardyn-test/installations/new"
	for _, raw := range []string{good, "http://github.com/apps/test/installations/new", "https://evil.example/apps/test/installations/new", "https://user:secret@github.com/apps/test/installations/new", good + "?secret=value", good + "#x", good + "#", "https://github.com:443/apps/test/installations/new", "https://github.com/apps/%74est/installations/new", "https://github.com/apps/test/../other/installations/new", "https://github.com/apps/test/installations/new?", "https://github.com/apps/Test/installations/new"} {
		row := githubRow("gh", false, "https://github.com/acme")
		row.GitHubAppInstallURL = raw
		if err := validateWorkspaceProviders(&types.WorkspaceProviders{Git: []types.GitProvider{row}}, true); (err == nil) != (raw == good) {
			t.Errorf("URL %q error = %v", raw, err)
		}
	}
	for _, row := range []types.GitProvider{
		{ID: "enterprise", Kind: types.GitProviderGitHub, BaseURLs: []string{"https://git.corp.example/acme"}, Lanes: []types.GitLane{types.GitLanePAT}, GitHubAppInstallURL: good},
		{ID: "no-app", Kind: types.GitProviderGitHub, BaseURLs: []string{"https://github.com/acme"}, Lanes: []types.GitLane{types.GitLanePAT}, GitHubAppInstallURL: good},
	} {
		if err := validateWorkspaceProviders(&types.WorkspaceProviders{Git: []types.GitProvider{row}}, true); err == nil {
			t.Fatalf("unsupported App capability accepted: %+v", row)
		}
	}
	for _, path := range []string{"/api/v1/workspace-providers", "/api/v1/site-config"} {
		store := &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{}}
		srv, _ := newProvidersHarness(t, store)
		block := `{"git":[{"id":"gh","kind":"github","base_urls":["https://github.com/acme"],"github_app_install_url":"` + good + `"}]}`
		body := block
		if path == "/api/v1/site-config" {
			body = `{"workspace_providers":` + block + `}`
		}
		w := do(t, srv, http.MethodPut, path, adminToken, body)
		if w.Code != http.StatusOK || store.cfg.WorkspaceProviders.Git[0].GitHubAppInstallURL != good {
			t.Fatalf("%s = %d %s; metadata not persisted", path, w.Code, w.Body.String())
		}
		bad := strings.ReplaceAll(body, good, "https://evil.example/apps/test/installations/new")
		w = do(t, srv, http.MethodPut, path, adminToken, bad)
		if w.Code != http.StatusBadRequest || store.cfg.WorkspaceProviders.Git[0].GitHubAppInstallURL != good {
			t.Fatalf("%s bad URL = %d %s; prior config must remain", path, w.Code, w.Body.String())
		}
	}
}
