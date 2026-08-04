// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
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
		Header:       row.Header,
		Hosts:        row.Hosts,
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

// integrationByID recomputes the live capability matrix for exactly one row of
// sc.Integrations — the shared response shape the three write handlers below
// return (the same SetupIntegration GET /integrations uses), built directly
// from the just-saved SiteConfig rather than a full effectiveIntegrations
// recompute. Zero value if id is somehow absent (defense only — every caller
// just wrote or found this exact id).
func (s *Server) integrationByID(ctx context.Context, sc types.SiteConfig, id string) SetupIntegration {
	for _, in := range sc.Integrations {
		if in.ID != id {
			continue
		}
		row := integrationRow{Integration: in, Source: "stored"}
		return SetupIntegration{integrationRow: row, Capabilities: capabilitiesFor(toIntegrationView(row), s.liveCapEnv(ctx))}
	}
	return SetupIntegration{}
}

// putIntegrationRequest is the wire body for PUT /integrations/{id}: every
// operator-settable field of a stored Integration. id comes from the URL, not
// the body (mirrors handleDeleteSecret's path-is-authoritative style) — PUT is
// a FULL REPLACEMENT (handlePutSiteConfig's own doctrine: no partial merge),
// so a caller must round-trip a GET first to preserve fields it does not
// intend to change.
type putIntegrationRequest struct {
	Name                 string                    `json:"name"`
	Category             types.IntegrationCategory `json:"category"`
	Type                 string                    `json:"type"`
	Disabled             bool                      `json:"disabled,omitempty"`
	Hosts                []string                  `json:"hosts,omitempty"`
	Header               string                    `json:"header,omitempty"`
	Format               string                    `json:"format,omitempty"`
	Docs                 string                    `json:"docs,omitempty"`
	Credentials          map[string]string         `json:"credentials,omitempty"`
	Config               json.RawMessage           `json:"config,omitempty"`
	DisabledCapabilities []string                  `json:"disabled_capabilities,omitempty"`
	DefaultFor           []string                  `json:"default_for,omitempty"`
}

// handlePutIntegration creates-or-replaces a STORED Integration. operatorOnly
// (same corp-wide blast radius as site-config: an ai_provider row here can
// steer every run's model credential — a strictly larger reach than a single
// workspace/policy write). Validated by validateIntegrationWrite
// (integrations.go) before anything is read/written. DefaultFor uses RADIO
// semantics: naming a mark here CLEARS it from every OTHER stored row in the
// SAME write (applyDefaultForRadio) — never a 409, per the approved spec.
//
//	PUT /api/v1/integrations/{id}
func (s *Server) handlePutIntegration(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req putIntegrationRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	in := types.Integration{
		ID: id, Name: req.Name, Category: req.Category, Type: req.Type,
		Disabled: req.Disabled, Hosts: req.Hosts, Header: req.Header, Format: req.Format, Docs: req.Docs,
		Credentials: req.Credentials, Config: req.Config,
		DisabledCapabilities: req.DisabledCapabilities, DefaultFor: req.DefaultFor,
	}
	if err := validateIntegrationWrite(in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid integration: "+err.Error())
		return
	}
	ctx := r.Context()
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get site config: "+err.Error())
		return
	}
	now := s.cfg.Now().UTC()
	rows := slices.Clone(sc.Integrations)
	if idx := slices.IndexFunc(rows, func(x types.Integration) bool { return x.ID == id }); idx >= 0 {
		in.CreatedAt, in.UpdatedAt = rows[idx].CreatedAt, now
		rows[idx] = in
	} else {
		in.CreatedAt, in.UpdatedAt = now, now
		rows = append(rows, in)
	}
	applyDefaultForRadio(rows, id, in.DefaultFor)
	sc.Integrations = rows
	saved, err := s.cfg.Store.PutSiteConfig(ctx, sc)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "put site config: "+err.Error())
		return
	}
	// hosts and header are the two facts an incident review actually needs from
	// this event: what a granted run may now REACH, and what credential header
	// gets presented there. Both are non-secret by construction (Credentials
	// holds names, never values), and neither is recoverable from a later GET
	// once the row is edited again.
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"integration.write", id, "success", mustJSON(map[string]any{
			"category": string(in.Category), "type": in.Type, "default_for": in.DefaultFor,
			"hosts": in.Hosts, "header": in.Header,
		})))
	writeJSON(w, http.StatusOK, s.integrationByID(ctx, saved, id))
}

// handleDeleteIntegration removes a STORED Integration. 404 when id names no
// stored row — a legacy-derived row was never persisted, so there is nothing
// to delete; the operator's underlying secret/config is untouched and the row
// simply keeps reappearing derived on the next read, exactly as if it had
// never been adopted.
//
//	DELETE /api/v1/integrations/{id}
func (s *Server) handleDeleteIntegration(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ctx := r.Context()
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get site config: "+err.Error())
		return
	}
	idx := slices.IndexFunc(sc.Integrations, func(x types.Integration) bool { return x.ID == id })
	if idx < 0 {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no stored integration %q", id))
		return
	}
	gone := sc.Integrations[idx]
	sc.Integrations = slices.Delete(slices.Clone(sc.Integrations), idx, idx+1)
	if _, err := s.cfg.Store.PutSiteConfig(ctx, sc); err != nil {
		writeError(w, http.StatusInternalServerError, "put site config: "+err.Error())
		return
	}
	// Record what stopped being reachable — after the delete the row is gone,
	// so this event is the only remaining answer to "what did that one open?".
	// The operator's underlying secrets are NOT deleted (handleDeleteIntegration's
	// contract), which is why the credential refs are worth keeping too.
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"integration.delete", id, "success", mustJSON(map[string]any{
			"category": string(gone.Category), "type": gone.Type,
			"hosts": gone.Hosts, "credentials": gone.Credentials,
		})))
	w.WriteHeader(http.StatusNoContent)
}

// handleAdoptIntegration persists a DERIVED legacy row VERBATIM so it becomes
// editable. 409 when id already names a STORED row (nothing to adopt — it
// already is one); 404 when id names neither a stored nor a legacy-derived
// row.
//
//	POST /api/v1/integrations/{id}/adopt
func (s *Server) handleAdoptIntegration(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ctx := r.Context()
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get site config: "+err.Error())
		return
	}
	stored := make(map[string]bool, len(sc.Integrations))
	for _, in := range sc.Integrations {
		stored[in.ID] = true
	}
	if stored[id] {
		writeError(w, http.StatusConflict, fmt.Sprintf("integration %q is already stored", id))
		return
	}
	legacy := s.legacyIntegrations(ctx, sc, stored)
	idx := slices.IndexFunc(legacy, func(row integrationRow) bool { return row.ID == id })
	if idx < 0 {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no derived integration %q to adopt", id))
		return
	}
	now := s.cfg.Now().UTC()
	in := legacy[idx].Integration
	in.CreatedAt, in.UpdatedAt = now, now
	rows := append(slices.Clone(sc.Integrations), in)
	applyDefaultForRadio(rows, in.ID, in.DefaultFor) // defensive; a derived row never carries DefaultFor today
	sc.Integrations = rows
	saved, err := s.cfg.Store.PutSiteConfig(ctx, sc)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "put site config: "+err.Error())
		return
	}
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"integration.adopt", id, "success", mustJSON(map[string]any{"category": string(in.Category), "type": in.Type})))
	writeJSON(w, http.StatusOK, s.integrationByID(ctx, saved, id))
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
