// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// cli_azure_signin.go is the Azure CLI row's sign-in and what the component
// gate asks of it. GET /components-entra/signin?component=<id> starts the
// per-person Entra capture for an org row of cli.kind azure the caller may
// attach, for the one audience an Azure CLI row is captured for (Azure
// Resource Manager). The capture machinery (identity binding, sealed blob,
// redemption) is ado_entra*.go's, run under the azure_arm scope policy
// (entra_capture.go); the refresh token lands sealed in the caller's own
// namespace under wardyn-cli-<component id>-entra.
//
// ONE REDIRECT URI, as for a model provider's Azure sign-in
// (azure_foundry_entra.go): the callback is the Azure DevOps callback, and it
// tells this flow apart by the state cookie this start leg sets.
//
// THE ROW IS NAMED BY A COOKIE THE BROWSER HOLDS, AND THE COOKIE IS NOT SIGNED.
// The start leg stamps the row's id and a digest of the subscriptions it pins,
// and the callback treats both as claims to re-check: nothing is stored unless
// the row still exists as an Azure CLI row pinning the same subscriptions and
// the caller may still attach it. The last check runs under the row's lock,
// which the row's purge also holds, so a sign-in for a row that has since been
// deleted or re-pinned never lands.

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The route that starts a capture, and the console destinations its callback
// lands on. The start route takes the component id as `component`.
const (
	azureCLISignInRoute     = "/components-entra/signin"
	azureCLISignInDonePath  = "/?azure_cli_signin=connected"
	azureCLISignInErrorPath = "/?azure_cli_signin_error="
)

// The one-time cookies the start leg sets and the callback spends: this flow's
// own, so an abandoned sign-in of another kind never spends them.
const (
	azureCLIStateCookieName = "wardyn_azcli_state"
	azureCLINonceCookieName = "wardyn_azcli_nonce"
	azureCLIPKCECookieName  = "wardyn_azcli_pkce"
)

// DRAFT (canon pending).
const (
	cliSignInNotAzure           = "this component is not an Azure CLI, so there is nothing to sign in to"
	azureCLISignInNoSession     = "An Azure sign-in must be started from a signed-in browser session: the captured sign-in is bound to your identity provider subject, and an admin-token caller has none"
	azureCLISignInUnconfigured  = "This deployment has no console Entra sign-in configured, which the Azure CLI requires"
	azureCLISignInRequired      = "components[%d]: sign in to Azure for %q first, then start the run."
	azureCLIReauthRaisedNote    = "This run's Azure CLI sign-in can no longer be renewed — the request is held while its owner signs in to Azure again, and resumes when the sign-in lands; nothing is substituted"
	azureCLISignInClosedRefusal = "This run's Azure CLI sign-in request is closed: %s"
	azureCLITooManyRefusal      = "This run has already asked for an Azure sign-in too many times; no further sign-in will be requested for it"
	azureCLIRaiseFailedBody     = "Could not raise the Azure sign-in request"
)

// azureCLIState is what the start leg stamps in the state cookie beside the
// random state. The callback re-checks both other fields against the live row.
type azureCLIState struct {
	State       string `json:"s"`
	ComponentID string `json:"c"`
	Pin         string `json:"p"`
}

func (st azureCLIState) encode() string {
	raw, err := json.Marshal(st)
	if err != nil {
		panic(fmt.Errorf("api: encode the azure cli sign-in state: %w", err))
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// azureCLIPin is the digest of what an Azure CLI row's sign-in is for: the
// audience and the subscriptions the row pins. A row that changes either is a
// different sign-in.
func azureCLIPin(az types.CLIAzure) string {
	sum := sha256.Sum256([]byte(azureARMAudience + "\n" + strings.Join(azureCLISubscriptions(az), "\n")))
	return hex.EncodeToString(sum[:])
}

// azureCLISignInPinChanged reports whether an org row's update leaves its
// Azure sign-ins captured for subscriptions it no longer pins.
func azureCLISignInPinChanged(before, after types.ComponentDefinition) bool {
	was := cliAzureOf(before)
	if was == nil {
		return false
	}
	now := cliAzureOf(after)
	return now == nil || azureCLIPin(*now) != azureCLIPin(*was)
}

func cliAzureOf(d types.ComponentDefinition) *types.CLIAzure {
	if d.CLI == nil || d.CLI.Kind != types.CLIKindAzure {
		return nil
	}
	return d.CLI.Azure
}

// componentAzureCLIAdmitted is the component gate's Azure CLI step: one Azure
// CLI row per run (the proxy holds one credential for management.azure.com),
// refused where no console Entra sign-in exists or the caller is not a person,
// and only once the caller has signed in for it. The preview reports a missing
// sign-in rather than refusing.
func (s *Server) componentAzureCLIAdmitted(r *http.Request, adm *componentAdmission, c *attachedComponent, i int) *componentRefusal {
	if attachedAzureCLI(*c) == nil {
		return nil
	}
	id := c.snapshot.ComponentID.String()
	own := func(msg string) *componentRefusal {
		// The person's own step, so never "ask your admin" (componentRefused).
		return &componentRefusal{refusal: runError(http.StatusUnprocessableEntity, reasonComponentCLISignInRequired, msg),
			reason: reasonComponentCLISignInRequired, ordinal: i, source: c.source, id: c.snapshot.ComponentID}
	}
	switch _, configured := s.azureCLIEntra(id); {
	case adm.azureCLI:
		return componentRefused(*c, i, reasonComponentDefinitionInvalid, azureCLISecondRowRefusal, "is a second Azure CLI component, and a run attaches one.")
	case !configured:
		return componentRefused(*c, i, reasonComponentCLIAzureNoEntra, azureCLINoEntraRefusal,
			"is the Azure CLI, which needs this deployment's console to sign people in with Microsoft Entra.")
	}
	adm.azureCLI = true
	signedIn := false
	if s.credentialPerson(adm.caller) && !previewHidesOwnCredential(r.Context()) {
		var err error
		if signedIn, err = s.azureCLISignedIn(secretstore.WithPurpose(r.Context(), secretstore.PurposeStatus), adm.caller, id); err != nil {
			return &componentRefusal{refusal: runServerError("read the Azure CLI sign-in", err), reason: reasonStoreError,
				ordinal: i, source: c.source, id: c.snapshot.ComponentID}
		}
	}
	switch {
	case signedIn:
		return nil
	case isServicePrincipal(adm.caller):
		return own(fmt.Sprintf("components[%d]: %s", i, azureCLINotServiceRefusal))
	case !adm.credentials:
		c.needsSignIn = true
		return nil
	case !s.credentialPerson(adm.caller):
		return own(fmt.Sprintf("components[%d]: %s", i, azureCLINotPersonRefusal))
	}
	return own(fmt.Sprintf(azureCLISignInRequired, i, c.snapshot.Name))
}

// azureCLISignInItem is the setup item a missing Azure CLI sign-in adds.
func azureCLISignInItem(componentID, name string) SetupItem {
	return SetupItem{Kind: "cli_access", ID: "cli_access:" + componentID, Label: "Sign in to Azure for " + name,
		RequiredBy: name, Status: "missing", Fix: &SetupFix{Action: fixSignIn, Target: signInAzureCLI, ID: componentID}}
}

// azureCLISignInRow resolves the row and the application a sign-in for the
// component the request names runs against, and applies the checks before any redirect.
// A row the caller may not attach answers exactly as the run doors answer it,
// so whether it exists is not observable. ok=false: the response is written.
func (s *Server) azureCLISignInRow(w http.ResponseWriter, r *http.Request) (uuid.UUID, types.CLIAzure, ADOEntraConfig, bool) {
	none := func() (uuid.UUID, types.CLIAzure, ADOEntraConfig, bool) {
		return uuid.Nil, types.CLIAzure{}, ADOEntraConfig{}, false
	}
	if _, ok := s.componentStoreOr501(w); !ok {
		return none()
	}
	id, err := uuid.Parse(strings.TrimSpace(r.URL.Query().Get("component")))
	if err != nil || id == uuid.Nil {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidIDParam, "invalid component id")
		return none()
	}
	if s.componentAttachRefusal(r, id.String()).write(s, w, r) {
		return none()
	}
	az, ok := s.azureCLIRow(r.Context(), id)
	if !ok {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonCLISignInNotAzure, cliSignInNotAzure)
		return none()
	}
	cfg, ok := s.azureCLIEntra(id.String())
	if !ok {
		writeErrorReason(w, http.StatusNotFound, reasonAzureSignInUnconfigured, azureCLISignInUnconfigured)
		return none()
	}
	return id, az, cfg, true
}

// handleAzureCLISignIn starts the capture for the Azure CLI row `component` names:
// it stamps the state cookie, sets the nonce and code verifier, and redirects
// to the tenant's authorization endpoint asking for Resource Manager's
// `.default`, offline access and openid, and nothing else.
func (s *Server) handleAzureCLISignIn(w http.ResponseWriter, r *http.Request) {
	if oidcHumanFromContext(r.Context()) == "" {
		writeErrorReason(w, http.StatusForbidden, reasonAzureSignInNoSession, azureCLISignInNoSession)
		return
	}
	if previewHidesOwnCredential(r.Context()) {
		writeErrorReason(w, http.StatusConflict, reasonCLISignInPreviewBlocked, cliSignInPreview)
		return
	}
	id, az, cfg, ok := s.azureCLISignInRow(w, r)
	if !ok {
		return
	}
	prompt, err := adoRequestedPrompt(r.URL.Query())
	if err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonADOSignInPromptInvalid, err.Error())
		return
	}
	verifier := oauth2.GenerateVerifier()
	state, nonce := adoRandomToken(), adoRandomToken()
	authURL, err := cfg.authorizeURL(state, nonce, oauth2.S256ChallengeFromVerifier(verifier),
		[]string{azureARMAudience, entraOfflineAccessScope, entraOpenIDScope}, prompt)
	if err != nil {
		writeServerError(w, r, "composing the Azure authorization request failed", err)
		return
	}
	stamp := azureCLIState{State: state, ComponentID: id.String(), Pin: azureCLIPin(az)}
	http.SetCookie(w, s.adoCookie(azureCLIStateCookieName, stamp.encode()))
	http.SetCookie(w, s.adoCookie(azureCLINonceCookieName, nonce))
	http.SetCookie(w, s.adoCookie(azureCLIPKCECookieName, verifier))
	http.Redirect(w, r, authURL, http.StatusFound)
}

// azureCLIFlow reports whether this callback belongs to an Azure CLI capture:
// its state cookie decodes and its state is the one the authority returned.
func (s *Server) azureCLIFlow(r *http.Request) (azureCLIState, bool) {
	c, err := r.Cookie(s.consoleCookieName(azureCLIStateCookieName))
	if err != nil || c.Value == "" {
		return azureCLIState{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	var st azureCLIState
	stateParam := r.URL.Query().Get("state")
	if err != nil || json.Unmarshal(raw, &st) != nil || st.State == "" || stateParam == "" ||
		subtle.ConstantTimeCompare([]byte(stateParam), []byte(st.State)) != 1 {
		return azureCLIState{}, false
	}
	return st, true
}

// consumeAzureCLICookies reads the nonce and verifier cookies and, only when
// both are present, spends all three one-time cookies.
func (s *Server) consumeAzureCLICookies(w http.ResponseWriter, r *http.Request) (nonce, verifier string, ok bool) {
	nonceCookie, err := r.Cookie(s.consoleCookieName(azureCLINonceCookieName))
	if err != nil || nonceCookie.Value == "" {
		writeErrorReason(w, http.StatusBadRequest, reasonAzureCallbackCookiesInvalid, "missing nonce cookie")
		return "", "", false
	}
	pkceCookie, err := r.Cookie(s.consoleCookieName(azureCLIPKCECookieName))
	if err != nil || pkceCookie.Value == "" {
		writeErrorReason(w, http.StatusBadRequest, reasonAzureCallbackCookiesInvalid, "missing pkce cookie")
		return "", "", false
	}
	s.clearADOCookie(w, azureCLIStateCookieName)
	s.clearADOCookie(w, azureCLINonceCookieName)
	s.clearADOCookie(w, azureCLIPKCECookieName)
	return nonceCookie.Value, pkceCookie.Value, true
}

// azureCLIStampHolds reports whether the row the stamp names is still an Azure
// CLI row pinning what the sign-in was started for, and the caller may still
// attach it.
func (s *Server) azureCLIStampHolds(r *http.Request, stamp azureCLIState) bool {
	id, err := uuid.Parse(stamp.ComponentID)
	if err != nil || s.componentAttachRefusal(r, stamp.ComponentID) != nil {
		return false
	}
	az, ok := s.azureCLIRow(r.Context(), id)
	return ok && subtle.ConstantTimeCompare([]byte(azureCLIPin(az)), []byte(stamp.Pin)) == 1
}

// handleAzureCLICallback finishes a capture started by handleAzureCLISignIn.
// The row is the cookie's, never the query's. Nothing is stored until every
// check has passed, and the last of them (the row is still what it was) runs
// under the row's lock in the same critical section as the write.
func (s *Server) handleAzureCLICallback(w http.ResponseWriter, r *http.Request, subject string, stamp azureCLIState) {
	ctx := r.Context()
	nonce, verifier, ok := s.consumeAzureCLICookies(w, r)
	if !ok {
		return
	}
	// A forged cookie can name anything. One that is not a component id in the
	// one spelling the start leg stamps is not a sign-in this server started:
	// another spelling would store the capture under a name no purge deletes.
	id, perr := uuid.Parse(stamp.ComponentID)
	ec, err := azureARMCapture(stamp.ComponentID)
	if perr != nil || err != nil || id.String() != stamp.ComponentID {
		writeErrorReason(w, http.StatusBadRequest, reasonAzureCallbackCookiesInvalid, "invalid state parameter")
		return
	}
	fail := func(reason string) {
		s.auditAzureCLICapture(ctx, subject, ec, "failure", map[string]any{"reason": reason})
		http.Redirect(w, r, s.cfg.BasePath+azureCLISignInErrorPath+reason+azureCLIComponentParam(stamp.ComponentID), http.StatusFound)
	}
	// Before the code is spent: the row must still be what the sign-in started
	// for. The authoritative check is the one under the row's lock at the write.
	cfg, configured := s.azureCLIEntra(stamp.ComponentID)
	if !configured || !s.azureCLIStampHolds(r, stamp) {
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
	grant, reason := s.redeemEntraCode(ctx, cfg, ec, subject, code, verifier, nonce)
	if reason != "" {
		fail(reason)
		return
	}
	// The resume of a run a resolved request held takes the run's lock, which must precede the sign-in
	// lock (db.LockOrder): it runs after the lock is released, the deferred calls running last-in first.
	var resolved []uuid.UUID
	defer func() { s.approvalsClosed(ctx, resolved) }()
	lctx, unlock, err := s.lockADOSignIn(ctx, subject, ec.rowUID)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: could not take the Azure sign-in lock", slog.String("row", ec.rowUID), slog.Any("err", err))
		fail(reasonStoreError)
		return
	}
	defer unlock()
	changed, err := s.storeAzureCLICapture(r.WithContext(lctx), subject, stamp, ec, grant.blob)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: storing the captured Azure CLI sign-in failed", slog.String("row", ec.rowUID), slog.Any("err", err))
		fail(reasonStoreError)
		return
	}
	if changed {
		fail(azureCaptureRowChanged)
		return
	}
	s.retireReplacedEntraTokens(lctx, subject, ec, grant)
	s.auditAzureCLICapture(ctx, subject, ec, "success", map[string]any{
		"audience": azureARMAudience, "scopes": grant.blob.Scopes, "expires_at": grant.blob.ExpiresAt.Format(time.RFC3339),
	})
	resolved = s.resolvePendingAzureReauth(lctx, azureCLIMechanism, subject, ec.rowUID)
	http.Redirect(w, r, s.cfg.BasePath+ec.donePath+azureCLIComponentParam(stamp.ComponentID), http.StatusFound)
}

// storeAzureCLICapture stores blob only while the row is still the Azure CLI
// row the sign-in was started for and the caller may still attach it, under
// the lock the row's purge holds (lockCLIRow), so a purge can never land
// between the check and the write. changed=true: refused, nothing stored.
func (s *Server) storeAzureCLICapture(r *http.Request, subject string, stamp azureCLIState, ec entraCapture, blob adoEntraBlob) (changed bool, err error) {
	ctx, unlock, err := s.lockCLIRow(r.Context(), ec.rowUID)
	if err != nil {
		return false, err
	}
	defer unlock()
	if !s.azureCLIStampHolds(r.WithContext(ctx), stamp) {
		return true, nil
	}
	return false, s.storeEntraBlob(ctx, subject, ec, blob)
}

// azureCLIComponentParam names the row the sign-in was started for, so the
// console can say which component connected or failed.
func azureCLIComponentParam(componentID string) string {
	return "&component=" + url.QueryEscape(componentID)
}

// auditAzureCLICapture records cli.signin.capture for an Azure CLI sign-in, on
// both outcomes. data never carries a token, a code or a verifier; Target is
// the sealed secret name in the signer's namespace.
func (s *Server) auditAzureCLICapture(ctx context.Context, actor string, ec entraCapture, outcome string, data map[string]any) {
	data["cli"], data["component_id"], data["owner"] = types.CLIKindAzure, ec.rowUID, actor
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorHuman, actor, "cli.signin.capture", ec.secretName, outcome, mustJSON(data)))
}

// RedeemAzureARMAccess mints a Resource Manager access token for owner from
// their sign-in for one Azure CLI row. The audience is this lane's one
// literal, never a caller's; cfg is the console application. The single-flight
// is keyed by owner and row, as every Entra redemption's is.
func (s *Server) RedeemAzureARMAccess(ctx context.Context, cfg ADOEntraConfig, owner, componentID string) (ADOEntraAccess, error) {
	ec, err := azureARMCapture(componentID)
	if err != nil {
		return ADOEntraAccess{}, err
	}
	if err := cfg.validateApp(); err != nil {
		return ADOEntraAccess{}, err
	}
	if cfg.RowID != componentID {
		return ADOEntraAccess{}, fmt.Errorf("azure cli sign-in: configuration is for row %q, not %q", cfg.RowID, componentID)
	}
	if owner == "" {
		return ADOEntraAccess{}, fmt.Errorf("%w: no principal to redeem it for", ErrADOEntraNotCaptured)
	}
	if err := ec.checkRequested(ec.scopes); err != nil {
		return ADOEntraAccess{}, err
	}
	lctx, unlock, err := s.lockADOSignInRedeem(ctx, owner, componentID)
	if err != nil {
		return ADOEntraAccess{}, err
	}
	defer unlock()
	return s.redeemEntraAccessLocked(lctx, cfg, ec, owner, ec.scopes)
}

// mountAzureCLISignInRoutes mounts the start door on the authenticated human
// group. The callback is the Azure DevOps callback (see the file doc).
func (s *Server) mountAzureCLISignInRoutes(r chi.Router) {
	r.Get(azureCLISignInRoute, s.handleAzureCLISignIn)
}
