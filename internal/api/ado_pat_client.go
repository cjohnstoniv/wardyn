// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// ado_pat_client.go is the minting half of a minted_pat row: the vssps client
// that creates and revokes a person's personal access tokens, and the doors
// every caller goes through (mintAccess, mintADOPAT, revokeADOPAT), which hold
// S1 — a sign-in that can create tokens is redeemed only with the console's own
// secret, never as a public client.
//
// The Entra access token these calls carry never leaves wardynd: it goes to
// vssps.dev.azure.com/{org}/_apis/tokens/pats and nowhere else, and the
// classifier keeps that API closed to every sandbox.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// adoPATAPIBase is the token lifecycle API's host; the organisation is the
	// first path segment.
	adoPATAPIBase    = "https://vssps.dev.azure.com"
	adoPATAPIVersion = "7.1-preview.1"
	adoPATTimeout    = 15 * time.Second
)

// vsspsPATClient is the adoPATClient Azure DevOps answers. base is
// adoPATAPIBase, or a test server under WARDYN_ALLOW_TEST_ENDPOINTS.
type vsspsPATClient struct{ base string }

// patClient is the client cfg's mints and revokes use. The override is the
// Entra authority override's twin and is refused, not ignored, without the
// test acknowledgement.
func (c ADOEntraConfig) patClient() (adoPATClient, error) {
	if c.PATAPIOverride == "" {
		return vsspsPATClient{base: adoPATAPIBase}, nil
	}
	if !c.AllowTestEndpoints {
		return nil, fmt.Errorf("refusing the Azure DevOps token API override %q: it is a test hatch; "+
			"set WARDYN_ALLOW_TEST_ENDPOINTS=true to acknowledge a test deployment", c.PATAPIOverride)
	}
	return vsspsPATClient{base: strings.TrimSuffix(c.PATAPIOverride, "/")}, nil
}

// Create asks Azure DevOps for an organisation-only token. A refusal Azure
// DevOps answered is an *adoPATError, including a 200 whose patTokenError
// names a policy and carries no token.
func (c vsspsPATClient) Create(ctx context.Context, org, accessToken string, req adoPATRequest) (adoPAT, error) {
	body, err := json.Marshal(map[string]any{
		"displayName": req.DisplayName, "scope": req.Scope,
		"validTo": req.ValidTo.UTC().Format(time.RFC3339), "allOrgs": false,
	})
	if err != nil {
		return adoPAT{}, err
	}
	status, hdr, raw, err := c.call(ctx, http.MethodPost, org, accessToken, nil, body)
	if err != nil {
		return adoPAT{}, err
	}
	var out struct {
		PatToken *struct {
			AuthorizationID string `json:"authorizationId"`
			Token           string `json:"token"`
			Scope           string `json:"scope"`
			ValidTo         string `json:"validTo"`
		} `json:"patToken"`
		PatTokenError string `json:"patTokenError"`
	}
	jerr := json.Unmarshal(raw, &out)
	perr := out.PatTokenError
	if perr == "none" {
		perr = ""
	}
	if status != http.StatusOK || jerr != nil || perr != "" || out.PatToken == nil ||
		out.PatToken.Token == "" || out.PatToken.AuthorizationID == "" {
		return adoPAT{}, &adoPATError{Status: status, ServiceError: hdr.Get("X-TFS-ServiceError"), PatTokenError: perr}
	}
	validTo, terr := time.Parse(time.RFC3339, out.PatToken.ValidTo)
	if terr != nil {
		validTo = req.ValidTo // the create succeeded; its expiry is what was asked for
	}
	return adoPAT{AuthorizationID: out.PatToken.AuthorizationID, Token: out.PatToken.Token,
		Scope: out.PatToken.Scope, ValidTo: validTo}, nil
}

// Revoke deletes one token by authorization id. 200 and 204 are success.
func (c vsspsPATClient) Revoke(ctx context.Context, org, accessToken, authorizationID string) error {
	if _, err := uuid.Parse(authorizationID); err != nil {
		return fmt.Errorf("azure devops personal access token: authorization id %q is not a GUID", authorizationID)
	}
	status, hdr, _, err := c.call(ctx, http.MethodDelete, org, accessToken, url.Values{"authorizationId": {authorizationID}}, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusNoContent {
		return &adoPATError{Status: status, ServiceError: hdr.Get("X-TFS-ServiceError")}
	}
	return nil
}

// call sends one request to the organisation's token API. The organisation is
// held to a DNS label, since it is a path segment of a request carrying a
// bearer; redirects are answered, never followed, for the same reason.
func (c vsspsPATClient) call(ctx context.Context, method, org, accessToken string, q url.Values, body []byte) (int, http.Header, []byte, error) {
	if _, ok := adoOrganisationLabel(org); !ok {
		return 0, nil, nil, fmt.Errorf("azure devops personal access token: organisation %q is not a DNS label", org)
	}
	if q == nil {
		q = url.Values{}
	}
	q.Set("api-version", adoPATAPIVersion)
	u := c.base + "/" + org + "/_apis/tokens/pats?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{
		Transport:     http.DefaultTransport,
		Timeout:       adoPATTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<18))
	if err != nil {
		return 0, nil, nil, err
	}
	// Azure DevOps prefixes some JSON bodies with a byte-order mark.
	return resp.StatusCode, resp.Header, bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), nil
}

// mintAccess redeems owner's stored sign-in for an access token that may create
// and revoke personal access tokens.
//
// S1 AT THE MINT, before any cache or token request: an empty secret means the
// row's refresh token would be redeemed as a public client, which is exactly
// the credential S1 exists to refuse. validate() holds the rest (the console's
// own application, the two mint scopes). The granted string must name both
// mint scopes: the authority, not the request, decides what the token can do.
func (s *Server) mintAccess(ctx context.Context, cfg ADOEntraConfig, owner string) (ADOEntraAccess, error) {
	if cfg.ClientSecret == "" {
		return ADOEntraAccess{}, fmt.Errorf("azure devops sign-in: %w", ErrADOMintNeedsSecret)
	}
	if err := cfg.validate(); err != nil {
		return ADOEntraAccess{}, err
	}
	a, err := s.adoEntraAccessFor(secretstore.WithPurpose(ctx, secretstore.PurposeADORefresh), cfg, owner, adoscope.MintScopes(), false)
	if err != nil {
		return ADOEntraAccess{}, err
	}
	for _, want := range adoscope.MintScopes() {
		if !slices.ContainsFunc(a.Scopes, func(g string) bool { return adoUnqualified(g) == adoUnqualified(want) }) {
			return ADOEntraAccess{}, fmt.Errorf("%w: the sign-in was not granted %s", ErrADOEntraConsentRequired, want)
		}
	}
	return a, nil
}

// mintADOPAT creates one personal access token in owner's name. A policy
// refusal is recorded on owner's stored sign-in so /me/scm-access can say the
// organisation blocks them; the next token created clears it.
func (s *Server) mintADOPAT(ctx context.Context, cfg ADOEntraConfig, owner, org string, req adoPATRequest) (adoPAT, error) {
	return s.createADOPAT(ctx, cfg, owner, org, req, true)
}

// createADOPAT is mintADOPAT; noteBlocked=false leaves owner's blocked state
// alone on a policy refusal — the organisation check's canaries are a probe of
// the organisation, not the admin's own tokens.
func (s *Server) createADOPAT(ctx context.Context, cfg ADOEntraConfig, owner, org string, req adoPATRequest, noteBlocked bool) (adoPAT, error) {
	access, err := s.mintAccess(ctx, cfg, owner)
	if err != nil {
		return adoPAT{}, err
	}
	client, err := cfg.patClient()
	if err != nil {
		return adoPAT{}, err
	}
	pat, err := client.Create(ctx, org, access.AccessToken, req)
	var perr *adoPATError
	switch {
	case errors.As(err, &perr) && perr.Reason() == reasonADOPATPolicyBlocked:
		if noteBlocked {
			s.noteADOMintBlocked(ctx, cfg.RowID, owner, true)
		}
	case err == nil:
		// Masked process-wide under its own name until it expires: one name per
		// token, so registering it never retires the sign-in's own values.
		if s.cfg.MaskRegistry != nil {
			s.cfg.MaskRegistry.AddGlobalUntil(owner, "ado-pat-"+pat.AuthorizationID, s.cfg.Now(), pat.ValidTo, []byte(pat.Token))
		}
		s.noteADOMintBlocked(ctx, cfg.RowID, owner, false)
	}
	return pat, err
}

// revokeADOPAT revokes one of owner's tokens with owner's own sign-in.
func (s *Server) revokeADOPAT(ctx context.Context, cfg ADOEntraConfig, owner, org, authorizationID string) error {
	access, err := s.mintAccess(ctx, cfg, owner)
	if err != nil {
		return err
	}
	client, err := cfg.patClient()
	if err != nil {
		return err
	}
	return client.Revoke(ctx, org, access.AccessToken, authorizationID)
}

// noteADOMintBlocked sets or clears MintBlockedAt on owner's stored sign-in,
// under the redemption lock and on a fresh read, so it cannot overwrite a
// rotation. Best-effort: the caller's answer does not depend on it.
func (s *Server) noteADOMintBlocked(ctx context.Context, rowID, owner string, blocked bool) {
	unlock := s.adoEntra.lock(owner, rowID)
	defer unlock()
	blob, found, err := s.readADOEntraBlob(secretstore.WithPurpose(ctx, secretstore.PurposeADORefresh), owner, rowID)
	if err != nil || !found || blocked == !blob.MintBlockedAt.IsZero() {
		return
	}
	blob.MintBlockedAt = time.Time{}
	if blocked {
		blob.MintBlockedAt = s.cfg.Now().UTC()
	}
	_ = s.storeADOEntraBlob(ctx, owner, rowID, blob)
}

// ErrADOMintNeedsSecret is S1: a sign-in that can create personal access
// tokens must be redeemable only with a secret wardynd holds. The console's
// secret is sent to the console's own app registration and no other, so a
// minted_pat row must name that application in the sign-in tenant AND the
// secret must be set; otherwise its refresh token would mint as a public
// client.
var ErrADOMintNeedsSecret = errors.New("per-run tokens need your Wardyn app registration to have a client secret: " +
	"the row must name the console's own tenant and client, and WARDYN_OIDC_CLIENT_SECRET must be set")

// validateTokenMode holds the scopes to the mode (S4) and a minted row to S1.
// A bearer sign-in never asks for a token permission — its token rides runs'
// traffic — and a minted one asks for the two token permissions and nothing
// else, since the run's access lives in the token it creates.
func (c ADOEntraConfig) validateTokenMode() error {
	switch cmpTokenMode(c.TokenMode) {
	case types.ADOTokenModeBearer:
		if i := slices.IndexFunc(c.Scopes, adoscope.IsTokenScope); i >= 0 {
			return fmt.Errorf("azure devops sign-in: a bearer row may not ask for %q, which can create personal access tokens", c.Scopes[i])
		}
	case types.ADOTokenModeMintedPAT:
		if c.ClientSecret == "" || !c.isLoginApplication() {
			return fmt.Errorf("azure devops sign-in: %w", ErrADOMintNeedsSecret)
		}
		if !slices.Equal(slices.Sorted(slices.Values(c.Scopes)), slices.Sorted(slices.Values(adoscope.MintScopes()))) {
			return fmt.Errorf("azure devops sign-in: a minted_pat row asks for exactly %v, not %v", adoscope.MintScopes(), c.Scopes)
		}
	default:
		return fmt.Errorf("azure devops sign-in: token mode %q has no sign-in", c.TokenMode)
	}
	return nil
}

// adoUnqualified strips the Azure DevOps resource from a scope, lower-cased.
func adoUnqualified(scope string) string {
	return strings.TrimPrefix(strings.ToLower(scope), adoscope.ResourceID+"/")
}

// adoBearerMintScopeRefusal is S2: "" when a bearer whose authority reported
// granted may ride a run's traffic, else the reason it may not — the token can
// create personal access tokens, or the authority reported no scope and so
// proved nothing about it (fail closed).
func adoBearerMintScopeRefusal(granted []string) string {
	switch {
	case len(granted) == 0:
		return reasonBearerScopeUnknown
	case slices.ContainsFunc(granted, adoscope.IsTokenScope):
		return reasonBearerMintScopes
	}
	return ""
}

// adoPATAuditConnect is the connect row a minted_pat row's capture writes
// beside ado.signin.capture: this person's sign-in can now create tokens.
const adoPATAuditConnect = "ado_pat.connect"

// adoPATNeedsConsoleAppRefusal is S1's sentence for the admin.
const adoPATNeedsConsoleAppRefusal = "Per-run tokens need your Wardyn app registration to have a client secret. " +
	"Set WARDYN_OIDC_CLIENT_SECRET, or choose another way to connect."

// auditADOPATConnect writes ado_pat.connect after a stored capture on a
// minted_pat row, and nothing for any other mode. Never a token.
func (s *Server) auditADOPATConnect(ctx context.Context, subject string, cfg ADOEntraConfig, granted []string, source string) {
	if cmpTokenMode(cfg.TokenMode) != types.ADOTokenModeMintedPAT {
		return
	}
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorHuman, subject, adoPATAuditConnect, adoEntraSecretName(cfg.RowID),
		"success", mustJSON(map[string]any{"provider_row": cfg.RowID, "client_id": cfg.ClientID, "scopes": granted, "source": source})))
}
