// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

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
func newADOEntraLogin(issuer, clientID, clientSecret, oidcRedirectURL, basePath string, allowTestEndpoints bool) adoEntraLogin {
	return adoEntraLogin{
		clientID:           strings.TrimSpace(clientID),
		clientSecret:       clientSecret,
		tenantID:           entraTenantFromIssuer(issuer),
		redirectURL:        adoEntraRedirectURL(oidcRedirectURL, basePath),
		allowTestEndpoints: allowTestEndpoints,
	}
}

// adoEntraRedirectURL puts the Azure DevOps callback on the SAME browser-facing
// origin the console's own OIDC callback is registered on, under the console's
// base path. "" when that URL has no usable origin, which the sign-in refuses
// by name.
func adoEntraRedirectURL(oidcRedirectURL, basePath string) string {
	u, err := url.Parse(strings.TrimSpace(oidcRedirectURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: basePath + api.ADOEntraCallbackPath}).String()
}

// adoEntraSourceFromFlags is main's one call: the per-person Azure DevOps
// sign-in, read from the live provider rows and bound to the console's OWN OIDC
// application. A deployment with no such row answers "not configured" exactly
// as an unset source does.
//
// It reads once at boot so a minted_pat row the console cannot redeem with its
// own secret (S1, ado_pat_needs_console_app) is named in the log the moment the
// daemon starts. The row stays unusable on every read after: api validates
// every resolved configuration before using it.
func adoEntraSourceFromFlags(st siteConfigReader, f *bootFlags) api.ADOEntraSource {
	src := adoEntraSource(st, adoEntraLoginFromFlags(f))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg, found, err := src(ctx)
	switch {
	case err != nil:
		slog.Error("wardynd: the Azure DevOps sign-in row could not be resolved; Azure DevOps access fails closed until it is fixed",
			slog.Any("err", err))
	case found && cfg.TokenMode == types.ADOTokenModeMintedPAT && cfg.ClientSecret == "":
		slog.Error("wardynd: the Azure DevOps row is unusable for per-run tokens",
			slog.String("row", cfg.RowID), slog.String("reason", api.ReasonADOPATNeedsConsoleApp), slog.Any("err", api.ErrADOMintNeedsSecret))
	}
	return src
}

// adoLoginFactsFromFlags is api.Config.ADOLoginFacts: the console's own client
// and tenant, and whether it holds a secret — what a row write needs to decide
// S1 without ever seeing the secret itself.
func adoLoginFactsFromFlags(f *bootFlags) func() (clientID, tenantID string, hasSecret bool) {
	login := adoEntraLoginFromFlags(f)
	return login.facts
}

func adoEntraLoginFromFlags(f *bootFlags) adoEntraLogin {
	return newADOEntraLogin(*f.oidcIssuer, *f.oidcClientID, *f.oidcClientSecret, *f.oidcRedirectURL, *f.basePath, *f.allowTestEndpoints)
}

// facts reports the login's client, tenant and whether a secret is set.
func (l adoEntraLogin) facts() (clientID, tenantID string, hasSecret bool) {
	return l.clientID, l.tenantID, l.clientSecret != ""
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
		row, ok, err := adoEntraRow(sc)
		if err != nil || !ok {
			return api.ADOEntraConfig{}, false, err
		}
		mode := row.Entra.TokenMode
		if mode == "" {
			mode = types.ADOTokenModeBearer
		}
		scopes := adoscope.MintScopes()
		if mode != types.ADOTokenModeMintedPAT {
			if scopes, err = adoscope.ScopesFor(row.Entra.CapabilityCeiling); err != nil {
				return api.ADOEntraConfig{}, false, fmt.Errorf("azure devops provider row %q: %w", row.ID, err)
			}
		}
		return adoEntraConfigFor(row, login, mode, scopes), true, nil
	}
}

// adoEntraByRow fills api.Config.ADOEntraByRow: the row with this id, found
// whether or not it is enabled or the first — what revoking a token created
// through a row an admin has since disabled needs. A row that is gone, is not
// Azure DevOps or has no Entra block answers found=false. The revoke uses only
// the row's tenant, client and the console's secret, so no filter that decides
// which row signs people in applies, and neither does its scope ceiling.
func adoEntraByRow(st siteConfigReader, login adoEntraLogin) func(ctx context.Context, rowID string) (api.ADOEntraConfig, bool, error) {
	return func(ctx context.Context, rowID string) (api.ADOEntraConfig, bool, error) {
		sc, err := st.GetSiteConfig(ctx)
		if err != nil || sc.WorkspaceProviders == nil {
			return api.ADOEntraConfig{}, false, err
		}
		for _, row := range sc.WorkspaceProviders.Git {
			if row.ID != rowID || row.Kind != types.GitProviderAzureDevOps || row.Entra == nil {
				continue
			}
			mode := row.Entra.TokenMode
			if mode == "" {
				mode = types.ADOTokenModeBearer
			}
			return adoEntraConfigFor(row, login, mode, adoscope.MintScopes()), true, nil
		}
		return api.ADOEntraConfig{}, false, nil
	}
}

// azureFoundryEntraByRow fills api.Config.AzureFoundryEntra: the application an
// azure_foundry provider row signs people in against, by the row's uid. It is
// always the console's own sign-in application (login), never a per-row one, so
// a console without Entra login yields an unusable configuration that both
// legs refuse. found=false for a uid that is not an azure_foundry row. The
// scope policy is the capture's (api.entraCapture), not this source's.
func azureFoundryEntraByRow(st siteConfigReader, login adoEntraLogin) func(ctx context.Context, rowUID string) (api.ADOEntraConfig, bool, error) {
	return func(ctx context.Context, rowUID string) (api.ADOEntraConfig, bool, error) {
		sc, err := st.GetSiteConfig(ctx)
		if err != nil || rowUID == "" || sc.ModelProviders == nil {
			return api.ADOEntraConfig{}, false, err
		}
		for _, p := range sc.ModelProviders.Providers {
			if p.UID != rowUID || p.Kind != types.ModelProviderAzureFoundry {
				continue
			}
			return api.ADOEntraConfig{
				RowID:              p.UID,
				TenantID:           login.tenantID,
				ClientID:           login.clientID,
				ClientSecret:       login.clientSecret,
				RedirectURL:        login.redirectURL,
				LoginClientID:      login.clientID,
				LoginTenantID:      login.tenantID,
				AllowTestEndpoints: login.allowTestEndpoints,
			}, true, nil
		}
		return api.ADOEntraConfig{}, false, nil
	}
}

// adoEntraConfigFor is the configuration row describes under login, with the
// console's secret attached only where it may be redeemed.
func adoEntraConfigFor(row types.GitProvider, login adoEntraLogin, mode types.ADOTokenMode, scopes []string) api.ADOEntraConfig {
	cfg := api.ADOEntraConfig{
		RowID:              row.ID,
		TenantID:           row.Entra.TenantID,
		ClientID:           row.Entra.ClientID,
		RedirectURL:        login.redirectURL,
		Scopes:             scopes,
		TokenMode:          mode,
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
	// S1 at first read: a row that creates tokens and names another tenant
	// gets no secret either, so the one test every door applies — an empty
	// secret is unusable for minting (api.ErrADOMintNeedsSecret) — covers
	// every way the console could not redeem it as a confidential client.
	if mode == types.ADOTokenModeMintedPAT && !strings.EqualFold(row.Entra.TenantID, login.tenantID) {
		cfg.ClientSecret = ""
	}
	return cfg
}

// adoEntraRow is the first ENABLED Azure DevOps row that permits the per-person
// sign-in: the entra lane named, an Entra block present, a per_user credential
// source — the dispatch lane's own predicate (resolveADOEntraRun), so a row the
// dispatch will not serve never drives a capture — and a token mode that signs
// in. An own_pat row pastes a token and has no sign-in, so it is skipped here
// rather than allowed to shadow one that does; its dispatch is its own lane's.
//
// A bearer row and a minted_pat row naming the same application are refused
// together: one app registration holds one consent, so the bearer row's token
// would carry the token permissions the minted row needs (S2's premise).
func adoEntraRow(sc types.SiteConfig) (types.GitProvider, bool, error) {
	if sc.WorkspaceProviders == nil {
		return types.GitProvider{}, false, nil
	}
	var rows []types.GitProvider
	for _, row := range sc.WorkspaceProviders.Git {
		if row.Disabled || row.Kind != types.GitProviderAzureDevOps || row.Entra == nil {
			continue
		}
		if !slices.Contains(row.Lanes, types.GitLaneEntra) || row.CredentialSource != types.CredentialSourcePerUser ||
			row.Entra.TokenMode == types.ADOTokenModeOwnPAT {
			continue
		}
		rows = append(rows, row)
	}
	for _, minted := range rows {
		if minted.Entra.TokenMode != types.ADOTokenModeMintedPAT {
			continue
		}
		for _, other := range rows {
			if other.Entra.TokenMode != types.ADOTokenModeMintedPAT && strings.EqualFold(other.Entra.ClientID, minted.Entra.ClientID) {
				return types.GitProvider{}, false, fmt.Errorf("azure devops provider rows %q (bearer) and %q (minted_pat) name the same application %s: "+
					"its consent carries the token permissions, so its bearer would let a run create tokens — give the bearer row an application without them",
					other.ID, minted.ID, minted.Entra.ClientID)
			}
		}
	}
	if len(rows) == 0 {
		return types.GitProvider{}, false, nil
	}
	return rows[0], true, nil
}
