// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The model-provider endpoints: GET/PUT /model-providers (admin) and the
// member-safe projection every person reads on GET /setup/status. The write
// boundary itself — normalization, validation, UIDs — is model_providers.go's.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"slices"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// DRAFT (M2 canon pending).
const (
	mp412Stale        = "model providers changed since you loaded them — reload and retry"
	mp400StillDefault = "model_providers: %q is the default for %q — choose another default first"
)

// mountModelProviderRoutes registers the two model-provider endpoints,
// operatorOnly for both, the sibling blocks' reasoning: the records name the
// org's gateways, its AWS access portal and account pins. The member-safe
// projection is a different, narrower document — SetupStatus.ModelProviders.
// There is no DELETE: removing a provider is a PUT without it. Each person's
// own credential and sign-in doors mount on the authenticated group r beside
// them.
func (s *Server) mountModelProviderRoutes(r, operatorOnly chi.Router) {
	operatorOnly.Get("/model-providers", s.handleGetModelProviders)
	operatorOnly.Put("/model-providers", s.handlePutModelProviders)
	s.mountModelProviderCredentialRoutes(r)
	s.mountProviderSignInRoutes(r)
}

// mountProviderRoutes mounts the three operatorOnly provider documents —
// workspace, agent and model, plus each person's own model-provider credential
// door on r — as one line in routes(), which sits at its funlen ratchet.
func (s *Server) mountProviderRoutes(r, operatorOnly chi.Router) {
	s.mountWorkspaceProviderRoutes(operatorOnly)
	s.mountAgentProviderRoutes(operatorOnly)
	s.mountModelProviderRoutes(r, operatorOnly)
}

// storedModelProviders is the stored block as a VALUE — what GET returns and
// what both verbs' ETag is computed over.
func storedModelProviders(sc types.SiteConfig) types.ModelProviders {
	if sc.ModelProviders == nil {
		return types.ModelProviders{}
	}
	return *sc.ModelProviders
}

// modelProvidersRead is GET's body: the stored block, and ConnectedPeople —
// per provider ID, how many people hold a credential of their own for it
// (#970). Read-only: PUT's strict decode refuses the field, and the ETag
// covers the stored block alone, so a person connecting never stales an
// admin's edit. Admins only, as the route is: a member never reads this list.
type modelProvidersRead struct {
	types.ModelProviders
	ConnectedPeople map[string]int `json:"connected_people,omitempty"`
}

// handleGetModelProviders returns the stored records with an ETag and each
// one's connected-people count; a never-configured install gets {} with 200.
//
//	GET /api/v1/model-providers
func (s *Server) handleGetModelProviders(w http.ResponseWriter, r *http.Request) {
	sc, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return
	}
	block := storedModelProviders(sc)
	connected, err := s.connectedPeople(r.Context(), block)
	if err != nil {
		writeServerError(w, r, "count connected people", err)
		return
	}
	w.Header().Set("ETag", computeETag(block))
	writeJSON(w, http.StatusOK, modelProvidersRead{ModelProviders: block, ConnectedPeople: connected})
}

// handlePutModelProviders replaces the WHOLE block; {} is the clear form.
// If-Match is optional optimistic concurrency, as on the sibling blocks.
// Removing a provider, or unticking the harness it is the default for, is
// refused while the agent roster names it — the admin chooses another default
// first. Turning one off is not: that is the incident switch. Adding a Claude
// subscription is refused until its sign-in image resolves (E4); keeping or
// turning off one already stored never is.
//
//	PUT /api/v1/model-providers
func (s *Server) handlePutModelProviders(w http.ResponseWriter, r *http.Request) {
	var body types.ModelProviders
	if !decodeStrict(w, r, &body) {
		return
	}
	block := normalizeModelProviders(&body)
	if err := validateModelProviders(block); err != nil {
		writeError(w, http.StatusBadRequest, "invalid model providers: "+err.Error())
		return
	}
	ctx := r.Context()
	imageOK := s.claudeSignInImageOK(ctx, block)
	// SEAM-1, handlePutAgentProviders's reason: the same singleton document.
	s.siteConfigMu.Lock()
	defer s.siteConfigMu.Unlock()
	existing, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return
	}
	if !ifMatchSatisfied(r, computeETag(storedModelProviders(existing))) {
		writeError(w, http.StatusPreconditionFailed, mp412Stale)
		return
	}
	if msg := stillDefaultRefusal(existing.AgentProviders, block); msg != "" {
		writeError(w, http.StatusBadRequest, "invalid model providers: "+msg)
		return
	}
	if err := validateModelProviderImagePrereqs(block, existing.ModelProviders, imageOK); err != nil {
		writeError(w, http.StatusBadRequest, "invalid model providers: "+err.Error())
		return
	}
	assignModelProviderUIDs(block, existing.ModelProviders)
	invalidated, err := s.purgeProviderCredentials(ctx, existing.ModelProviders, block)
	if err != nil {
		writeServerError(w, r, "purge model provider credentials", err)
		return
	}
	candidate := existing
	candidate.ModelProviders = block
	candidate.EffectiveScmHosts = nil
	saved, err := s.cfg.Store.PutSiteConfig(ctx, candidate)
	if err != nil {
		writeServerError(w, r, "put site config", err)
		return
	}
	savedBlock := storedModelProviders(saved)
	datum := modelProviderAuditData(storedModelProviders(existing), savedBlock)
	datum["per_user_credentials_invalidated"] = invalidated
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"model_provider.write", "model_providers", "success", mustJSON(datum)))
	w.Header().Set("ETag", computeETag(savedBlock))
	writeJSON(w, http.StatusOK, savedBlock)
}

// stillDefaultRefusal is this door's spelling of validateDefaultProviders: the
// roster is not what the caller is editing, so the sentence names the provider
// being removed rather than the roster row.
func stillDefaultRefusal(roster *types.AgentProviders, block *types.ModelProviders) string {
	if roster == nil {
		return ""
	}
	for _, row := range roster.Agents {
		if row.DefaultProvider == "" {
			continue
		}
		if mp, ok := modelProviderByID(block, row.DefaultProvider); !ok || !mp.Serves(row.ID) {
			return fmt.Sprintf(mp400StillDefault, row.DefaultProvider, row.ID)
		}
	}
	return ""
}

// modelProviderAuditData is model_provider.write's datum: which providers, of
// which kinds, serving which harnesses, which are off, which AWS identity each
// Bedrock SSO provider pins, and which had their address or header scheme
// changed (the change that, once people hold credentials for a provider,
// invalidates every one of them), and which changed kind — and so got a fresh
// UID (assignModelProviderUIDs), leaving every credential given for the old
// kind to no provider. Never a base URL or a start URL: the audit log is read by
// more people than the providers page is.
func modelProviderAuditData(before, after types.ModelProviders) map[string]any {
	ids, kinds, pairs, disabled, pins, changed := []string{}, []string{}, []string{}, []string{}, []string{}, []string{}
	rekinded := []string{}
	prior, priorByID := map[string]types.ModelProvider{}, map[string]types.ModelProvider{}
	for _, p := range before.Providers {
		prior[p.UID], priorByID[p.ID] = p, p
	}
	for _, p := range after.Providers {
		ids = append(ids, p.ID)
		if !slices.Contains(kinds, string(p.Kind)) {
			kinds = append(kinds, string(p.Kind))
		}
		for _, h := range p.Harnesses {
			pairs = append(pairs, h.Harness+":"+p.ID)
		}
		if p.Disabled {
			disabled = append(disabled, p.ID)
		}
		if p.Bedrock != nil && p.Bedrock.SSOAccountID != "" {
			pins = append(pins, p.ID+":"+p.Bedrock.SSOAccountID+"/"+p.Bedrock.SSORoleName)
		}
		if old, ok := prior[p.UID]; ok && providerAddressChanged(old, p) {
			changed = append(changed, p.ID)
		}
		if old, ok := priorByID[p.ID]; ok && old.Kind != p.Kind {
			rekinded = append(rekinded, p.ID)
		}
	}
	for _, l := range [][]string{ids, kinds, pairs, disabled, pins, changed, rekinded} {
		slices.Sort(l)
	}
	return map[string]any{
		"provider_count": len(after.Providers), "ids": ids, "kinds": kinds, "harnesses": pairs,
		"disabled": disabled, "pins": pins, "base_url_changed": changed, "kind_changed": rekinded,
	}
}

// providerAddressChanged is rule 8's predicate: where requests go, or how each
// person's credential is sent, differs between two versions of one provider.
func providerAddressChanged(a, b types.ModelProvider) bool {
	if a.BaseURL != b.BaseURL || bedrockAddress(a) != bedrockAddress(b) || authHeader(a.Auth) != authHeader(b.Auth) {
		return true
	}
	for _, ha := range a.Harnesses {
		for _, hb := range b.Harnesses {
			if ha.Harness == hb.Harness && (ha.Path != hb.Path || ha.AuthHeader != hb.AuthHeader) {
				return true
			}
		}
	}
	return false
}

// providerAddressDigest digests every input providerAddressChanged reads, for
// a sign-in to stamp at launch and compare at capture: a capture for an old
// address must not land after rule 8's purge. A digest, so no base URL reaches
// the audit log. Coarser than the predicate on one point only: it also moves
// when a harness is added or removed, and that capture is refused (sign in
// again) rather than stored.
func providerAddressDigest(p types.ModelProvider) string {
	harnesses := map[string][2]string{}
	for _, h := range p.Harnesses {
		harnesses[h.Harness] = [2]string{h.Path, h.AuthHeader}
	}
	sum := sha256.Sum256(mustJSON([]any{p.BaseURL, bedrockAddress(p), authHeader(p.Auth), harnesses}))
	return hex.EncodeToString(sum[:])
}

func bedrockAddress(p types.ModelProvider) string {
	if p.Bedrock == nil {
		return ""
	}
	return p.Bedrock.Region + " " + p.Bedrock.BaseURL
}

func authHeader(a *types.ProviderAuth) string {
	if a == nil {
		return ""
	}
	return a.Header
}

// SetupModelProvider is one model provider as a person sees it on
// /setup/status: enough to choose it, and — D7 — the host their own credential
// would be sent to (the host only, never a path). No start URL, account pin,
// header scheme or UID. Built the same for admins and members, so there is
// nothing for the member redaction to strip.
type SetupModelProvider struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Kind string `json:"kind"`
	// Disabled is published, never hidden: a turned-off default is why this
	// person's runs are refused, and they need to see it.
	Disabled bool `json:"disabled,omitempty"`
	// Harnesses are the ones this provider serves that this person may launch.
	Harnesses []string `json:"harnesses"`
	// DefaultFor are the harnesses whose roster default this provider is.
	DefaultFor []string `json:"default_for,omitempty"`
	Host       string   `json:"host"`
}

// setupModelProviders is SetupStatus.ModelProviders: nil (absent) with no block,
// else one row per provider serving at least one harness this caller may launch
// — enabled on the roster and allowed by their agent capability. A capability
// read that fails drops the harness: the list only ever narrows.
func (s *Server) setupModelProviders(ctx context.Context, sc types.SiteConfig) []SetupModelProvider {
	if sc.ModelProviders == nil {
		return nil
	}
	caps := s.newCapBatch(ctx)
	launchable := func(harness string) bool {
		ok, err := caps.allowed(ctx, capAgent, harness)
		return err == nil && ok && agentEnabled(sc, harness)
	}
	out := []SetupModelProvider{}
	for _, p := range sc.ModelProviders.Providers {
		row := SetupModelProvider{ID: p.ID, Name: p.Name, Kind: string(p.Kind), Disabled: p.Disabled,
			Harnesses: []string{}, Host: providerHost(p)}
		for _, h := range p.Harnesses {
			if !launchable(h.Harness) {
				continue
			}
			row.Harnesses = append(row.Harnesses, h.Harness)
			if agent, ok := agentProviderFor(sc, h.Harness); ok && agent.DefaultProvider == p.ID {
				row.DefaultFor = append(row.DefaultFor, h.Harness)
			}
		}
		if len(row.Harnesses) > 0 {
			out = append(out, row)
		}
	}
	return out
}

// providerHost is the one host a provider sends requests to: its own address
// when it has one, else the vendor's.
func providerHost(p types.ModelProvider) string {
	base := p.BaseURL
	if p.Bedrock != nil {
		base = p.Bedrock.BaseURL
		if base == "" {
			return bedrockRuntimeHost(p.Bedrock.Region)
		}
	}
	if base == "" {
		if hosts := providerPublicHosts(p.Kind); len(hosts) > 0 {
			return hosts[0]
		}
		return ""
	}
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
