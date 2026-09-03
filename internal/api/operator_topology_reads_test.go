// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// topologyStore holds one populated row of each admin-authored document, spelled
// with values a reader can recognise as the operator's own estate.
type topologyStore struct {
	store.Store
	cfg    types.SiteConfig
	srcs   []types.Source
	images []types.BaseImageEntry
}

func (s *topologyStore) GetSiteConfig(context.Context) (types.SiteConfig, error) { return s.cfg, nil }
func (s *topologyStore) ListSources(context.Context) ([]types.Source, error)     { return s.srcs, nil }
func (s *topologyStore) ListBaseImages(context.Context) ([]types.BaseImageEntry, error) {
	return s.images, nil
}
func (s *topologyStore) GetSource(_ context.Context, id uuid.UUID) (types.Source, error) {
	for _, x := range s.srcs {
		if x.ID == id {
			return x, nil
		}
	}
	return types.Source{}, store.ErrNotFound
}

// TestOperatorTopologyReadsAreNotMemberReadable is the tier decision for the
// four admin-authored READS, and — like the record route — it is a TIER
// decision rather than a redaction.
//
// All four had their WRITES on operatorOnly and their READS on the member
// group: a split made by VERB rather than by what the document carries. What it
// carried was the operator's estate. GET /site-config returns the same whole
// document authz_test.go keeps the PUT classAdmin for ("integration credential
// refs included"): upstream_proxy_secret_ref, every
// integrations[].secrets[].secret_name, and the internal proxy / SCM /
// artifact-override / redirect hostnames. A Source row carries Locator — the
// HOST FILESYSTEM PATH of a local_dir source — and requirement keys spelled
// `secret:<name>` / `egress:<host>`. A BaseImageEntry carries the internal
// registry ref and the bootstrap URLs in Steps. No secret VALUES (those are
// write-only), so this is a lateral-movement target list rather than a key —
// handed to every authenticated member by routes the console called on page
// load.
//
// RECLASSIFIED, NOT PROJECTED, and the question was asked before it was
// answered: nothing member-facing consumes them. The console has no client
// method for /sources or /base-images at all, and both /site-config callers
// already wrap the GET in .catch() and render with a null config. Three
// per-route projections would have been new code serving no caller; the
// cheapest redaction is a field nobody asked for.
func TestOperatorTopologyReadsAreNotMemberReadable(t *testing.T) {
	srcID := uuid.New()
	st := &topologyStore{
		cfg: types.SiteConfig{
			UpstreamProxySecretRef: "corp-proxy-password",
			UpstreamProxyURL:       "http://proxy.internal.corp.example:3128",
			ScmHosts:               []string{"ghe.internal.corp.example"},
		},
		srcs: []types.Source{{
			ID: srcID, Kind: types.SourceLocalDir, Locator: "/srv/corp-nfs/finance/payroll-svc",
			Requirements: map[string]types.WorkspaceRequirement{
				"secret:payroll-db-password":         {Level: "required"},
				"egress:vault.internal.corp.example": {Level: "required"},
			},
		}},
		images: []types.BaseImageEntry{{
			ID: uuid.New(), Name: "base", Image: "registry.internal.corp.example/platform/base:1.2.3",
			Steps: []string{"curl -sSL https://artifactory.internal.corp.example/bootstrap.sh | sh"},
		}},
	}
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)

	// The values that must never reach a member, per route. Asserted as STRING
	// ABSENCE from the raw body rather than as struct fields: the leak is
	// whatever the wire actually carries, and a projection that renamed a field
	// or nested it would pass a field-shaped assertion while still shipping the
	// hostname.
	routes := []struct {
		path    string
		secrets []string
	}{
		{"/api/v1/site-config", []string{"corp-proxy-password", "proxy.internal.corp.example", "ghe.internal.corp.example"}},
		{"/api/v1/sources", []string{"/srv/corp-nfs/finance/payroll-svc", "secret:payroll-db-password", "vault.internal.corp.example"}},
		{"/api/v1/sources/" + srcID.String(), []string{"/srv/corp-nfs/finance/payroll-svc", "secret:payroll-db-password"}},
		{"/api/v1/base-images", []string{"registry.internal.corp.example", "artifactory.internal.corp.example"}},
	}

	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleMember)
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)

	for _, rt := range routes {
		t.Run("member is refused "+rt.path, func(t *testing.T) {
			w := doSSO(t, srv, http.MethodGet, rt.path, member, "")
			if w.Code != http.StatusForbidden {
				t.Fatalf("GET %s as a member = %d, want 403 — this route returns operator topology and credential refs; body=%s",
					rt.path, w.Code, w.Body.String())
			}
			for _, secret := range rt.secrets {
				if strings.Contains(w.Body.String(), secret) {
					t.Errorf("the refusal body itself leaks %q: %s", secret, w.Body.String())
				}
			}
		})
	}

	// A RE-TIERING, not a removal: the admin still reads every one of them, and
	// the content is really there — a fixture that served empty documents would
	// pass every absence assertion above and prove nothing.
	for _, rt := range routes {
		t.Run("admin still reads "+rt.path, func(t *testing.T) {
			w := doSSO(t, srv, http.MethodGet, rt.path, admin, "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET %s as an admin = %d, want 200; body=%s", rt.path, w.Code, w.Body.String())
			}
			for _, secret := range rt.secrets {
				if !strings.Contains(w.Body.String(), secret) {
					t.Errorf("admin body is missing %q — the fixture stopped carrying the values this test is about: %s",
						secret, w.Body.String())
				}
			}
		})
	}
}
