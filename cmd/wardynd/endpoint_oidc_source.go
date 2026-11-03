// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/url"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/api"
)

// endpointOIDCEnvFor is what a custom endpoint's sign-in takes from the console's own login: the issuer a
// `console`-bound sign-in must have been issued by, and the one callback URL an admin registers on every
// endpoint client. The callback sits on the same browser-facing origin as the console's own OIDC callback,
// under the console's base path. ok=false (the door answers endpoint_oidc_unconfigured) when the console has
// no OIDC login, or its redirect URL has no usable origin: there is no session to bind a sign-in to.
func endpointOIDCEnvFor(issuer, oidcRedirectURL, basePath string) (api.EndpointOIDCEnv, bool) {
	issuer = strings.TrimSpace(issuer)
	u, err := url.Parse(strings.TrimSpace(oidcRedirectURL))
	if issuer == "" || err != nil || u.Scheme == "" || u.Host == "" {
		return api.EndpointOIDCEnv{}, false
	}
	callback := (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: basePath + api.EndpointOIDCCallbackPath}).String()
	return api.EndpointOIDCEnv{ConsoleIssuer: issuer, CallbackURL: callback}, true
}

// endpointOIDCFromFlags fills api.Config.EndpointOIDC from the daemon's OIDC flags.
func endpointOIDCFromFlags(f *bootFlags) func() (api.EndpointOIDCEnv, bool) {
	env, ok := endpointOIDCEnvFor(*f.oidcIssuer, *f.oidcRedirectURL, *f.basePath)
	return func() (api.EndpointOIDCEnv, bool) { return env, ok }
}
