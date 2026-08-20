// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// applyServer answers PUT /site-config with whatever body it received, and
// records it so a test can assert what actually went on the wire.
func applyServer(t *testing.T, got *types.SiteConfig) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/api/v1/site-config" {
			_ = json.NewDecoder(r.Body).Decode(got)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(got)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runSiteConfigApply writes doc to a temp file, runs `site-config apply` on it,
// and returns stdout, stderr and the command error.
func runSiteConfigApply(t *testing.T, url, doc string) (string, string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "corp-baseline.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write doc: %v", err)
	}
	root := rootCmd()
	root.SetArgs([]string{"site-config", "apply", path, "--url", url, "--token", "tok"})
	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := root.Execute()
	return stdout.String(), stderr.String(), err
}

// TestSiteConfigApply_RejectsUnknownField: `apply` REPLACES the whole document,
// so a typo'd key dropped by a non-strict decode is not a no-op — the field the
// operator meant to set is absent from the re-marshaled body and the real
// setting is DELETED server-side. The server's own decodeStrict can never catch
// it, because the CLI already dropped the field before re-encoding.
func TestSiteConfigApply_RejectsUnknownField(t *testing.T) {
	var got types.SiteConfig
	srv := applyServer(t, &got)

	// "upstream_proxy_ur1" — one transposed character away from the real key.
	_, _, err := runSiteConfigApply(t, srv.URL, `{"upstream_proxy_ur1":"http://proxy.corp:3128"}`)
	if err == nil {
		t.Fatalf("apply accepted an unknown field and PUT %+v — a typo'd key must fail here, not silently wipe the setting", got)
	}
	if !strings.Contains(err.Error(), "upstream_proxy_ur1") {
		t.Errorf("error = %v, want it to name the unknown field", err)
	}
}

// TestSiteConfigApply_AcceptsLegacyArtifactOverrides: strictness must not break
// the documented legacy round-trip — artifact_overrides is still a real (if
// deprecated) field, folded server-side, so a document saved before
// egress_redirects existed still applies.
func TestSiteConfigApply_AcceptsLegacyArtifactOverrides(t *testing.T) {
	var got types.SiteConfig
	srv := applyServer(t, &got)

	if _, _, err := runSiteConfigApply(t, srv.URL,
		`{"artifact_overrides":{"npm":{"base_url":"https://nexus.corp/npm"}}}`); err != nil {
		t.Fatalf("apply rejected a legacy artifact_overrides document: %v", err)
	}
	if len(got.ArtifactOverrides) != 1 {
		t.Errorf("server received %+v, want artifact_overrides forwarded for the server-side fold", got)
	}
}

// TestSiteConfigApply_WarnsIntegrationsNotRestored: PutSiteConfig strips
// Integrations before the request (the server 400s on a non-empty one and
// carries the STORED rows forward instead), so a captured document's
// integrations are neither sent nor restored. Silence there reads as a
// successful restore of something that was never restored.
func TestSiteConfigApply_WarnsIntegrationsNotRestored(t *testing.T) {
	var got types.SiteConfig
	srv := applyServer(t, &got)

	_, stderr, err := runSiteConfigApply(t, srv.URL,
		`{"integrations":[{"id":"11111111-1111-1111-1111-111111111111","kind":"anthropic","name":"prod"}]}`)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(got.Integrations) != 0 {
		t.Errorf("integrations reached the server (%+v); PutSiteConfig must strip them", got.Integrations)
	}
	if !strings.Contains(stderr, "integration") {
		t.Errorf("stderr = %q, want a warning that the file's integrations were not applied", stderr)
	}
}
