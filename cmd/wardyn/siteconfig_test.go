// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//lint:file-ignore SA1019 TestSiteConfigApply_AcceptsLegacyArtifactOverrides reads
// the deprecated SiteConfig.ArtifactOverrides on purpose: it proves `wardyn
// site-config apply` still FORWARDS a legacy artifact_overrides document to the
// server, which is the only way the server-side fold can happen at all. Same
// reason internal/api/site_config_test.go carries this directive.

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestSiteConfigApply_ForwardsTheOnboardingMark is the CLI half of the
// round-trip contract internal/api's TestPutSiteConfig_GetBodyRoundTripsVerbatim
// pins on the server: `wardyn site-config get > corp-baseline.json` emits
// onboarding_completed_at on any install whose operator finished the Getting
// Started funnel, and `apply` forwards that document VERBATIM — no client-side
// strip stands between the operator's file and the handler. That is why the
// server had to stop 400ing it (R3 F025): the fix belongs in the one place
// every consumer routes through, and a strip added here instead would silently
// re-break the hand-rolled curl and the MDM-delivered
// /etc/wardyn/site-config.json, which no client of ours touches.
//
// Unlike the server-side pin this one is green at the RC too — deliberately:
// its job is to fail if someone later "fixes" the same footgun client-side, the
// way Integrations is stripped ten lines above in PutSiteConfig.
func TestSiteConfigApply_ForwardsTheOnboardingMark(t *testing.T) {
	var got types.SiteConfig
	srv := applyServer(t, &got)

	captured := `{"scm_hosts":["gitlab.corp"],"onboarding_completed_at":"2026-08-30T12:00:00Z"}`
	if _, _, err := runSiteConfigApply(t, srv.URL, captured); err != nil {
		t.Fatalf("apply of a captured document: %v", err)
	}
	if got.OnboardingCompletedAt == nil {
		t.Fatalf("server received %+v, want onboarding_completed_at forwarded verbatim", got)
	}
	if want := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC); !got.OnboardingCompletedAt.Equal(want) {
		t.Errorf("server received onboarding_completed_at = %v, want %v", got.OnboardingCompletedAt, want)
	}
}

// TestSiteConfigApply_WarnsTheOnboardingMarkWasNotApplied is the CLI half of the
// signal that REPLACED the 400 (R3 F025 fix-up): onboarding_completed_at is
// server-owned, so a captured document's copy is dropped on the write. The
// server says so with onboarding_completed_at_ignored, and `apply` must print
// that the way it prints the integrations warning ten lines above — this file is
// exactly the one an operator applies after a reset, or an MDM re-applies on
// every boot, when the mark it carries is another install's. Silence here reads
// as a restore that happened; a 400 here broke the recovery flow outright.
func TestSiteConfigApply_WarnsTheOnboardingMarkWasNotApplied(t *testing.T) {
	captured := `{"scm_hosts":["gitlab.corp"],"onboarding_completed_at":"2026-08-30T12:00:00Z"}`

	// The shape the server answers when the body named a mark it did not keep:
	// the write succeeded, the install's OWN mark came back, and the drop is
	// reported beside it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/site-config" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scm_hosts":["gitlab.corp"],` +
			`"onboarding_completed_at":"2026-09-01T09:00:00Z","onboarding_completed_at_ignored":true}`))
	}))
	t.Cleanup(srv.Close)

	_, stderr, err := runSiteConfigApply(t, srv.URL, captured)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(stderr, "onboarding_completed_at") {
		t.Errorf("stderr = %q, want a warning that the file's onboarding_completed_at was not applied", stderr)
	}

	// And the ordinary case stays quiet: a server that reports no drop (an exact
	// echo, or a build that predates the signal) must print no warning at all.
	var got types.SiteConfig
	_, quiet, err := runSiteConfigApply(t, applyServer(t, &got).URL, captured)
	if err != nil {
		t.Fatalf("apply against an echoing server: %v", err)
	}
	if strings.Contains(quiet, "onboarding_completed_at") {
		t.Errorf("stderr = %q, want silence when the server reports no dropped mark", quiet)
	}
}
