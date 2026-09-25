// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Each person's own credential for a model provider: the reserved UID-keyed
// names it is stored under, the strict read that never falls back to the
// operator's row, the door that writes it into the caller's own namespace, and
// rule 8's purge — an address, scheme or kind change, or a deletion, removes
// every person's credential for that provider. Nothing at run create or
// dispatch reads these rows yet (MP-7 onward).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// providerSecretPrefix starts every per-person model-provider credential name:
// wardyn-provider-<uid>-key for a typed key or token, -oauth for a Claude
// sign-in, -sso for an AWS sign-in. Keyed by the provider's server-minted UID,
// never its admin-chosen ID, so a provider deleted and re-added under the same
// ID starts with nobody's credential.
const providerSecretPrefix = types.ModelProviderSecretPrefix

const (
	providerKeyPart   = "key"
	providerOAuthPart = "oauth"
	providerSSOPart   = "sso"
)

// providerSecretParts are every part a person's credential for one provider
// may be stored under.
var providerSecretParts = []string{providerKeyPart, providerOAuthPart, providerSSOPart}

func providerSecretName(uid, part string) string { return providerSecretPrefix + uid + "-" + part }

// providerSignInSecret reports whether name is a per-person sign-in capture
// (-oauth, -sso). Those are reserved at every sink and API (reservedSecret): a
// sign-in blob is never a header value, so no grant may name one. A -key is
// not: the injection sink resolves it by name from the namespace a grant
// snapshots, so it is reserved only at the generic secrets API and the broker.
func providerSignInSecret(name string) bool {
	return strings.HasPrefix(name, providerSecretPrefix) &&
		(strings.HasSuffix(name, "-"+providerOAuthPart) || strings.HasSuffix(name, "-"+providerSSOPart))
}

// providerTypedKinds are the kinds whose credential each person types (a key or
// a token) — the only ones PUT /model-providers/{id}/credential accepts. The two
// sign-in kinds are captured by a sign-in instead.
var providerTypedKinds = map[types.ModelProviderKind]bool{
	types.ModelProviderAnthropicAPIKey: true, types.ModelProviderOpenAIAPIKey: true,
	types.ModelProviderBedrockBearer: true, types.ModelProviderCustomEndpoint: true,
}

// DRAFT (M2 canon pending).
const (
	mpcNotFound = "no model provider %q is available to you"
	mpcSignIn   = "%q is connected by signing in, not with a key"
	mpcNoPerson = "the admin token is a shared credential rather than a person, so it cannot hold a model credential of its own — sign in to the console, or use your own wdn_ API token"
	mpcBody     = `body must be {"value":"<your key or token>"}`
	mpcTooShort = "your key or token must be at least %d characters"
	mpcNoStore  = "no secret store configured"
)

// ownSecret reads name from owner's OWN namespace and never the operator's.
// Store.For(owner).Get FALLS BACK to the operator's row by contract, so the
// obvious one-liner would serve an admin's credential to a person who stored
// none; List-then-Get is what refuses that. No owner, or a read inside the
// no-credential member preview, is absent. found=false with a nil error is
// "not stored"; any other store failure is returned, never read as absent.
//
// Ceiling: a row deleted between the List and the Get reads the operator's
// row of that name. For a wardyn-provider-* name there is none to read — every
// write door refuses those names in the operator namespace.
func (s *Server) ownSecret(ctx context.Context, owner, name string) ([]byte, bool, error) {
	if s.cfg.Secrets == nil || owner == "" || previewHidesOwnCredential(ctx) {
		return nil, false, nil
	}
	st := s.cfg.Secrets.For(owner)
	own, err := st.List(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("list own secrets: %w", err)
	}
	if !slices.Contains(own, name) {
		return nil, false, nil
	}
	raw, err := st.Get(ctx, name)
	if errors.Is(err, secretstore.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read own secret: %w", err)
	}
	return raw, true, nil
}

// mountModelProviderCredentialRoutes registers the per-person credential door
// on the authenticated group, not operatorOnly: every person, admins included,
// writes only their own credential, and the handler decides who may.
func (s *Server) mountModelProviderCredentialRoutes(r chi.Router) {
	r.Put("/model-providers/{id}/credential", s.handlePutProviderCredential)
	r.Delete("/model-providers/{id}/credential", s.handleDeleteProviderCredential)
}

// credentialOwner is the namespace the caller's own model credential lives in
// — their run-identity subject, the one dispatch will read for their runs — or
// "" with the refusal already written. Never the operator namespace: an
// operator's own credential lives under their subject like anyone's. Under
// OIDC the admin token is a mechanism, not a person, and holds none.
func (s *Server) credentialOwner(w http.ResponseWriter, r *http.Request) string {
	if s.cfg.Secrets == nil {
		writeError(w, http.StatusServiceUnavailable, mpcNoStore)
		return ""
	}
	owner := runIdentitySubject(r.Context(), principalFromRequest(r))
	if owner == "" || (s.cfg.OIDC != nil && owner == adminTokenPrincipal) {
		writeError(w, http.StatusUnprocessableEntity, mpcNoPerson)
		return ""
	}
	return owner
}

// providerForCredential reads the provider {id} names as this caller may see
// it. granted requires it to serve an agent the caller may launch (the
// /setup/status projection's own rule); a delete does not, so a person whose
// grant was withdrawn can still remove their key. A provider the caller cannot
// see, or one whose credential is a sign-in, is refused here. The caller holds
// siteConfigMu, so the UID read here is still the provider's when the write
// lands — rule 8's purge runs under the same lock.
func (s *Server) providerForCredential(w http.ResponseWriter, r *http.Request, granted bool) (types.ModelProvider, bool) {
	id := chi.URLParam(r, "id")
	sc, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return types.ModelProvider{}, false
	}
	p, ok := modelProviderByID(sc.ModelProviders, id)
	if ok && granted {
		ok = slices.ContainsFunc(s.setupModelProviders(r.Context(), sc), func(v SetupModelProvider) bool { return v.ID == id })
	}
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf(mpcNotFound, id))
		return types.ModelProvider{}, false
	}
	if !providerTypedKinds[p.Kind] {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf(mpcSignIn, id))
		return types.ModelProvider{}, false
	}
	return p, true
}

// handlePutProviderCredential stores the caller's own key or token for one
// provider, in their own namespace only. Write-only: nothing returns it.
//
//	PUT /api/v1/model-providers/{id}/credential  {"value": "..."}
func (s *Server) handlePutProviderCredential(w http.ResponseWriter, r *http.Request) {
	owner := s.credentialOwner(w, r)
	if owner == "" {
		return
	}
	var body putSecretRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, mpcBody)
		return
	}
	value := strings.TrimSpace(body.Value)
	if value == "" {
		writeError(w, http.StatusBadRequest, mpcBody)
		return
	}
	// The generic secrets API's reason: masking and scanning ignore anything
	// shorter, and accepting it would imply they cover it.
	if len(value) < secretmask.MinLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(mpcTooShort, secretmask.MinLen))
		return
	}
	s.siteConfigMu.Lock()
	defer s.siteConfigMu.Unlock()
	p, ok := s.providerForCredential(w, r, true)
	if !ok {
		return
	}
	if err := s.cfg.Secrets.For(owner).Put(r.Context(), providerSecretName(p.UID, providerKeyPart), []byte(value)); err != nil {
		writeServerError(w, r, "store model provider credential", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"model_provider.credential.write", p.ID, "success", mustJSON(map[string]any{"provider": p.ID, "owner": owner})))
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteProviderCredential removes the caller's own key or token for one
// provider. Idempotent: removing one never stored answers 204 too.
//
//	DELETE /api/v1/model-providers/{id}/credential
func (s *Server) handleDeleteProviderCredential(w http.ResponseWriter, r *http.Request) {
	owner := s.credentialOwner(w, r)
	if owner == "" {
		return
	}
	s.siteConfigMu.Lock()
	defer s.siteConfigMu.Unlock()
	p, ok := s.providerForCredential(w, r, false)
	if !ok {
		return
	}
	if err := s.cfg.Secrets.For(owner).Delete(r.Context(), providerSecretName(p.UID, providerKeyPart)); err != nil {
		writeServerError(w, r, "delete model provider credential", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"model_provider.credential.delete", p.ID, "success", mustJSON(map[string]any{"provider": p.ID, "owner": owner})))
	w.WriteHeader(http.StatusNoContent)
}

// invalidatedProviderUIDs is rule 8's set: every stored provider whose
// credentials must not survive the write — dropped from the block (deleted, or
// re-kinded, which assignModelProviderUIDs gives a fresh UID), or kept with a
// changed address or header scheme (providerAddressChanged). Runs after UIDs
// are assigned, so a kept provider matches its stored self by UID.
func invalidatedProviderUIDs(before, after *types.ModelProviders) []string {
	if before == nil {
		return nil
	}
	kept := map[string]types.ModelProvider{}
	if after != nil {
		for _, p := range after.Providers {
			kept[p.UID] = p
		}
	}
	var out []string
	for _, old := range before.Providers {
		if p, ok := kept[old.UID]; !ok || providerAddressChanged(old, p) {
			out = append(out, old.UID)
		}
	}
	return out
}

// purgeProviderCredentials deletes, in every namespace, each person's
// credential for every provider invalidatedProviderUIDs names, and returns how
// many it removed. Both write doors call it BEFORE saving the block, under
// siteConfigMu: a purge that fails refuses the write, so a credential given
// for one destination never follows the provider to a new one. A failed save
// after it leaves people to add their credential again — the closed side.
func (s *Server) purgeProviderCredentials(ctx context.Context, before, after *types.ModelProviders) (int, error) {
	uids := invalidatedProviderUIDs(before, after)
	if len(uids) == 0 || s.cfg.Secrets == nil {
		return 0, nil
	}
	names := make([]string, 0, len(providerSecretParts)*len(uids))
	for _, uid := range uids {
		for _, part := range providerSecretParts {
			names = append(names, providerSecretName(uid, part))
		}
	}
	n, err := s.cfg.Secrets.DeleteEverywhere(ctx, names)
	if err != nil {
		return 0, fmt.Errorf("purge model provider credentials: %w", err)
	}
	return n, nil
}

// connectedPeople is, per provider ID in block, how many distinct people hold
// a credential of their own for it — a key, a Claude sign-in or an AWS sign-in,
// one person counted once. One store call over the rows, never a value read;
// the operator namespace is no person. Every provider has an entry, 0
// included. A never-configured block, or no secret store, reads nil and all
// zeros respectively.
func (s *Server) connectedPeople(ctx context.Context, block types.ModelProviders) (map[string]int, error) {
	if len(block.Providers) == 0 {
		return nil, nil
	}
	out := map[string]int{}
	providerOf := map[string]string{}
	names := make([]string, 0, len(providerSecretParts)*len(block.Providers))
	for _, p := range block.Providers {
		out[p.ID] = 0
		for _, part := range providerSecretParts {
			name := providerSecretName(p.UID, part)
			providerOf[name] = p.ID
			names = append(names, name)
		}
	}
	if s.cfg.Secrets == nil {
		return out, nil
	}
	holders, err := s.cfg.Secrets.Holders(ctx, names)
	if err != nil {
		return nil, fmt.Errorf("list model provider credential holders: %w", err)
	}
	people := map[string]map[string]bool{}
	for name, owners := range holders {
		id, ok := providerOf[name]
		if !ok {
			continue
		}
		for _, owner := range owners {
			if owner == "" {
				continue
			}
			if people[id] == nil {
				people[id] = map[string]bool{}
			}
			people[id][owner] = true
		}
	}
	for id, set := range people {
		out[id] = len(set)
	}
	return out, nil
}
