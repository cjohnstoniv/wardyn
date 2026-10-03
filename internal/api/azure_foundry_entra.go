// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// azure_foundry_entra.go is the per-row door of the per-person Entra capture for
// azure_foundry providers: a start route that takes the provider row's uid and a
// callback that resolves the row from the one-time state cookie, never from the
// query. The capture machinery (identity binding, sealed blob, redemption) is
// ado_entra*.go's, run under this kind's scope policy (entra_capture.go).
//
// ONE REDIRECT URI. The callback is the Azure DevOps callback path, because
// that is the URI the console's Entra login application already registers; a
// route per row would need an application edit for every new row. The browser
// returns to one URL and the callback tells the two flows apart by the state
// cookie each sets (handleADOCallback hands this one the request when the
// azure state cookie matches the state it came back with).
//
// THE ROW IS NAMED BY A COOKIE THE BROWSER HOLDS, AND THE COOKIE IS NOT SIGNED.
// So the start leg stamps the row uid, the audience its route names and the
// row's address digest beside the random state, and the callback treats all
// three as claims to re-check: it re-reads the row by uid from the current site
// config under the site-config lock, and stores nothing unless the row still exists, is
// an azure_foundry row, and still has the digest and audience the sign-in was
// started for. That is the discipline the provider sign-ins already follow
// (storeProviderSignIn): a capture for an old address must not land after rule
// 8's purge.
//
// THE CONSOLE-LOGIN DOOR IS NEVER WIDENED FOR THIS KIND. Entra refuses a login
// request whose scopes name two resources (AADSTS700022), so mixing an Azure
// audience into the login would break every sign-in. LoginScopes,
// CaptureLoginGrant and adoEntraForLogin read the Azure DevOps source only.

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/oauth2"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// azureSignInCapturedAction is the one audit action this capture owns, on both
// outcomes (the Azure DevOps sign-in's ado.signin.capture is unchanged).
const azureSignInCapturedAction = "azure.signin.capture"

// The route that starts a capture, and the console destinations the callback
// lands on. The start route takes the row uid as `uid`.
const (
	azureFoundrySignInRoute     = "/model-providers-entra/signin"
	azureFoundrySignInDonePath  = "/?azure_signin=connected"
	azureFoundrySignInErrorPath = "/?azure_signin_error="
)

// The one-time cookies the start leg sets and the callback spends. They are not
// the Azure DevOps sign-in's, so an abandoned flow of one kind never spends the
// other's.
const (
	azureStateCookieName = "wardyn_az_state"
	azureNonceCookieName = "wardyn_az_nonce"
	azurePKCECookieName  = "wardyn_az_pkce"
)

// azureCaptureRowChanged is the console code for a capture refused because the
// row it was started for is gone or no longer what it was. It joins the
// fixed vocabulary the other capture failures use (consent_required,
// interaction_required, exchange_failed, identity_binding, unusable_grant,
// store_error).
const azureCaptureRowChanged = "row_changed"

const (
	azureSignInNoSessionRefusal = "An Azure sign-in must be started from a signed-in browser session: " +
		"the captured credential is bound to your identity provider subject, and an admin-token caller has none"
	azureSignInUnconfiguredRefusal = "This deployment has no console Entra sign-in configured, which an Azure Foundry provider requires"
	azureSignInUnknownRowRefusal   = "No Azure Foundry provider has that uid"
)

// azureFoundryState is what the start leg stamps in the state cookie beside the
// random state. The callback re-checks every field but State against the live
// row, never trusts them.
type azureFoundryState struct {
	State    string `json:"s"`
	RowUID   string `json:"u"`
	Audience string `json:"a"`
	Digest   string `json:"d"`
}

func (st azureFoundryState) encode() string {
	raw, err := json.Marshal(st)
	if err != nil {
		panic(fmt.Errorf("api: encode the azure sign-in state: %w", err))
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeAzureFoundryState(v string) (azureFoundryState, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return azureFoundryState{}, false
	}
	var st azureFoundryState
	if err := json.Unmarshal(raw, &st); err != nil || st.State == "" {
		return azureFoundryState{}, false
	}
	return st, true
}

// matches reports whether the row the stamp names still exists in sc as an
// azure_foundry row with the audience and address digest the sign-in was
// started for.
func (st azureFoundryState) matches(sc types.SiteConfig) bool {
	facts, ok := azureFoundryRowFor(sc, st.RowUID)
	return ok && facts.audience == st.Audience && facts.digest == st.Digest
}

// azureFoundryRowFacts are the two row-derived values a capture is stamped
// with and re-checked against.
type azureFoundryRowFacts struct {
	audience string
	digest   string
}

// azureFoundryRowFor reads the azure_foundry row with this uid out of sc. ok=false
// when there is none: no such uid, another kind, or a route the audience table
// does not name.
func azureFoundryRowFor(sc types.SiteConfig, uid string) (azureFoundryRowFacts, bool) {
	if uid == "" {
		return azureFoundryRowFacts{}, false
	}
	for _, p := range modelProviderRows(sc) {
		if p.UID != uid {
			continue
		}
		if p.Kind != types.ModelProviderAzureFoundry || p.Azure == nil {
			return azureFoundryRowFacts{}, false
		}
		audience, ok := azureAudienceForRoute(p.Azure.Route)
		if !ok {
			return azureFoundryRowFacts{}, false
		}
		return azureFoundryRowFacts{audience: audience, digest: providerAddressDigest(p)}, true
	}
	return azureFoundryRowFacts{}, false
}

// resolveAzureFoundry resolves the row and the application a sign-in for uid
// runs against, and applies the checks both legs share. ok=false means the
// response has been written.
func (s *Server) resolveAzureFoundry(w http.ResponseWriter, r *http.Request, uid string) (azureFoundryRowFacts, ADOEntraConfig, bool) {
	if s.cfg.Store == nil || s.cfg.AzureFoundryEntra == nil {
		writeErrorReason(w, http.StatusNotFound, reasonAzureSignInUnconfigured, azureSignInUnconfiguredRefusal)
		return azureFoundryRowFacts{}, ADOEntraConfig{}, false
	}
	sc, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeServerError(w, r, "reading the site configuration failed", err)
		return azureFoundryRowFacts{}, ADOEntraConfig{}, false
	}
	facts, ok := azureFoundryRowFor(sc, uid)
	if !ok {
		writeErrorReason(w, http.StatusNotFound, reasonAzureSignInUnknownRow, azureSignInUnknownRowRefusal)
		return azureFoundryRowFacts{}, ADOEntraConfig{}, false
	}
	cfg, found, err := s.cfg.AzureFoundryEntra(r.Context(), uid)
	if err != nil {
		writeServerError(w, r, "reading the Azure sign-in configuration failed", err)
		return azureFoundryRowFacts{}, ADOEntraConfig{}, false
	}
	if !found {
		writeErrorReason(w, http.StatusNotFound, reasonAzureSignInUnknownRow, azureSignInUnknownRowRefusal)
		return azureFoundryRowFacts{}, ADOEntraConfig{}, false
	}
	if err := cfg.validateApp(); err != nil {
		writeServerError(w, r, "the Azure sign-in configuration is unusable", err)
		return azureFoundryRowFacts{}, ADOEntraConfig{}, false
	}
	// The application is always the console's own sign-in application (D4): a
	// deployment without Entra console login cannot use the kind.
	if !cfg.isLoginApplication() {
		writeErrorReason(w, http.StatusNotFound, reasonAzureSignInUnconfigured, azureSignInUnconfiguredRefusal)
		return azureFoundryRowFacts{}, ADOEntraConfig{}, false
	}
	if _, err := cfg.authority(); err != nil {
		writeServerError(w, r, "the Azure sign-in authority is refused", err)
		return azureFoundryRowFacts{}, ADOEntraConfig{}, false
	}
	return facts, cfg, true
}

// handleAzureFoundrySignIn starts the capture for the row named by `uid`: it
// stamps the state cookie, sets the nonce and code verifier, and redirects to
// the tenant's authorization endpoint asking for `<resource>/.default
// offline_access openid` and nothing else.
func (s *Server) handleAzureFoundrySignIn(w http.ResponseWriter, r *http.Request) {
	subject := oidcHumanFromContext(r.Context())
	if subject == "" {
		writeErrorReason(w, http.StatusForbidden, reasonAzureSignInNoSession, azureSignInNoSessionRefusal)
		return
	}
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	facts, cfg, ok := s.resolveAzureFoundry(w, r, uid)
	if !ok {
		return
	}
	verifier := oauth2.GenerateVerifier()
	state, nonce := adoRandomToken(), adoRandomToken()
	authURL, err := cfg.authorizeURL(state, nonce, oauth2.S256ChallengeFromVerifier(verifier),
		[]string{facts.audience, entraOfflineAccessScope, entraOpenIDScope}, "")
	if err != nil {
		writeServerError(w, r, "composing the Azure authorization request failed", err)
		return
	}
	stamp := azureFoundryState{State: state, RowUID: uid, Audience: facts.audience, Digest: facts.digest}
	http.SetCookie(w, s.adoCookie(azureStateCookieName, stamp.encode()))
	http.SetCookie(w, s.adoCookie(azureNonceCookieName, nonce))
	http.SetCookie(w, s.adoCookie(azurePKCECookieName, verifier))
	http.Redirect(w, r, authURL, http.StatusFound)
}

// azureFoundryFlow reports whether this callback belongs to an Azure Foundry
// capture: the azure state cookie decodes and its state is the one the
// authority returned. Anything else is the Azure DevOps callback's to answer.
func (s *Server) azureFoundryFlow(r *http.Request) (azureFoundryState, bool) {
	c, err := r.Cookie(s.consoleCookieName(azureStateCookieName))
	if err != nil || c.Value == "" {
		return azureFoundryState{}, false
	}
	st, ok := decodeAzureFoundryState(c.Value)
	stateParam := r.URL.Query().Get("state")
	if !ok || stateParam == "" || subtle.ConstantTimeCompare([]byte(stateParam), []byte(st.State)) != 1 {
		return azureFoundryState{}, false
	}
	return st, true
}

// consumeAzureFoundryCookies reads the nonce and verifier cookies and, only when
// both are present, spends all three one-time cookies (the Azure DevOps
// discipline: a request that never reaches the exchange spends nothing).
func (s *Server) consumeAzureFoundryCookies(w http.ResponseWriter, r *http.Request) (nonce, verifier string, ok bool) {
	nonceCookie, err := r.Cookie(s.consoleCookieName(azureNonceCookieName))
	if err != nil || nonceCookie.Value == "" {
		writeErrorReason(w, http.StatusBadRequest, reasonAzureCallbackCookiesInvalid, "missing nonce cookie")
		return "", "", false
	}
	pkceCookie, err := r.Cookie(s.consoleCookieName(azurePKCECookieName))
	if err != nil || pkceCookie.Value == "" {
		writeErrorReason(w, http.StatusBadRequest, reasonAzureCallbackCookiesInvalid, "missing pkce cookie")
		return "", "", false
	}
	s.clearADOCookie(w, azureStateCookieName)
	s.clearADOCookie(w, azureNonceCookieName)
	s.clearADOCookie(w, azurePKCECookieName)
	return nonceCookie.Value, pkceCookie.Value, true
}

// azureFoundryRedirect sends the browser to the console's error page with a
// fixed reason code.
func (s *Server) azureFoundryRedirect(w http.ResponseWriter, r *http.Request, reason string) {
	http.Redirect(w, r, s.cfg.BasePath+azureFoundrySignInErrorPath+reason, http.StatusFound)
}

// handleAzureFoundryCallback finishes a capture started by
// handleAzureFoundrySignIn. The row is the cookie's, never the query's: a `uid`
// in the callback query is not read at all.
//
// Nothing is stored until every check has passed, and the last of them (the row
// is still what it was) runs under the site-config lock in the same critical section as
// the write, so a purge cannot land between them.
func (s *Server) handleAzureFoundryCallback(w http.ResponseWriter, r *http.Request, subject string, stamp azureFoundryState) {
	ctx := r.Context()
	nonce, verifier, ok := s.consumeAzureFoundryCookies(w, r)
	if !ok {
		return
	}
	// A forged cookie can name anything. One that does not compose into a store
	// name and a closed audience is not a sign-in this server started.
	ec, err := azureFoundryCapture(stamp.RowUID, stamp.Audience)
	if err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonAzureCallbackCookiesInvalid, "invalid state parameter")
		return
	}
	fail := func(reason string) {
		s.auditAzureCapture(ctx, subject, ec, "failure", map[string]any{"reason": reason})
		s.azureFoundryRedirect(w, r, reason)
	}
	// Before the code is spent: the row must still be what the sign-in started
	// for. The authoritative check is the one under the site-config lock at the write.
	if s.cfg.Store == nil || s.cfg.AzureFoundryEntra == nil {
		fail(azureCaptureRowChanged)
		return
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: reading the site configuration failed", slog.String("row", ec.rowUID), slog.Any("err", err))
		fail(reasonStoreError)
		return
	}
	if !stamp.matches(sc) {
		fail(azureCaptureRowChanged)
		return
	}
	cfg, found, err := s.cfg.AzureFoundryEntra(ctx, ec.rowUID)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: reading the azure sign-in configuration failed", slog.String("row", ec.rowUID), slog.Any("err", err))
		fail(reasonStoreError)
		return
	}
	if !found || cfg.validateApp() != nil || !cfg.isLoginApplication() {
		fail(azureCaptureRowChanged)
		return
	}
	if code := r.URL.Query().Get("error"); code != "" {
		fail(adoCaptureErrorCode(classifyADOEntraError(code, r.URL.Query().Get("error_description"))))
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		writeErrorReason(w, http.StatusBadRequest, reasonAzureCallbackMissingCode, "missing code parameter")
		return
	}
	resp, err := s.postADOEntraToken(ctx, cfg, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {cfg.RedirectURL},
	})
	if err != nil {
		reason := adoCaptureErrorCode(err)
		slog.WarnContext(ctx, "wardynd: the azure sign-in code exchange failed",
			slog.String("row", ec.rowUID), slog.String("reason", reason), slog.Any("err", err))
		fail(reason)
		return
	}
	now := s.cfg.Now()
	accessExpiry := now.Add(time.Duration(resp.ExpiresIn) * time.Second).UTC()
	s.cfg.MaskRegistry.MergeGlobalUntil(subject, ec.secretName, accessExpiry, []byte(resp.AccessToken), []byte(resp.RefreshToken))

	if reason, ok := s.bindADOEntraIdentity(ctx, cfg, resp.IDToken, nonce, subject); !ok {
		slog.WarnContext(ctx, "wardynd: azure sign-in identity binding failed",
			slog.String("row", ec.rowUID), slog.String("reason", reason))
		fail(reasonADOCallbackIdentityBinding)
		return
	}
	granted := ec.capturedScopes(resp.Scope)
	if resp.RefreshToken == "" || len(granted) == 0 {
		fail(reasonADOCallbackUnusableGrant)
		return
	}
	blob := adoEntraBlob{
		RefreshToken: resp.RefreshToken,
		Scopes:       granted,
		ExpiresAt:    accessExpiry,
		TenantID:     cfg.TenantID,
		ClientID:     cfg.ClientID,
		Subject:      subject,
		CapturedAt:   now.UTC(),
		Source:       adoEntraSourceSignIn,
	}
	ctx, unlock, err := s.lockADOSignIn(ctx, subject, ec.rowUID)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: could not take the Azure sign-in lock", slog.String("row", ec.rowUID), slog.Any("err", err))
		fail(reasonStoreError)
		return
	}
	defer unlock()
	reason, err := s.storeAzureFoundryCapture(ctx, subject, stamp, ec, blob)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: storing the captured Azure sign-in failed", slog.String("row", ec.rowUID), slog.Any("err", err))
		fail(reasonStoreError)
		return
	}
	if reason != "" {
		fail(reason)
		return
	}
	s.cfg.MaskRegistry.AddGlobalUntil(subject, ec.secretName, s.cfg.Now(), blob.ExpiresAt, []byte(resp.AccessToken), []byte(resp.RefreshToken))
	s.auditAzureCapture(ctx, subject, ec, "success", map[string]any{
		"audience": stamp.Audience, "scopes": granted,
		"expires_at": blob.ExpiresAt.Format(time.RFC3339),
	})
	http.Redirect(w, r, s.cfg.BasePath+ec.donePath, http.StatusFound)
}

// storeAzureFoundryCapture stores blob only while the row is still the one the
// sign-in was started for: it exists, is an azure_foundry row, and has the
// audience and address digest the start leg stamped. Under the site-config lock, which
// rule 8's purge also holds, so a purge can never land between the check and the
// write. A non-empty reason means refused and nothing stored.
func (s *Server) storeAzureFoundryCapture(ctx context.Context, subject string, stamp azureFoundryState, ec entraCapture, blob adoEntraBlob) (string, error) {
	ctx, unlock, err := s.lock(ctx, db.SiteConfigLockClass)
	if err != nil {
		return "", err
	}
	defer unlock()
	if s.cfg.Store == nil {
		return "", errors.New("no store configured")
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return "", fmt.Errorf("read model providers: %w", err)
	}
	if !stamp.matches(sc) {
		return azureCaptureRowChanged, nil
	}
	return "", s.storeEntraBlob(ctx, subject, ec, blob)
}

// auditAzureCapture writes the capture's one audit row, on both outcomes. data
// never carries a token, a code or a verifier; Target is the sealed secret name,
// so "whose Azure access, for which row" is answerable without a join.
func (s *Server) auditAzureCapture(ctx context.Context, actor string, ec entraCapture, outcome string, data map[string]any) {
	data["owner"] = actor
	data["provider"] = string(entraKindAzureFoundry)
	data["source"] = adoEntraSourceSignIn
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorHuman, actor,
		azureSignInCapturedAction, ec.secretName, outcome, mustJSON(data)))
}

// RedeemAzureFoundryAccess mints an access token for owner, for the audience
// this row's capture names. The audience comes from the dispatch snapshot, so it
// is refused unless it is one of the two closed literals BEFORE a token request
// is built: a tampered snapshot cannot point a person's refresh token at a
// resource the table does not name. cfg is the console application (the by-uid
// source's answer).
//
// The single-flight is keyed by owner and row uid, exactly as Azure DevOps'
// is by owner and row.
func (s *Server) RedeemAzureFoundryAccess(ctx context.Context, cfg ADOEntraConfig, owner, uid, audience string) (ADOEntraAccess, error) {
	ec, err := azureFoundryCapture(uid, audience)
	if err != nil {
		return ADOEntraAccess{}, err
	}
	if err := cfg.validateApp(); err != nil {
		return ADOEntraAccess{}, err
	}
	if cfg.RowID != uid {
		return ADOEntraAccess{}, fmt.Errorf("azure foundry sign-in: configuration is for row %q, not %q", cfg.RowID, uid)
	}
	if owner == "" {
		return ADOEntraAccess{}, fmt.Errorf("%w: no principal to redeem it for", ErrADOEntraNotCaptured)
	}
	if err := ec.checkRequested(ec.scopes); err != nil {
		return ADOEntraAccess{}, err
	}
	lctx, unlock, err := s.lockADOSignInRedeem(ctx, owner, uid)
	if err != nil {
		return ADOEntraAccess{}, err
	}
	defer unlock()
	return s.redeemEntraAccessLocked(lctx, cfg, ec, owner, ec.scopes)
}

// mountAzureFoundrySignInRoutes mounts the start door on the authenticated
// human group. The callback is the Azure DevOps callback (see the file doc).
func (s *Server) mountAzureFoundrySignInRoutes(r chi.Router) {
	r.Get(azureFoundrySignInRoute, s.handleAzureFoundrySignIn)
}
