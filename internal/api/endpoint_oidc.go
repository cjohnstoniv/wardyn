// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// endpoint_oidc.go is a custom_endpoint's own sign-in: each person signs in to the
// endpoint's authority (a provider row's `oidc` block) instead of pasting a token. It
// is the Entra capture's machinery (ado_entra_store.go, entra_capture.go) run as one
// more kind, with a token endpoint, client and identity binding of its own.
//
// WHERE A TOKEN LIVES. The refresh token is sealed in the person's own namespace at
// wardyn-provider-<uid>-oidc, written only through putOwned (storeEntraBlob), read only
// by the owner-namespace list-then-get (readEntraBlob), and reserved at every sink
// (providerSignInSecret), so no grant can name it and nothing hands it to a sandbox. An
// access token is minted per use and never stored. Neither token, nor the identity token,
// nor a code or verifier reaches a log line, an audit row, or an error a viewer sees.
//
// A TOKEN RESPONSE WITH NO `scope` is read as the requested set (RFC 6749 section 5.1),
// and a refresh sends no `scope`, for this kind only. Every Entra kind keeps refusing a
// response that names none (entra_capture.go).

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The refusals a provider row's `oidc` block is held to.
const (
	mp400OIDCKind      = "model_providers: %q: oidc applies only to a custom_endpoint provider"
	mp400OIDCIssuer    = "model_providers: %q: oidc.issuer is required — sign-in is discovered from it and the identity token is verified against it"
	mp400OIDCURL       = "model_providers: %q: oidc.%s: %w"
	mp400OIDCClient    = "model_providers: %q: oidc.client_id must be 1 to 256 printable characters with no spaces"
	mp400OIDCSecretRef = "model_providers: %q: oidc.client_secret_ref must be the name of an operator secret — letters, digits and ._- , at most 128 characters"
	mp400OIDCScopes    = "model_providers: %q: oidc.scopes must name 1 to %d scopes, each a single value with no .default"
	mp400OIDCBinding   = "model_providers: %q: oidc.identity_binding must be %q or %q"
)

var (
	// oidcClientIDPattern is a client id: printable ASCII, no space, bounded.
	oidcClientIDPattern = regexp.MustCompile(`^[\x21-\x7e]{1,256}$`)
	// oidcSecretRefPattern is an operator secret name a client secret may be read from.
	oidcSecretRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// normalizeProviderOIDC trims the block's free-text fields on its way to storage.
func normalizeProviderOIDC(o *types.ProviderOIDC) {
	if o == nil {
		return
	}
	o.Issuer, o.AuthorizeURL, o.TokenURL = strings.TrimSpace(o.Issuer), strings.TrimSpace(o.AuthorizeURL), strings.TrimSpace(o.TokenURL)
	o.ClientID, o.ClientSecretRef = strings.TrimSpace(o.ClientID), strings.TrimSpace(o.ClientSecretRef)
	o.IdentityBinding = strings.TrimSpace(o.IdentityBinding)
	for i := range o.Scopes {
		o.Scopes[i] = strings.TrimSpace(o.Scopes[i])
	}
}

// validateProviderOIDC holds a row's `oidc` block to its rules. allowHTTP is the test-endpoint
// posture, the one place a plain-http authority is admitted. A private address follows the
// gateway posture every admin-typed endpoint here has (validateOneLLMGateway).
func validateProviderOIDC(mp types.ModelProvider, allowHTTP bool) error {
	o := mp.OIDC
	if o == nil {
		return nil
	}
	if mp.Kind != types.ModelProviderCustomEndpoint {
		return atField("oidc", fmt.Errorf(mp400OIDCKind, mp.ID))
	}
	if o.Issuer == "" {
		return atField("oidc.issuer", fmt.Errorf(mp400OIDCIssuer, mp.ID))
	}
	for _, f := range []struct{ name, value string }{{"issuer", o.Issuer}, {"authorize_url", o.AuthorizeURL}, {"token_url", o.TokenURL}} {
		if f.value == "" {
			continue
		}
		if _, err := validateOneLLMGateway("", f.value, allowHTTP); err != nil {
			return atField("oidc."+f.name, fmt.Errorf(mp400OIDCURL, mp.ID, f.name, err))
		}
	}
	if !oidcClientIDPattern.MatchString(o.ClientID) {
		return atField("oidc.client_id", fmt.Errorf(mp400OIDCClient, mp.ID))
	}
	if o.ClientSecretRef != "" && (!oidcSecretRefPattern.MatchString(o.ClientSecretRef) || nameSinkReservedSecret(o.ClientSecretRef)) {
		return atField("oidc.client_secret_ref", fmt.Errorf(mp400OIDCSecretRef, mp.ID))
	}
	if len(o.Scopes) == 0 || len(o.Scopes) > adoEntraMaxScopes {
		return atField("oidc.scopes", fmt.Errorf(mp400OIDCScopes, mp.ID, adoEntraMaxScopes))
	}
	for _, sc := range o.Scopes {
		if sc == "" || strings.ContainsFunc(sc, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) ||
			sc == ".default" || strings.HasSuffix(sc, azureDefaultSuffix) {
			return atField("oidc.scopes", fmt.Errorf(mp400OIDCScopes, mp.ID, adoEntraMaxScopes))
		}
	}
	if b := o.IdentityBinding; b != "" && b != types.OIDCBindingConsole && b != types.OIDCBindingNone {
		return atField("oidc.identity_binding", fmt.Errorf(mp400OIDCBinding, mp.ID, types.OIDCBindingConsole, types.OIDCBindingNone))
	}
	return nil
}

// oidcAddress is a row's sign-in authority and client: what an old refresh token must never be
// redeemed against after it changes. "" for a row with no `oidc` block. Scopes are an unordered
// set. The binding is in it too, so a person's sign-in made under one rule is never kept under
// another.
func oidcAddress(p types.ModelProvider) string {
	o := p.OIDC
	if o == nil {
		return ""
	}
	return string(mustJSON([]any{o.Issuer, o.AuthorizeURL, o.TokenURL, o.ClientID, slices.Sorted(slices.Values(o.Scopes)), o.Binding()}))
}

// reasonModelProviderCredentialSignsIn is the paste door's 409 for a row that signs people in.
const reasonModelProviderCredentialSignsIn = "model_provider_credential_signs_in"
