// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	r3IntegSecretRef = "acme-artifactory-token"
	r3IntegEgress    = "artifacts.corp.internal"
	r3IntegConfig    = "https://artifacts.corp.internal/api"
	r3IntegDocs      = "https://wiki.corp.internal/artifactory"
)

// r3IntegStore serves one SiteConfig carrying a stored integration with a
// credential ref, an internal egress host, operator config and an internal docs
// link — the exact row shape GET /site-config answers a member 403 for.
type r3IntegStore struct{ r3PlainStore }

func (r3IntegStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{Integrations: []types.Integration{{
		ID: "corp-artifactory", Name: "Corp Artifactory", Kind: "artifactory",
		Secrets: []types.IntegrationSecret{{Role: "token", SecretName: r3IntegSecretRef}},
		Egress:  []string{r3IntegEgress},
		Config:  map[string]any{"base_url": r3IntegConfig},
		Docs:    r3IntegDocs,
	}}}, nil
}
func (r3IntegStore) ListRoleMappings(context.Context) ([]types.RoleMapping, error) { return nil, nil }
func (r3IntegStore) ListRuns(context.Context) ([]types.AgentRun, error)            { return nil, nil }
func (r3IntegStore) ListRunsPage(context.Context, store.Page) ([]types.AgentRun, error) {
	return nil, nil
}

// TestIntegrationProjectionWithholdsCredentialRefs is F247 (GET /integrations)
// and F250 (GET /setup/status) — one projection, both routes.
//
// routes.go called GET /integrations "the same RBAC posture as site-config's
// GET", and that claim went false when site-config's GET moved to operatorOnly
// BECAUSE integrations[].secrets[].secret_name is a credential ref. A member
// then read that same secret_name, the internal egress host and the operator's
// connection config from two other routes at 200 while the document embedding
// the identical rows answered them 403 — which made the narrowing cosmetic.
//
// /setup/status is the sharper form of it: redactSetupStatusForMember drops
// SetupSecrets.Present as "secret NAMES" and shipped
// integrations[].secrets[].secret_name in the SAME body.
func TestIntegrationProjectionWithholdsCredentialRefs(t *testing.T) {
	newSrv := func(t *testing.T) *Server {
		t.Helper()
		h := newHarness(t)
		cfg := baseTestConfig(h, r3IntegStore{})
		cfg.OIDC = &oidc.Authenticator{}
		return New(cfg)
	}
	withheld := []string{r3IntegSecretRef, r3IntegEgress, r3IntegConfig, r3IntegDocs}

	for _, path := range []string{"/api/v1/integrations", "/api/v1/setup/status"} {
		t.Run("a member reading "+path, func(t *testing.T) {
			srv := newSrv(t)
			w := doSSO(t, srv, http.MethodGet, path,
				ssoSession(t, "sub-plain-member", "m@corp.example", oidc.RoleMember), "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200; body=%s", path, w.Code, w.Body.String())
			}
			body := w.Body.String()
			for _, bad := range withheld {
				if strings.Contains(body, bad) {
					t.Errorf("GET %s leaked %q to a member, while GET /site-config answers that same session 403 "+
						"for the row this is a copy of.\nbody=%s", path, bad, body)
				}
			}
			// STILL USEFUL: the launch card picks an integration by identity.
			for _, want := range []string{"corp-artifactory", "Corp Artifactory", "artifactory"} {
				if !strings.Contains(body, want) {
					t.Errorf("GET %s dropped %q — the projection must keep what the run-launch UI renders, "+
						"or it is a tier move wearing a projection's clothes.\nbody=%s", path, want, body)
				}
			}
		})

		t.Run("an operator reading "+path+" still sees everything", func(t *testing.T) {
			srv := newSrv(t)
			w := doSSO(t, srv, http.MethodGet, path,
				ssoSession(t, "sub-super", "super@corp.example", oidc.RoleAdmin), "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200; body=%s", path, w.Code, w.Body.String())
			}
			for _, want := range withheld {
				if !strings.Contains(w.Body.String(), want) {
					t.Errorf("GET %s withheld %q from an OPERATOR; this is a projection for non-operators, "+
						"not a deletion.\nbody=%s", path, want, w.Body.String())
				}
			}
		})
	}

	// The projection must not edit the caller's rows in place: /setup/status
	// computes the integration list ONCE per request and hands the same value to
	// its own response, so an in-place redaction would reach an operator's copy.
	t.Run("the projection leaves its input untouched", func(t *testing.T) {
		in := []SetupIntegration{{integrationRow: integrationRow{Integration: types.Integration{
			ID: "x", Egress: []string{r3IntegEgress},
			Secrets: []types.IntegrationSecret{{SecretName: r3IntegSecretRef}},
		}}}}
		_ = memberSafeIntegrations(in)
		if len(in[0].Secrets) != 1 || in[0].Secrets[0].SecretName != r3IntegSecretRef || len(in[0].Egress) != 1 {
			t.Errorf("memberSafeIntegrations mutated its input: %+v", in[0])
		}
	})
}
