// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoEntraLogin is the console's OWN sign-in application, as the daemon was
// booted with it. It is the other half of the Azure DevOps sign-in's boundary:
// a captured credential is bound by comparing the Azure DevOps sign-in's
// subject with the console session's, and that comparison means something only
// when both tokens come from one app registration in one tenant. So these two
// values come from the console's OIDC configuration and from nowhere else.
type adoEntraLogin struct {
	clientID     string
	clientSecret string
	// tenantID is the segment in front of /v2.0 in the console's issuer — ""
	// for any issuer that is not Entra's, which refuses every capture.
	tenantID string
	// redirectURL is the absolute Azure DevOps callback on the console's own
	// browser-facing origin.
	redirectURL        string
	allowTestEndpoints bool
}

// newADOEntraLogin derives the login half from the daemon's OIDC flags. Every
// empty input stays empty rather than being defaulted: an empty client or
// tenant is what makes both Azure DevOps sign-in doors refuse and the console
// login decline to widen, which is the fail-closed direction.
func newADOEntraLogin(issuer, clientID, clientSecret, oidcRedirectURL string, allowTestEndpoints bool) adoEntraLogin {
	return adoEntraLogin{
		clientID:           strings.TrimSpace(clientID),
		clientSecret:       clientSecret,
		tenantID:           entraTenantFromIssuer(issuer),
		redirectURL:        adoEntraRedirectURL(oidcRedirectURL),
		allowTestEndpoints: allowTestEndpoints,
	}
}

// adoEntraRedirectURL puts the Azure DevOps callback on the SAME browser-facing
// origin the console's own OIDC callback is registered on. "" when that URL
// has no usable origin, which the sign-in refuses by name.
func adoEntraRedirectURL(oidcRedirectURL string) string {
	u, err := url.Parse(strings.TrimSpace(oidcRedirectURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: api.ADOEntraCallbackPath}).String()
}

// adoEntraSourceFromFlags is main's one call: the per-person Azure DevOps
// sign-in, read from the live provider rows and bound to the console's OWN OIDC
// application. A deployment with no such row answers "not configured" exactly
// as an unset source does.
func adoEntraSourceFromFlags(st siteConfigReader, f *bootFlags) api.ADOEntraSource {
	return adoEntraSource(st, newADOEntraLogin(*f.oidcIssuer, *f.oidcClientID,
		*f.oidcClientSecret, *f.oidcRedirectURL, *f.allowTestEndpoints))
}

// siteConfigReader is the one store read the source needs.
type siteConfigReader interface {
	GetSiteConfig(ctx context.Context) (types.SiteConfig, error)
}

// adoEntraSource fills api.Config.ADOEntra from the LIVE provider rows on every
// call, so the row stays the single source of truth and nothing acts on a copy.
//
// A deployment with no Azure DevOps Entra row answers found=false, which is the
// same refusal both sign-in doors give with no source at all and leaves the
// console login and every dispatch exactly as they were.
func adoEntraSource(st siteConfigReader, login adoEntraLogin) api.ADOEntraSource {
	return func(ctx context.Context) (api.ADOEntraConfig, bool, error) {
		sc, err := st.GetSiteConfig(ctx)
		if err != nil {
			return api.ADOEntraConfig{}, false, err
		}
		row, ok := adoEntraRow(sc)
		if !ok {
			return api.ADOEntraConfig{}, false, nil
		}
		scopes, err := adoscope.ScopesFor(row.Entra.CapabilityCeiling)
		if err != nil {
			return api.ADOEntraConfig{}, false, fmt.Errorf("azure devops provider row %q: %w", row.ID, err)
		}
		cfg := api.ADOEntraConfig{
			RowID:              row.ID,
			TenantID:           row.Entra.TenantID,
			ClientID:           row.Entra.ClientID,
			RedirectURL:        login.redirectURL,
			Scopes:             scopes,
			LoginClientID:      login.clientID,
			LoginTenantID:      login.tenantID,
			AllowTestEndpoints: login.allowTestEndpoints,
		}
		// The console's client secret belongs to the console's app registration
		// and is sent to NO OTHER. A row naming a different application gets no
		// secret; its sign-in is refused by name before any token request anyway.
		if login.clientID != "" && strings.EqualFold(row.Entra.ClientID, login.clientID) {
			cfg.ClientSecret = login.clientSecret
		}
		return cfg, true, nil
	}
}

// adoEntraRow is the first ENABLED Azure DevOps row that permits the per-person
// lane: the entra lane named, an Entra block present, and a per_user credential
// source — the same predicate the dispatch lane authors a credential under
// (resolveADOEntraRun), so a row the dispatch will not serve never drives a
// capture either.
func adoEntraRow(sc types.SiteConfig) (types.GitProvider, bool) {
	if sc.WorkspaceProviders == nil {
		return types.GitProvider{}, false
	}
	for _, row := range sc.WorkspaceProviders.Git {
		if row.Disabled || row.Kind != types.GitProviderAzureDevOps || row.Entra == nil {
			continue
		}
		if !slices.Contains(row.Lanes, types.GitLaneEntra) || row.CredentialSource != types.CredentialSourcePerUser {
			continue
		}
		return row, true
	}
	return types.GitProvider{}, false
}
