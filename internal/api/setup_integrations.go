// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// setup_integrations.go wires the Integrations entity (integrations.go) into
// the live server and the first-run setup surface:
//   - liveCapEnv folds the Server's config/secret store into the capability
//     matrix's external-signal input (capEnv, defined in integrations.go);
//     the row itself goes to capabilitiesFor unchanged.
//   - SetupIntegration is the wire shape GET /integrations and
//     SetupStatus.Integrations both return: an effective row (stored or
//     legacy-derived) plus its live capabilities.
//   - SetupHarnessTool/setupHarnessTools project the static coding-agent
//     harness catalog (harnessCatalog, harness.go) for SetupStatus.Harnesses.

// liveCapEnv builds a capEnv from the server's actual live config/secret
// store — the readiness signals capabilitiesFor cannot derive from an
// integrationView alone. Read-only throughout (Peek/host-CLI detection,
// never a refresh or a write).
//
// present/providers are taken as parameters (PLATFORM-API-7), not recomputed;
// bedrock likewise (its own SetupStatus/GET-/integrations callers already
// compute it once). See integrationsWithCapabilities' doc for why.
func (s *Server) liveCapEnv(ctx context.Context, present map[string]bool, providers []SetupProvider, bedrock SetupBedrock) capEnv {
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
		// mirrors setupBedrock's own four-lane OR (runs_bedrock.go) — reused, not
		// re-derived, so this can never drift from what resolveBedrockAuth accepts.
		BedrockCredentialPresent: bedrock.CredsPresent || bedrock.AWSMount || bedrock.BearerPresent || bedrock.SSOPresent,
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

// UnmarshalJSON decodes the embedded row (see integrationRow.UnmarshalJSON —
// the embedded Integration's custom decoder would otherwise be promoted here
// and swallow capabilities) and then the wrapper's own field.
func (si *SetupIntegration) UnmarshalJSON(b []byte) error {
	if err := json.Unmarshal(b, &si.integrationRow); err != nil {
		return err
	}
	var caps struct {
		Capabilities []Capability `json:"capabilities"`
	}
	if err := json.Unmarshal(b, &caps); err != nil {
		return err
	}
	si.Capabilities = caps.Capabilities
	return nil
}

// integrationsWithCapabilities computes the full effective integration set
// enriched with each row's live capabilities — the one computation GET
// /integrations and SetupStatus.Integrations both call, so they can never
// disagree. `present` is the CALLER's presence map — owner-scoped for a
// request (presentSecretNamesFor with secretOwnerFromRequest), so a member's
// own key lists exactly as it resolves in their run; operator-wide elsewhere.
func (s *Server) integrationsWithCapabilities(ctx context.Context, present map[string]bool) []SetupIntegration {
	providers, _ := s.setupProviders()
	return s.integrationsWithCapabilitiesUsing(ctx, present, providers, s.setupBedrock(ctx, present))
}

// integrationsWithCapabilitiesUsing is integrationsWithCapabilities' pure-ish
// half, taking the three live signals capabilitiesFor's inputs are built from
// (present secret names, detected providers, Bedrock readiness) as parameters
// instead of recomputing them (PLATFORM-API-7): /setup/status ALREADY
// computes all three for its own checklist rows, and this used to silently
// redo each 1-2 more times on the SAME polled request — a full secret
// listing, a filesystem CLI-detection sweep + subscription peek, an
// AWS-SSO-blob age decrypt, each 2-3x instead of once.
func (s *Server) integrationsWithCapabilitiesUsing(ctx context.Context, present map[string]bool, providers []SetupProvider, bedrock SetupBedrock) []SetupIntegration {
	rows := s.effectiveIntegrations(ctx, present, bedrock)
	env := s.liveCapEnv(ctx, present, providers, bedrock)
	out := make([]SetupIntegration, len(rows))
	for i, row := range rows {
		out[i] = SetupIntegration{integrationRow: row, Capabilities: capabilitiesFor(row.Integration, env)}
	}
	return out
}

// memberSafeIntegration is the ONE projection of an integration row for a
// non-operator, used by both routes that publish these rows: GET /integrations
// and GET /setup/status.
//
// WHAT IT DROPS AND WHY. secrets[].secret_name is a credential REF — the exact
// datum routes.go names as the reason GET /site-config is admin-only ("every
// integrations[].secrets[].secret_name are credential REFS"). egress[] is where
// the system LIVES: internal hostnames, the same class R1 narrowed /sources and
// /base-images for. config[] is operator-authored connection detail (base_url,
// endpoint_hint, app_id/installation_id) and docs[] is wherever the operator
// documented the internal system. None of it is readable by a member on
// /site-config, and serving the identical rows through two other routes made
// that narrowing cosmetic — a member could read "acme-artifactory-token" from
// GET /integrations while GET /site-config answered them 403.
//
// WHAT IT KEEPS is exactly what the run-launch UI renders: identity (id, name,
// kind), whether it is off, what it is the default for, and the live capability
// matrix — enough to choose an integration for a run, and nothing about how the
// platform reaches it. Source stays too: "stored" vs "legacy" is not topology.
//
// THE CAPABILITY MATRIX IS DERIVED FROM WHAT THIS DROPS, so nil-ing the four
// fields is not enough on its own: capabilitiesFor computes each cell FROM the
// row, and a needs_setup cell states why — gatedCap/secretGate interpolate the
// credential ref verbatim ("secret %q not stored", integrations.go). A member
// therefore read acme-artifactory-token out of capabilities[].reason in the
// very response whose secrets[] had been emptied to withhold it. The cell's
// ANSWER is not the leak and is kept (a member must still learn the capability
// is unusable); the derived TEXT is scrubbed of anything the projection
// withheld — see memberSafeCapabilities.
func memberSafeIntegration(in SetupIntegration) SetupIntegration {
	in.Capabilities = memberSafeCapabilities(in.Capabilities, in.Integration)
	in.Secrets = nil
	in.Egress = nil
	in.Config = nil
	in.Docs = ""
	return in
}

// memberSafeCapabilities is the anti-forgetting half of the projection: a
// member-visible DERIVED string may not restate a datum the projection
// withheld. Rather than allowlisting today's reason sentences (which the canon
// pass rewrites) or teaching this file which reasons happen to interpolate a
// ref (which the next capability cell would silently break), it compares each
// reason against the row's OWN withheld values and replaces the whole sentence
// when one appears. A new cell that interpolates a secret name, an egress host
// or an operator config value is therefore projected correctly the day it is
// written, without anyone remembering this rule.
//
// The replacement is secretGate's already-shipped ref-less sentence, not a new
// string: it is what this same field already says for the OTHER half of the
// same gate (a capability whose credential ref is unset), and it is the honest
// member-facing form of every case here — the credential this cell needs is not
// usable, and which credential it is belongs to the operator.
//
// Non-mutating, like memberSafeIntegrations: both publishing routes share one
// value computed per request, so editing the slice in place would reach an
// operator's copy.
func memberSafeCapabilities(caps []Capability, in types.Integration) []Capability {
	if len(caps) == 0 {
		return caps
	}
	withheld := withheldIntegrationValues(in)
	out := make([]Capability, len(caps))
	copy(out, caps)
	for i := range out {
		if out[i].Reason == "" {
			continue
		}
		for _, bad := range withheld {
			if strings.Contains(out[i].Reason, bad) {
				out[i].Reason = reasonCredentialWithheld
				break
			}
		}
	}
	return out
}

// reasonCredentialWithheld is secretGate's own ref-less sentence
// (integrations.go), reused verbatim so this projection introduces no new
// member-facing copy.
const reasonCredentialWithheld = "no credential configured"

// withheldIntegrationValues lists the strings memberSafeIntegration drops from
// a row: the credential refs, the egress entries (and their bare hosts, since a
// reason may name the host without the ":port" the entry carries), the
// operator's config values and the docs link. Empty and 1-2 character values
// are skipped — they cannot identify a host or a credential, and would blank
// every reason that happens to contain the fragment.
func withheldIntegrationValues(in types.Integration) []string {
	var out []string
	add := func(v string) {
		if v = strings.TrimSpace(v); len(v) > 2 {
			out = append(out, v)
		}
	}
	for _, sec := range in.Secrets {
		add(sec.SecretName)
	}
	for _, host := range in.Egress {
		add(host)
		if h, _, ok := strings.Cut(host, ":"); ok {
			add(h)
		}
	}
	for _, v := range in.Config {
		add(fmt.Sprint(v))
	}
	add(in.Docs)
	return out
}

// memberSafeIntegrations projects a whole list, leaving the caller's slice
// untouched — both call sites share a value computed once per request
// (integrationsWithCapabilitiesUsing), so editing in place would redact an
// operator's own copy.
func memberSafeIntegrations(rows []SetupIntegration) []SetupIntegration {
	if len(rows) == 0 {
		return rows
	}
	out := make([]SetupIntegration, len(rows))
	for i, in := range rows {
		out[i] = memberSafeIntegration(in)
	}
	return out
}

// handleListIntegrations returns the effective integration set (stored ∪
// legacy-derived) with each row's live capabilities. Read-only, humanOrAdmin.
//
// PROJECTED FOR A NON-OPERATOR (memberSafeIntegration). This route used to
// justify its wide tier with "Credentials only ever holds secret NAMES, never
// values, so this is safe for any authenticated human" — and routes.go called it
// "the same RBAC posture as site-config's GET". Both went false when site-config's
// GET moved to operatorOnly precisely BECAUSE a secret name is a credential ref:
// a member read secrets[].secret_name and the internal egress hosts here at 200
// while the document that embeds the identical rows answered them 403. The tier
// stays (the console's launch card needs the identity and capability half); the
// credential refs, hosts and operator config do not cross it.
//
// No audit — this is a read, like GET /site-config.
//
//	GET /api/v1/integrations
func (s *Server) handleListIntegrations(w http.ResponseWriter, r *http.Request) {
	present := s.presentSecretNamesFor(r.Context(), s.secretOwnerFromRequest(r))
	rows := s.integrationsWithCapabilities(r.Context(), present)
	if !s.isOperator(r.Context()) {
		rows = memberSafeIntegrations(rows)
	}
	writeJSON(w, http.StatusOK, map[string]any{"integrations": rows})
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
		present := s.presentSecretNames(ctx)
		providers, _ := s.setupProviders()
		bedrock := s.setupBedrock(ctx, present)
		return SetupIntegration{integrationRow: row, Capabilities: capabilitiesFor(row.Integration, s.liveCapEnv(ctx, present, providers, bedrock))}
	}
	return SetupIntegration{}
}

// putIntegrationRequest is the wire body for PUT /integrations/{id}: every
// operator-settable field of a stored Integration, and ONLY those — name, kind,
// disabled, secrets, egress, config, docs, disabled_capabilities, default_for.
// id comes from the URL, not the body (mirrors handleDeleteSecret's
// path-is-authoritative style).
//
// PUT is a FULL REPLACEMENT (handlePutSiteConfig's own doctrine: no partial
// merge), so send every field you want to keep — but the body is NOT a GET
// document. GET /integrations emits five fields the server owns and this DTO
// has no home for — id, created_at, updated_at, source, capabilities — and the
// decode is STRICT, so PUTting a GET row back unchanged is a 400 naming the
// first of them, not a round trip. Take the GET row, drop those five, send the
// rest. (Strict stays deliberately: this write steers every run's model
// credential, so a misspelled field name has to 400 rather than silently
// resolve to the zero value.)
type putIntegrationRequest struct {
	Name                 string                    `json:"name"`
	Kind                 string                    `json:"kind"`
	Disabled             bool                      `json:"disabled,omitempty"`
	Secrets              []types.IntegrationSecret `json:"secrets,omitempty"`
	Egress               []string                  `json:"egress,omitempty"`
	Config               map[string]any            `json:"config,omitempty"`
	Docs                 string                    `json:"docs,omitempty"`
	DisabledCapabilities []string                  `json:"disabled_capabilities,omitempty"`
	DefaultFor           []string                  `json:"default_for,omitempty"`
}

// handlePutIntegration creates-or-replaces a STORED Integration. operatorOnly
// (same corp-wide blast radius as site-config: an AI-provider row here can
// steer every run's model credential — a strictly larger reach than a single
// workspace/policy write). Validated by validateIntegrationWrite
// (integrations.go) before anything is read/written. DefaultFor uses RADIO
// semantics: naming a mark here CLEARS it from every OTHER stored row in the
// SAME write (applyDefaultForRadio) — never a 409, per the approved spec.
//
// ADOPTION: the write IS the adoption now. A PUT whose id so far exists only
// as a DERIVATION simply stores it — the explicit POST {id}/adopt promotion
// route left with the integration catalog, and a 409 naming a route the
// router no longer registers would be a dead end. Same audit event either
// way; the in-handler comment below carries the full story.
//
// integrationIDParam reads the {id} route param UNESCAPED: chi hands handlers
// the raw path segment, and adopted legacy ids legitimately contain colons
// ("anthropic_subscription:managed") which clients percent-encode — without
// this, the lookup sees "…%3Amanaged" and honestly-but-wrongly 404s.
func integrationIDParam(r *http.Request) string {
	id := chi.URLParam(r, "id")
	if dec, err := url.PathUnescape(id); err == nil {
		return dec
	}
	return id
}

// PUT /api/v1/integrations/{id}
func (s *Server) handlePutIntegration(w http.ResponseWriter, r *http.Request) {
	id := integrationIDParam(r)
	var req putIntegrationRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	in := types.Integration{
		ID: id, Name: req.Name, Kind: req.Kind,
		Disabled: req.Disabled, Secrets: req.Secrets, Egress: req.Egress,
		Config: req.Config, Docs: req.Docs,
		DisabledCapabilities: req.DisabledCapabilities, DefaultFor: req.DefaultFor,
	}
	if err := validateIntegrationWrite(in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid integration: "+err.Error())
		return
	}
	ctx := r.Context()
	// SEAM-1: serializes this read-modify-write against the site config's
	// other two writers (handleDeleteIntegration, handlePutSiteConfig) — see
	// handlePutIntegration's own SEAM-1 comment for why an
	// unguarded RMW here can silently erase a concurrent one.
	s.siteConfigMu.Lock()
	defer s.siteConfigMu.Unlock()
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
		// A PUT onto an id that so far exists only as a DERIVED row simply
		// stores it. This used to 409 and demand an explicit POST .../adopt
		// first, so that freezing a copy of live config was a deliberate act.
		// That endpoint is gone with the integration catalog, and a 409 naming
		// a route the router no longer registers is a dead end — the write IS
		// the adoption now, and it carries the same audit event either way.
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
	// egress and header are the two facts an incident review actually needs
	// from this event: what a granted run may now REACH, and what credential
	// header gets presented there. Both are non-secret by construction (Secrets
	// holds names, never values), and neither is recoverable from a later GET
	// once the row is edited again.
	_, auditHeader, _, _ := in.HeaderSecret()
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"integration.write", id, "success", mustJSON(map[string]any{
			"kind": in.Kind, "default_for": in.DefaultFor,
			"egress": in.Egress, "header": auditHeader,
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
	id := integrationIDParam(r)
	ctx := r.Context()
	// SEAM-1: see handlePutIntegration's comment.
	s.siteConfigMu.Lock()
	defer s.siteConfigMu.Unlock()
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
			"kind": gone.Kind, "egress": gone.Egress, "credentials": gone.CredentialsMap(),
		})))
	w.WriteHeader(http.StatusNoContent)
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
