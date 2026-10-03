// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/api"
)

// minSCIMTokenBytes is the shortest SCIM bearer boot accepts: 32 bytes is 256 bits of an
// `openssl rand -hex 16`-style value, and the token can suspend every person on the deployment.
const minSCIMTokenBytes = 32

// WARDYN_SCIM_LEAVER_WORKSPACES values: what a purge does with the workspaces the person owns.
const (
	scimWorkspacesReassign = "reassign"
	scimWorkspacesKeep     = "keep"
)

// scimConfigValidated is scimConfig after validateBootPosture has already refused every bad
// setting, so it has no error left to report.
func scimConfigValidated(f *bootFlags, posture tlsPosture) *api.SCIMConfig {
	c, _ := scimConfig(f, posture)
	return c
}

// scimConfig is WARDYN_SCIM_TOKEN (and its rotation twin, WARDYN_SCIM_TOKEN_NEXT) as the api's
// SCIMConfig, or nil when SCIM is off (no token), or an error naming the refusal. It refuses to
// mount SCIM, rather than mount something that would silently miss a leaver:
//
//   - without OIDC configured, there is no sign-in identity to deprovision;
//   - on an issuer whose derived tenant is empty (not commercial-cloud Entra) or is `common`,
//     `organizations` or `consumers` (entraTenantFromIssuer returns the raw first path segment, so a
//     multi-tenant issuer is not empty): externalId matching would look for entra:common:<oid> people
//     rows while people rows are keyed by the token's real tid, and a suspension would miss;
//   - without TLS (built in, or WARDYN_TLS_TERMINATED), the bearer would travel in clear;
//   - with a token shorter than 32 bytes, or equal to its twin, the admin token or the published
//     demo admin token;
//   - with a negative WARDYN_SCIM_PURGE_AFTER or a WARDYN_SCIM_LEAVER_WORKSPACES other than
//     reassign or keep.
//
// No refusal carries a token's value.
func scimConfig(f *bootFlags, posture tlsPosture) (*api.SCIMConfig, error) {
	if f.scimToken == nil || f.scimTokenNext == nil { // a flag set built without SCIM's two (tests) has none
		return nil, nil
	}
	token, next := strings.TrimSpace(*f.scimToken), strings.TrimSpace(*f.scimTokenNext)
	if token == "" && next == "" {
		return nil, nil
	}
	if token == "" {
		return nil, errors.New("refusing to start: WARDYN_SCIM_TOKEN_NEXT is set without WARDYN_SCIM_TOKEN — rotation adds a second token beside the first, it does not replace it")
	}
	issuer := strings.TrimSpace(*f.oidcIssuer)
	if issuer == "" {
		return nil, errors.New("refusing to start: WARDYN_SCIM_TOKEN is set but OIDC is not configured (WARDYN_OIDC_ISSUER) — SCIM suspends the people who sign in through it")
	}
	tenant := entraTenantFromIssuer(issuer)
	switch strings.ToLower(tenant) {
	case "", "common", "organizations", "consumers":
		return nil, fmt.Errorf("refusing to start: WARDYN_SCIM_TOKEN is set but WARDYN_OIDC_ISSUER %q names no single Entra tenant — "+
			"SCIM matches a leaver by the tenant in the issuer's path, so it needs a commercial-cloud Entra issuer of the form "+
			"https://login.microsoftonline.com/<tenant id>/v2.0, never a multi-tenant (common, organizations, consumers) or non-Entra one", issuer)
	}
	if !posture.secureCookies {
		return nil, errors.New("refusing to start: WARDYN_SCIM_TOKEN is set but wardynd does not know the connection is TLS — serve TLS (WARDYN_TLS_CERT and WARDYN_TLS_KEY) " +
			"or set WARDYN_TLS_TERMINATED when a proxy terminates it; the SCIM bearer must never cross the network in clear")
	}
	for _, t := range []struct{ name, value string }{{"WARDYN_SCIM_TOKEN", token}, {"WARDYN_SCIM_TOKEN_NEXT", next}} {
		if t.value == "" {
			continue
		}
		if len(t.value) < minSCIMTokenBytes {
			return nil, fmt.Errorf("refusing to start: %s is shorter than %d bytes; generate one with `openssl rand -hex 32`", t.name, minSCIMTokenBytes)
		}
		if t.value == strings.TrimSpace(*f.adminToken) || t.value == demoAdminToken {
			return nil, fmt.Errorf("refusing to start: %s equals the admin token (or the published demo token) — the identity provider's connector must not hold the admin credential", t.name)
		}
	}
	if next != "" && next == token {
		return nil, errors.New("refusing to start: WARDYN_SCIM_TOKEN_NEXT equals WARDYN_SCIM_TOKEN — the rotation token must be a different value")
	}
	cfg := &api.SCIMConfig{Token: token, TokenNext: next, Issuer: issuer, Tenant: tenant, PurgeAfter: 720 * time.Hour}
	if f.scimPurgeAfter != nil {
		if *f.scimPurgeAfter < 0 {
			return nil, errors.New("refusing to start: WARDYN_SCIM_PURGE_AFTER is negative; use 0 to disable the automatic purge")
		}
		cfg.PurgeAfter = *f.scimPurgeAfter
	}
	if f.scimLeaverWorkspaces != nil {
		switch w := strings.TrimSpace(*f.scimLeaverWorkspaces); w {
		case scimWorkspacesReassign:
		case scimWorkspacesKeep:
			cfg.KeepWorkspaces = true
		default:
			return nil, fmt.Errorf("refusing to start: WARDYN_SCIM_LEAVER_WORKSPACES %q is not %q or %q", w, scimWorkspacesReassign, scimWorkspacesKeep)
		}
	}
	return cfg, nil
}
