// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The sign-in doors keyed by model provider (MP-13). POST
// /model-providers/{id}/sign-in launches the login sandbox for a bedrock_sso or
// anthropic_subscription provider; its capture lands in the caller's own
// namespace under that provider's UID-keyed name (wardyn-provider-<uid>-sso or
// -oauth), never the roster's harness name and never the operator's. An AWS
// sign-in is stored by the sandbox's own helper upload (ssotoken.go, bound to
// the provider as it read at launch); a Claude sign-in by PUT on the same path,
// with the setup-token the sandbox printed, bound to that sign-in's own run.
//
// The doors answer only while the model-provider block exists, and POST
// /setup/harness-login only while it does not: the two never both apply.
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
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// DRAFT (M2 canon pending).
const (
	mpsNoBlock     = "this install has no model providers yet, so sign in from Setup instead"
	mpsLegacyDoor  = "this install signs in to each model provider on its own — sign in from Getting started in the console"
	mpsTyped       = "%q is connected with your own key or token, not by signing in"
	mpsOff         = "model provider %q is turned off, so there is nothing to sign in to"
	mpsNotGranted  = "you are not granted model provider %q — ask an admin to grant it before signing in to it"
	mpsNoPortal    = "model provider %q has no AWS access portal or region set — ask your admin to set them before you sign in"
	mpsAccounts    = "model provider %q serves models in more than one AWS account and pins none — ask your admin to pin the account and role before you sign in"
	mpsNoImage     = "signing in to Claude needs the Claude Code sign-in image, which this install hasn't built yet. See Operations → Claude sign-in image."
	mpsPreview     = "Exit member mode to sign in — the capture would land on your own identity."
	mpsUnreadable  = "Wardyn couldn't read its model providers just now — nothing was started. Try again in a moment."
	mpsCaptureBody = `body must be {"run_id":"<your sign-in run>","token":"<the claude setup-token output>"}`
	mpsAWSByHelper = "%q stores your AWS sign-in itself when you finish it in the sign-in sandbox — there is nothing to paste"
	mpsNotYourRun  = "that is not a Claude sign-in you started for model provider %q — start the sign-in again"
	// The upload's refusals for a provider sign-in (handleUploadSSOToken); the
	// second is the Claude PUT's too.
	mpsCaptureNotOwner = "this sign-in was started by someone else, so it cannot be stored for you — start the sign-in again"
	mpsCaptureChanged  = "the model provider this sign-in was for was removed, changed or re-addressed while it was open — start the sign-in again"
)

// loginTarget is what a login sandbox signs in for: the access portal, SSO
// region and pin it is seeded with and an AWS upload binds to, whose namespace
// the capture lands in, and — on the provider door only — the model provider.
type loginTarget struct {
	startURL string
	region   string
	pin      awsSSOPin
	scope    awsSSOScope
	provider *types.ModelProvider
	model    string // signInModel's, on the provider door
}

// stampProvider adds the provider half of harness.login.started (the stamp an
// upload binds to): nothing on the legacy door.
func (t loginTarget) stampProvider(stamp map[string]any) {
	if t.provider == nil {
		return
	}
	stamp["model_provider"], stamp["model_provider_uid"] = t.provider.ID, t.provider.UID
	stamp["model_provider_address"] = providerAddressDigest(*t.provider)
	stamp["sso_region"], stamp["model"] = t.region, t.model
}

// signInModel is the model whose account an unpinned AWS sign-in's session is
// bound to (bindCaptureToPin), from the harnesses on p the caller may launch:
// one that names an account when any does. ok=false: two name different
// accounts, which no one session can serve.
func signInModel(p types.ModelProvider, launchable []string) (model string, ok bool) {
	for _, h := range p.Harnesses {
		if !slices.Contains(launchable, h.Harness) {
			continue
		}
		have, next := bedrockModelAccount(model), bedrockModelAccount(h.Model)
		switch {
		case model == "" || (have == "" && next != ""):
			model = h.Model
		case next != "" && next != have:
			return model, false
		}
	}
	return model, true
}

// withModelProvider adds model_provider to an audit datum when there is one.
func withModelProvider(data map[string]any, id string) map[string]any {
	if id != "" {
		data["model_provider"] = id
	}
	return data
}

// reauthScopeForRun is the scope whose stored session answers run's AWS
// sign-in holds: the chosen provider's own name for a provider run, the
// roster's otherwise. ok=false: the run's provider is gone or no longer an AWS
// sign-in, so nothing can answer.
func reauthScopeForRun(sc types.SiteConfig, run types.AgentRun, owner string) (awsSSOScope, bool) {
	if run.ModelProviderID == "" {
		return awsSSOScopeFor(sc, run.Agent, owner), true
	}
	p, ok := modelProviderByID(sc.ModelProviders, run.ModelProviderID)
	if !ok || p.Kind != types.ModelProviderBedrockSSO {
		return awsSSOScope{}, false
	}
	return chosenProvider{provider: p, owner: owner}.awsScope(), true
}

// storeProviderSignIn stores an AWS provider sign-in only while its provider
// is still the one the sign-in was launched for: same UID and kind, same
// address (rule 8's, digested), region, portal and pin. Under siteConfigMu,
// which rule 8's purge also holds, so a purge can never land between the check
// and the write and leave a session behind for an old address. changed=true:
// refused, nothing stored.
func (s *Server) storeProviderSignIn(ctx context.Context, stamp loginRunStamp, scope awsSSOScope, blob awsSSOBlob) (bool, error) {
	s.siteConfigMu.Lock()
	defer s.siteConfigMu.Unlock()
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return false, fmt.Errorf("read model providers: %w", err)
	}
	p, ok := modelProviderByID(sc.ModelProviders, stamp.ModelProvider)
	b := providerBedrockSettings(p)
	if !ok || p.UID != stamp.ModelProviderUID || p.Kind != types.ModelProviderBedrockSSO ||
		providerAddressDigest(p) != stamp.ModelProviderAddress || b.Region != stamp.SSORegion || b.SSOStartURL != stamp.SSOStartURL ||
		b.SSOAccountID != stamp.SSOAccountID || b.SSORoleName != stamp.SSORoleName {
		return true, nil
	}
	return false, s.storeAWSSSOBlob(ctx, scope, blob)
}

// mountProviderSignInRoutes registers the sign-in doors on the authenticated
// group: every person, admins included, signs in for themselves, and the
// handlers decide who may.
func (s *Server) mountProviderSignInRoutes(r chi.Router) {
	r.Post("/model-providers/{id}/sign-in", s.handleProviderSignIn)
	r.Put("/model-providers/{id}/sign-in", s.handleProviderSignInCapture)
}

// signInProvider reads the provider {id} names as this caller may sign in to
// it, from sc: the block must exist; the provider must serve an agent the
// caller may launch (capAgent — the /setup/status projection's own rule, so a
// provider the caller cannot see is a 404) and be granted by capModelProvider;
// and it must be on and a sign-in kind. Returns its login convention and the
// harnesses on it the caller may launch. ok=false: the refusal is written.
func (s *Server) signInProvider(w http.ResponseWriter, r *http.Request, sc types.SiteConfig) (types.ModelProvider, harnessLogin, []string, bool) {
	id := chi.URLParam(r, "id")
	if sc.ModelProviders == nil {
		writeError(w, http.StatusConflict, mpsNoBlock)
		return types.ModelProvider{}, harnessLogin{}, nil, false
	}
	p, ok := modelProviderByID(sc.ModelProviders, id)
	var launchable []string
	if ok {
		rows := s.setupModelProviders(r.Context(), sc)
		i := slices.IndexFunc(rows, func(v SetupModelProvider) bool { return v.ID == id })
		if ok = i >= 0; ok {
			launchable = rows[i].Harnesses
		}
	}
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf(mpcNotFound, id))
		return types.ModelProvider{}, harnessLogin{}, nil, false
	}
	if s.denyMemberCapability(w, r, capModelProvider, p.ID, "model_provider.sign_in", fmt.Sprintf(mpsNotGranted, p.ID)) {
		return types.ModelProvider{}, harnessLogin{}, nil, false
	}
	login := map[types.ModelProviderKind]string{
		types.ModelProviderBedrockSSO: awsSSOProvider, types.ModelProviderAnthropicSubscription: "anthropic",
	}[p.Kind]
	hl, ok := harnessLoginByProvider(login)
	switch {
	case !ok:
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf(mpsTyped, p.ID))
		return types.ModelProvider{}, harnessLogin{}, nil, false
	case p.Disabled:
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf(mpsOff, p.ID))
		return types.ModelProvider{}, harnessLogin{}, nil, false
	}
	return p, hl, launchable, true
}

// handleProviderSignIn launches the caller's own sign-in for one provider:
//
//	POST /api/v1/model-providers/{id}/sign-in
//
// No body: everything the sandbox is seeded with comes from the provider
// record (an AWS start URL, region and pin are admin-owned), and the capture
// is stamped for the caller's own namespace under the provider's UID. Answers
// with the run id as soon as the run exists, like POST /setup/harness-login.
func (s *Server) handleProviderSignIn(w http.ResponseWriter, r *http.Request) {
	owner := s.credentialOwner(w, r)
	if owner == "" {
		return
	}
	sc, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, mpsUnreadable)
		return
	}
	p, hl, launchable, ok := s.signInProvider(w, r, sc)
	if !ok {
		return
	}
	// After the authorization, as on the legacy door: a capture here would
	// land on the previewing admin's own namespace.
	if previewHidesOwnCredential(r.Context()) {
		writeError(w, http.StatusConflict, mpsPreview)
		return
	}
	model, oneAccount := signInModel(p, launchable)
	t := loginTarget{scope: chosenProvider{provider: p, owner: owner}.awsScope(), provider: &p, model: model}
	if hl.regionalSSOEgress {
		b := providerBedrockSettings(p)
		t.startURL, t.region = b.SSOStartURL, b.Region
		t.pin = awsSSOPin{AccountID: b.SSOAccountID, RoleName: b.SSORoleName}
		if t.region == "" || validateSSOStartURL(t.startURL) != nil {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf(mpsNoPortal, p.ID))
			return
		}
		// A pin outranks the model's account (bindCaptureToPin); unpinned, one
		// session must serve every model the caller may run here.
		if !oneAccount && !t.pin.set() {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf(mpsAccounts, p.ID))
			return
		}
	} else if !claudeSignInImageResolves(r.Context(), s.cfg.AgentImages, s.cfg.Runner) {
		writeError(w, http.StatusUnprocessableEntity, mpsNoImage)
		return
	}
	_, actor := actorFromRequest(r)
	run, dispatch, err := s.launchHarnessLoginRun(r.Context(), actor, hl, t)
	if err != nil {
		if errors.Is(err, errRecordCeilingLimit) {
			writeError(w, http.StatusForbidden, strings.TrimPrefix(err.Error(), errRecordCeilingLimit.Error()+": "))
			return
		}
		writeServerError(w, r, "launch login sandbox", err)
		return
	}
	writeJSON(w, http.StatusOK, harnessLoginResponse{RunID: run.ID.String(), State: string(run.State)})
	go s.finishHarnessLoginLaunch(context.WithoutCancel(r.Context()), run, dispatch)
}

type providerSignInCapture struct {
	RunID string `json:"run_id"`
	Token string `json:"token"`
}

// handleProviderSignInCapture stores the caller's own Claude sign-in for one
// provider — the setup-token their sign-in sandbox printed:
//
//	PUT /api/v1/model-providers/{id}/sign-in  {"run_id": "...", "token": "..."}
//
// Bound to that sign-in: run_id must be a Claude login run this caller
// launched through this door for this provider (its harness.login.started
// stamp names both), and not one a newer sign-in replaced. Stored under
// wardyn-provider-<uid>-oauth in the caller's own namespace only; write-only.
func (s *Server) handleProviderSignInCapture(w http.ResponseWriter, r *http.Request) {
	owner := s.credentialOwner(w, r)
	if owner == "" {
		return
	}
	var body providerSignInCapture
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, mpsCaptureBody)
		return
	}
	runID, err := uuid.Parse(body.RunID)
	if err != nil {
		writeError(w, http.StatusBadRequest, mpsCaptureBody)
		return
	}
	token := strings.TrimSpace(body.Token)
	// Under siteConfigMu, like the key door: the UID read here is still the
	// provider's when the write lands, since rule 8's purge holds it too.
	s.siteConfigMu.Lock()
	defer s.siteConfigMu.Unlock()
	sc, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, mpsUnreadable)
		return
	}
	p, hl, _, ok := s.signInProvider(w, r, sc)
	if !ok {
		return
	}
	if hl.captureViaHelper {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf(mpsAWSByHelper, p.ID))
		return
	}
	if previewHidesOwnCredential(r.Context()) {
		writeError(w, http.StatusConflict, mpsPreview)
		return
	}
	if msg := harnessPasteRefusal(hl, token); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if !s.ownProviderSignInRun(w, r, runID, hl, p, owner) {
		return
	}
	name := providerSecretName(p.UID, providerOAuthPart)
	raw, _ := json.Marshal(managedCredBlob{Token: token, CapturedAt: s.cfg.Now().UTC(), SourceRunID: runID.String()})
	if err := s.cfg.Secrets.For(owner).Put(r.Context(), name, raw); err != nil {
		writeServerError(w, r, "store model provider sign-in", err)
		return
	}
	// Masked for the sign-in run's own capture only, not process-wide: the
	// bytes are the caller's choice (the prefix guard is shape, not proof), and
	// the sink masks the token for every run it is injected into.
	s.cfg.MaskRegistry.Add(runID, []byte(token))
	s.recordAudit(r.Context(), s.auditEvent(&runID, actorTypeFromRequest(r), principalFromRequest(r),
		"harness.credential.captured", name, "success", mustJSON(map[string]any{
			"provider": hl.provider, "source": "paste", "owner": owner,
			"credential_source": string(types.CredentialSourcePerUser), "model_provider": p.ID,
		})))
	w.WriteHeader(http.StatusNoContent)
}

// ownProviderSignInRun reports whether runID is a live Claude sign-in this
// caller launched through the provider door for p, while p still has the
// address it had then (rule 8: a sign-in given for one address must not land
// after the purge). Everything it compares is server-written at launch (the
// run row and its harness.login.started stamp). ok=false: the refusal is
// written.
func (s *Server) ownProviderSignInRun(w http.ResponseWriter, r *http.Request, runID uuid.UUID, hl harnessLogin, p types.ModelProvider, owner string) bool {
	run, err := s.cfg.Store.GetRun(r.Context(), runID)
	if err != nil || run.Task != harnessLoginTask || run.Agent != hl.agent {
		writeError(w, http.StatusConflict, fmt.Sprintf(mpsNotYourRun, p.ID))
		return false
	}
	stamp, err := s.loginRunStamp(r.Context(), runID)
	if err != nil {
		writeServerError(w, r, "read sign-in run", err)
		return false
	}
	if stamp.ModelProviderUID != p.UID || stamp.Owner != owner {
		writeError(w, http.StatusConflict, fmt.Sprintf(mpsNotYourRun, p.ID))
		return false
	}
	if stamp.ModelProviderAddress != providerAddressDigest(p) {
		writeError(w, http.StatusConflict, mpsCaptureChanged)
		return false
	}
	if run.State == types.RunKilled {
		writeError(w, http.StatusConflict, ssoTokenRunKilledRefusal)
		return false
	}
	return true
}
