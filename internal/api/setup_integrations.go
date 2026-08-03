// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
)

// setup_integrations.go wires the Integrations entity (integrations.go) into
// the live server and the first-run setup surface:
//   - toIntegrationView/liveCapEnv adapt the real store-backed data
//     (integrationRow, Server config/secrets) into the pure capability
//     matrix's input shapes (integrationView/capEnv, defined in
//     integrations.go) WITHOUT changing that file, per its own doc comment.
//   - SetupIntegration is the wire shape GET /integrations and
//     SetupStatus.Integrations both return: an effective row (stored or
//     legacy-derived) plus its live capabilities.
//   - SetupHarnessTool/setupHarnessTools project the static coding-agent
//     harness catalog (harnessCatalog, harness.go) for SetupStatus.Harnesses.

// toIntegrationView adapts an integrationRow into the shape capabilitiesFor
// consumes. A malformed stored Config just yields fewer capability facts (a
// nil map, same as "no config") rather than an error surface here — the
// write path (a later wave) is where a bad Config should be rejected, not
// this read path.
func toIntegrationView(row integrationRow) integrationView {
	var cfg map[string]any
	if len(row.Config) > 0 {
		_ = json.Unmarshal(row.Config, &cfg)
	}
	return integrationView{
		ID:           row.ID,
		Category:     string(row.Category),
		Type:         row.Type,
		Disabled:     row.Disabled,
		Credentials:  row.Credentials,
		Config:       cfg,
		DisabledCaps: row.DisabledCapabilities,
	}
}

// liveCapEnv builds a capEnv from the server's actual live config/secret
// store — the readiness signals capabilitiesFor cannot derive from an
// integrationView alone. Read-only throughout (Peek/host-CLI detection,
// never a refresh or a write).
func (s *Server) liveCapEnv(ctx context.Context) capEnv {
	present := s.presentSecretNames(ctx)
	providers, _ := s.setupProviders()
	residentLive := false
	if s.cfg.SubscriptionToken != nil {
		if tok, err := s.cfg.SubscriptionToken.Peek(); err == nil && tok.Value != "" {
			residentLive = true
		}
	}
	return capEnv{
		SecretPresent:    func(name string) bool { return present[name] },
		HostLike:         deploymentHostLike(providers),
		BedrockRegionSet: s.cfg.BedrockRegion != "",
		BedrockModelSet:  s.cfg.BedrockModel != "",
		ManagedBlobPresent: func(provider string) bool {
			_, ok, err := s.readManagedBlob(ctx, provider)
			return err == nil && ok
		},
		ResidentSubscriptionLive: residentLive,
	}
}

// SetupIntegration is one integration as the UI sees it: the effective row
// (stored or legacy-derived, integrationRow) plus its live capability
// matrix. Shared wire shape for GET /integrations and
// SetupStatus.Integrations, so there is exactly one representation of "an
// integration" on the wire.
type SetupIntegration struct {
	integrationRow
	Capabilities []Capability `json:"capabilities"`
}

// integrationsWithCapabilities computes the full effective integration set
// enriched with each row's live capabilities — the one computation GET
// /integrations and SetupStatus.Integrations both call, so they can never
// disagree.
func (s *Server) integrationsWithCapabilities(ctx context.Context) []SetupIntegration {
	rows := s.effectiveIntegrations(ctx)
	env := s.liveCapEnv(ctx)
	out := make([]SetupIntegration, len(rows))
	for i, row := range rows {
		out[i] = SetupIntegration{integrationRow: row, Capabilities: capabilitiesFor(toIntegrationView(row), env)}
	}
	return out
}

// handleListIntegrations returns the effective integration set (stored ∪
// legacy-derived) with each row's live capabilities. Read-only, humanOrAdmin
// (same posture as GET /site-config, not operator-only): Credentials only
// ever holds secret NAMES, never values, so this is safe for any
// authenticated human. No audit — this is a read, like GET /site-config.
//
//	GET /api/v1/integrations
func (s *Server) handleListIntegrations(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"integrations": s.integrationsWithCapabilities(r.Context())})
}

// SetupHarnessTool is one coding-agent harness's static catalog metadata
// (harnessCatalog, harness.go): display name, plus whether Wardyn can wire it
// a managed model credential (Gateway) and/or a container-login subscription
// (Login), or whether it is the BYOA row (NoManagedAuth). This is the STATIC
// "what tools does Wardyn know how to run" catalog — distinct from
// SetupHarness, which reports a CAPTURED credential's live readiness.
type SetupHarnessTool struct {
	ID            string `json:"id"`
	Display       string `json:"display"`
	HasGateway    bool   `json:"has_gateway"`
	HasLogin      bool   `json:"has_login"`
	NoManagedAuth bool   `json:"no_managed_auth,omitempty"`
}

// setupHarnessTools projects the static harness catalog for SetupStatus.
func setupHarnessTools() []SetupHarnessTool {
	out := make([]SetupHarnessTool, len(harnessCatalog))
	for i, d := range harnessCatalog {
		out[i] = SetupHarnessTool{
			ID: d.ID, Display: d.Display,
			HasGateway: d.Gateway != nil, HasLogin: d.Login != nil,
			NoManagedAuth: d.NoManagedAuth,
		}
	}
	return out
}
