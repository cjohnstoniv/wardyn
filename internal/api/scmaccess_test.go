// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ── adoAccessState: the pure six-state grader ──────────────────────────────

func TestAdoAccessState(t *testing.T) {
	cases := []struct {
		name                                                    string
		rowConfigured, entraAvailable, isMechanism, found, shared bool
		want                                                    string
	}{
		{"no row at all: nothing per-principal to say", false, false, false, false, false, ""},
		{"no row, even if every other input says yes", false, true, false, true, true, ""},
		{"entra row, human, found: live", true, true, false, true, false, modelAccessLive},
		{"entra row, human, not found: not_configured", true, true, false, false, false, modelAccessNotConfigured},
		{"entra row, mechanism caller: not_applicable, regardless of found", true, true, true, true, false, modelAccessNotApplicable},
		{"entra row, mechanism caller, nothing found: still not_applicable", true, true, true, false, false, modelAccessNotApplicable},
		{"shared row, the stored credential present: live", true, false, false, false, true, modelAccessLive},
		{"shared row, nothing stored: shared_expired", true, false, false, false, false, modelAccessSharedExpired},
		{"shared row ignores isMechanism/found entirely", true, false, true, true, true, modelAccessLive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := adoAccessState(tc.rowConfigured, tc.entraAvailable, tc.isMechanism, tc.found, tc.shared)
			if got != tc.want {
				t.Errorf("adoAccessState(%v,%v,%v,%v,%v) = %q, want %q",
					tc.rowConfigured, tc.entraAvailable, tc.isMechanism, tc.found, tc.shared, got, tc.want)
			}
		})
	}
}

func TestScmAccessSourceFor(t *testing.T) {
	cases := map[string]string{
		adoEntraSourceLogin:  scmAccessSourceOrg,
		adoEntraSourceSignIn: scmAccessSourceSeparate,
		"":                   "",
		"something-unknown":  "",
	}
	for in, want := range cases {
		if got := scmAccessSourceFor(in); got != want {
			t.Errorf("scmAccessSourceFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// ── computeSCMAccessFor: the end-to-end integration over a site config ────

// scmTestStore is the minimal store.Store the computation reads: one
// SiteConfig, nothing else (the embed answers every other method with a nil
// panic no test here should ever reach).
type scmTestStore struct {
	store.Store
	site types.SiteConfig
}

func (s *scmTestStore) GetSiteConfig(context.Context) (types.SiteConfig, error) { return s.site, nil }

const scmTestRowID = "ado-row-1"

func adoTestSiteConfig(disabled bool) types.SiteConfig {
	return types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{{
		ID: scmTestRowID, Kind: types.GitProviderAzureDevOps, Disabled: disabled,
		BaseURLs: []string{"https://dev.azure.com/contoso"},
	}}}}
}

// newSCMTestServer builds a Server whose Azure DevOps row (id scmTestRowID)
// is per-user ONLY when entra is true — a nil ADOEntra source otherwise,
// which is exactly what an un-configured (shared) row looks like from this
// package's own seam (#383 has not landed; see scmaccess.go's file doc).
func newSCMTestServer(t *testing.T, sc types.SiteConfig, entra bool) *Server {
	t.Helper()
	cfg := Config{
		Store:        &scmTestStore{site: sc},
		Secrets:      &memSecrets{m: map[string][]byte{}},
		MaskRegistry: secretmask.NewRegistry(),
		Now:          func() time.Time { return adoTestNow },
	}
	if entra {
		cfg.ADOEntra = func(context.Context) (ADOEntraConfig, bool, error) {
			return ADOEntraConfig{
				RowID: scmTestRowID, TenantID: "tenant-1", ClientID: "client-1",
				RedirectURL: "https://console.example.invalid/cb", Scopes: []string{"vso.code"},
				LoginClientID: "client-1", LoginTenantID: "tenant-1",
			}, true, nil
		}
	}
	return &Server{cfg: cfg}
}

func TestComputeSCMAccessFor(t *testing.T) {
	t.Run("no Azure DevOps row: absent", func(t *testing.T) {
		s := newSCMTestServer(t, types.SiteConfig{}, false)
		access, ok := s.computeSCMAccessFor(context.Background(), types.SiteConfig{}, "alice")
		if ok || access.State != "" {
			t.Fatalf("got (%+v, %v), want (zero, false)", access, ok)
		}
	})

	t.Run("a disabled row is the same as no row", func(t *testing.T) {
		sc := adoTestSiteConfig(true)
		s := newSCMTestServer(t, sc, true)
		access, ok := s.computeSCMAccessFor(context.Background(), sc, "alice")
		if ok || access.State != "" {
			t.Fatalf("got (%+v, %v), want (zero, false)", access, ok)
		}
	})

	t.Run("per-user row, never signed in: not_configured with the row-is-newer cause", func(t *testing.T) {
		sc := adoTestSiteConfig(false)
		s := newSCMTestServer(t, sc, true)
		access, ok := s.computeSCMAccessFor(context.Background(), sc, "alice")
		if !ok || access.State != modelAccessNotConfigured || access.Cause != scmAccessCauseRowIsNewer {
			t.Fatalf("got %+v ok=%v, want state=not_configured cause=row_is_newer", access, ok)
		}
	})

	t.Run("per-user row, a captured sign-in: live, sourced from the blob", func(t *testing.T) {
		sc := adoTestSiteConfig(false)
		s := newSCMTestServer(t, sc, true)
		ctx := context.Background()
		if err := s.storeADOEntraBlob(ctx, "alice", scmTestRowID, adoEntraBlob{
			RefreshToken: "rt", Scopes: []string{"vso.code"}, TenantID: "tenant-1", ClientID: "client-1",
			Subject: "alice", Source: adoEntraSourceLogin,
		}); err != nil {
			t.Fatalf("store blob: %v", err)
		}
		access, ok := s.computeSCMAccessFor(ctx, sc, "alice")
		if !ok || access.State != modelAccessLive || access.Source != scmAccessSourceOrg {
			t.Fatalf("got %+v ok=%v, want state=live source=org", access, ok)
		}
	})

	t.Run("per-user row, a DIFFERENT person's captured sign-in is invisible", func(t *testing.T) {
		sc := adoTestSiteConfig(false)
		s := newSCMTestServer(t, sc, true)
		ctx := context.Background()
		if err := s.storeADOEntraBlob(ctx, "bob", scmTestRowID, adoEntraBlob{
			RefreshToken: "rt", Scopes: []string{"vso.code"}, TenantID: "tenant-1", ClientID: "client-1",
			Subject: "bob", Source: adoEntraSourceSignIn,
		}); err != nil {
			t.Fatalf("store blob: %v", err)
		}
		access, ok := s.computeSCMAccessFor(ctx, sc, "alice")
		if !ok || access.State != modelAccessNotConfigured {
			t.Fatalf("alice read bob's connection: got %+v ok=%v", access, ok)
		}
	})

	t.Run("per-user row, a mechanism caller (no OIDC subject): not_applicable", func(t *testing.T) {
		sc := adoTestSiteConfig(false)
		s := newSCMTestServer(t, sc, true)
		access, ok := s.computeSCMAccessFor(context.Background(), sc, "")
		if !ok || access.State != modelAccessNotApplicable {
			t.Fatalf("got %+v ok=%v, want not_applicable", access, ok)
		}
	})

	t.Run("shared row (no entra), nothing stored: shared_expired", func(t *testing.T) {
		sc := adoTestSiteConfig(false)
		s := newSCMTestServer(t, sc, false)
		access, ok := s.computeSCMAccessFor(context.Background(), sc, "alice")
		if !ok || access.State != modelAccessSharedExpired {
			t.Fatalf("got %+v ok=%v, want shared_expired", access, ok)
		}
	})

	t.Run("shared row, a stored git-pat- secret for the row's host: live, no source", func(t *testing.T) {
		sc := adoTestSiteConfig(false)
		s := newSCMTestServer(t, sc, false)
		s.cfg.Secrets.(*memSecrets).m["git-pat-dev-azure-com"] = []byte("x")
		access, ok := s.computeSCMAccessFor(context.Background(), sc, "alice")
		if !ok || access.State != modelAccessLive || access.Source != "" {
			t.Fatalf("got %+v ok=%v, want state=live source=\"\"", access, ok)
		}
		if access.Org != "https://dev.azure.com/contoso" {
			t.Errorf("Org = %q", access.Org)
		}
	})
}

// ── the wire handler ────────────────────────────────────────────────────────

func TestHandleGetSCMAccess(t *testing.T) {
	sc := adoTestSiteConfig(false)
	s := newSCMTestServer(t, sc, true)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me/scm-access", nil)
	r = r.WithContext(withOIDCHuman(r.Context(), "alice"))
	w := httptest.NewRecorder()
	s.handleGetSCMAccess(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"state":"not_configured"`) || !strings.Contains(body, `"cause":"row_is_newer"`) {
		t.Errorf("body = %s", body)
	}
}

// ── the launch door's 422 ───────────────────────────────────────────────────

func TestGitCredentialRefusal(t *testing.T) {
	const repo = "https://dev.azure.com/contoso/proj/_git/repo"

	t.Run("per-user row, no captured sign-in: 422 reason git_credential", func(t *testing.T) {
		sc := adoTestSiteConfig(false)
		s := newSCMTestServer(t, sc, true)
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
		r = r.WithContext(withOIDCHuman(r.Context(), "alice"))
		w := httptest.NewRecorder()
		if !s.gitCredentialRefusal(w, r, repo) {
			t.Fatalf("want refused")
		}
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want 422", w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, `"reason":"git_credential"`) || !strings.Contains(body, gitCredentialNotConnectedRefusal) {
			t.Errorf("body = %s", body)
		}
	})

	t.Run("per-user row, a captured sign-in: not refused", func(t *testing.T) {
		sc := adoTestSiteConfig(false)
		s := newSCMTestServer(t, sc, true)
		ctx := context.Background()
		if err := s.storeADOEntraBlob(ctx, "alice", scmTestRowID, adoEntraBlob{
			RefreshToken: "rt", Scopes: []string{"vso.code"}, TenantID: "tenant-1", ClientID: "client-1",
			Subject: "alice", Source: adoEntraSourceSignIn,
		}); err != nil {
			t.Fatalf("store blob: %v", err)
		}
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(withOIDCHuman(ctx, "alice"))
		w := httptest.NewRecorder()
		if s.gitCredentialRefusal(w, r, repo) {
			t.Fatalf("refused body %q, want admitted", w.Body.String())
		}
	})

	t.Run("shared row (no entra): never refused — nothing per-person to connect", func(t *testing.T) {
		sc := adoTestSiteConfig(false)
		s := newSCMTestServer(t, sc, false)
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
		r = r.WithContext(withOIDCHuman(r.Context(), "alice"))
		w := httptest.NewRecorder()
		if s.gitCredentialRefusal(w, r, repo) {
			t.Fatalf("refused body %q, want a shared row to pass through untouched", w.Body.String())
		}
	})

	t.Run("a repo on no Azure DevOps row: never refused", func(t *testing.T) {
		sc := adoTestSiteConfig(false)
		s := newSCMTestServer(t, sc, true)
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
		r = r.WithContext(withOIDCHuman(r.Context(), "alice"))
		w := httptest.NewRecorder()
		if s.gitCredentialRefusal(w, r, "https://github.com/acme/widgets") {
			t.Fatalf("refused a GitHub repo over an Azure DevOps row's credential")
		}
	})

	t.Run("no session subject at all: never refused — nothing to bind a repair to", func(t *testing.T) {
		sc := adoTestSiteConfig(false)
		s := newSCMTestServer(t, sc, true)
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
		w := httptest.NewRecorder()
		if s.gitCredentialRefusal(w, r, repo) {
			t.Fatalf("refused a mechanism caller, which admitRepoSources already let through")
		}
	})
}

func TestHandleGetSCMAccess_NoRowConfigured_AnswersZeroValueNot404(t *testing.T) {
	s := newSCMTestServer(t, types.SiteConfig{}, false)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me/scm-access", nil)
	r = r.WithContext(withOIDCHuman(r.Context(), "alice"))
	w := httptest.NewRecorder()
	s.handleGetSCMAccess(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	// The absent-row doctrine: a zero SCMAccess (state ""), never a 404 — the
	// console's own "nothing per-principal to say" reading.
	if body := w.Body.String(); !strings.Contains(body, `"state":""`) {
		t.Errorf("body = %s, want the zero-state object", body)
	}
}
