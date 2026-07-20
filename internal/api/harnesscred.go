// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Managed harness credentials — "subscription token as a first-class secret".
//
// A COMPOSE/containerized deployment's distroless wardynd has no host ~/.claude
// to read, so the resident-subscription path (stage-claude-creds.sh + the
// internal/subscription resident provider) is host-mode-only; compose fell back
// to a stale RESIDENT COPY of the token (WARDYN_SUBSCRIPTION_INJECT=off), which
// contradicts the "secrets never resident" invariant and had no re-auth path.
//
// This module lets an operator CONNECT a Claude subscription from anywhere:
// Wardyn launches an interactive login sandbox, the operator runs
// `claude setup-token` in the embedded attach terminal (device-style OAuth,
// remote callback — no localhost dependency), and pastes the printed long-lived
// (~1yr) token into the setup UI. Wardyn stores it once, age-encrypted, under a
// RESERVED name and thereafter injects it PROXY-SIDE into every run exactly like
// the resident subscription token — the sandbox holds only the inert sentinel.
// Refresh is deferred (setup-token is long-lived); expiry is surfaced honestly
// and re-auth is re-running the flow.

const (
	// harnessLoginTask / harnessRefreshTask discriminate a managed-harness run
	// from ordinary runs (precedent: "workspace record" / "workspace verify").
	// They are set SERVER-SIDE, never from client input, and gate the credential
	// upload/seed endpoints.
	harnessLoginTask   = "harness login"
	harnessRefreshTask = "harness refresh" // reserved for the deferred auto-refresh path

	// harnessLoginIdleCap bounds an abandoned login sandbox (self-terminates +
	// revokes rather than living forever).
	harnessLoginIdleCap = 30 * time.Minute
)

// harnessLogin is the per-agent container-login convention. Adding a provider is
// a new row here (house style: one more table entry, not a new interface). v1
// ships Anthropic/claude-code only; codex ChatGPT-login capture is the
// documented v2 seam (needs ~/.codex/auth.json capture + a chatgpt.com sink).
type harnessLogin struct {
	provider    string   // canonical provider id, e.g. "anthropic"
	agent       string   // agent (and thus image) the login sandbox runs
	secretName  string   // reserved store name holding the captured token blob
	sentinel    string   // injection sentinel (types.ManagedOAuthSecret); "" = no injection
	injectHost  string   // the ONLY host the sentinel may inject to
	tokenPrefix string   // accepted setup-token prefix (format guard, not auth); "" = validate structurally
	egress      []string // hosts the interactive login flow must reach
	hint        string   // the command the operator runs in the terminal
	// captureViaHelper: the credential is written to a FILE in the sandbox and
	// uploaded by an in-sandbox helper (wardyn-aws-sso), not printed to the PTY
	// and scraped. Also means the run is safe to record: the terminal only ever
	// shows a short-lived device code + verification URL, never a live secret.
	captureViaHelper bool
}

// agentHarnessLogin returns the login convention for an agent, if it supports
// container login.
func agentHarnessLogin(agent string) (harnessLogin, bool) {
	switch agent {
	case "claude-code":
		return harnessLogin{
			provider:    "anthropic",
			agent:       "claude-code",
			secretName:  harnessCredSecretName("anthropic"),
			sentinel:    types.ManagedOAuthSecret,
			injectHost:  subscriptionInjectionHost, // api.anthropic.com
			tokenPrefix: "sk-ant-oat",
			// `claude setup-token` OAuth (observed v2.1.x): authorize on claude.com,
			// remote callback on platform.claude.com, token exchange on the Anthropic
			// console/api hosts. Enumerated empirically; prune/extend from the login
			// run's decision log (any extra host surfaces as a deny_with_review).
			egress: []string{"claude.com", "platform.claude.com", "console.anthropic.com", "api.anthropic.com"},
			hint:   "claude setup-token",
		}, true
	case awsSSOAgent:
		return harnessLogin{
			provider:   awsSSOProvider,
			agent:      awsSSOAgent,
			secretName: harnessCredSecretName(awsSSOProvider),
			// Phase A delivers the captured token as a minimal synthetic ~/.aws in
			// the sandbox, so there is nothing to inject yet. Phase B fills
			// sentinel/injectHost in to proxy-inject x-amz-sso_bearer_token on
			// portal.sso.<region> (that call is authtype:none, so a MITM can set the
			// header without AWS signing keys) and the token stops being resident.
			sentinel:   "",
			injectHost: "",
			// No AWS analogue to `sk-ant-oat`: the SSO cache is structured JSON, so
			// capture validates its SHAPE instead of a prefix.
			tokenPrefix: "",
			// `aws sso login --no-browser --use-device-code` (RFC 8628): oidc.* does
			// RegisterClient/StartDeviceAuthorization/CreateToken, device.sso.* serves
			// the verification page, portal.sso.* answers GetRoleCredentials +
			// list-accounts/roles, and *.awsapps.com is the org access portal.
			// Region isn't known statically here, hence the wildcards; anything else
			// the flow dials surfaces as a deny_with_review rather than a silent deny.
			egress: []string{
				"oidc.*.amazonaws.com",
				"portal.sso.*.amazonaws.com",
				"device.sso.*.amazonaws.com",
				"*.awsapps.com",
			},
			hint:             "aws sso login --no-browser --use-device-code",
			captureViaHelper: true,
		}, true
	default:
		return harnessLogin{}, false
	}
}

// harnessLoginByProvider finds the login convention by provider id.
func harnessLoginByProvider(provider string) (harnessLogin, bool) {
	// A linear scan over the known rows; stays correct as rows are added.
	for _, agent := range []string{"claude-code", awsSSOAgent} {
		if hl, ok := agentHarnessLogin(agent); ok && hl.provider == provider {
			return hl, true
		}
	}
	return harnessLogin{}, false
}

// managedSentinelAccessToken mirrors the inert placeholder stage-claude-creds.sh
// writes for the resident path: an obviously-not-live token in the sk-ant-oat
// shape so `claude` accepts the field and starts, granting nothing (the proxy
// overrides Authorization on the wire with the live managed token).
const managedSentinelAccessToken = "sk-ant-oat01-wardyn-inert-sentinel-proxy-injects-the-live-token"

// managedSentinelCredsB64 builds the base64 sentinel .credentials.json delivered
// to a managed run in WARDYN_CLAUDE_MANAGED_B64. All fields are inert by
// construction (blank refresh, placeholder access, far-future expiry), so it is
// safe as sandbox env — it carries no secret. Go port of the sentinelization in
// scripts/stage-claude-creds.sh:117-138.
func managedSentinelCredsB64() string {
	creds := map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":      managedSentinelAccessToken,
			"refreshToken":     "",
			"expiresAt":        int64(4102444800000), // 2100-01-01 ms: claude never client-refreshes
			"scopes":           []string{"user:inference"},
			"subscriptionType": "max",
		},
	}
	b, _ := json.Marshal(creds)
	return base64.StdEncoding.EncodeToString(b)
}

// managedInjectReady reports whether a claude-code run with no resident
// subscription mount and no Bedrock should be credentialed by the Wardyn-managed
// token: the provider is wired AND a token blob is actually present. This is the
// dispatch precedence gate (host-staged mount > managed > Bedrock > api-key).
func (s *Server) managedInjectReady(agent string) bool {
	if agent != "claude-code" || s.cfg.ManagedToken == nil {
		return false
	}
	_, err := s.cfg.ManagedToken.Peek()
	return err == nil
}

// harnessCredSecretName is the reserved store name holding a provider's captured
// token blob. Reserved (see reservedSecretNames) so the generic secrets API
// cannot overwrite/delete/list it and the injection sink refuses to resolve it
// as a stored value — it is served ONLY via the managed provider + sentinel.
func harnessCredSecretName(provider string) string {
	return "wardyn-harness-" + provider + "-oauth"
}

// managedCredBlob is the stored shape: the verbatim setup-token plus provenance.
// The token is long-lived; wardynd never parses or refreshes it (single-owner
// discipline — the token's owner is the operator who minted it via the CLI).
type managedCredBlob struct {
	Token       string    `json:"token"`
	CapturedAt  time.Time `json:"captured_at"`
	SourceRunID string    `json:"source_run_id,omitempty"`
}

// ─── AWS IAM Identity Center (SSO) container login ──────────────────────────
// A second container-login provider, for Bedrock. Unlike the Anthropic row it
// captures a STRUCTURED credential written to a file by `aws sso login`, not a
// single opaque token printed to the PTY — so managedCredBlob doesn't fit and
// the capture path is an in-sandbox helper upload (see cmd/wardyn-aws-sso),
// mirroring wardyn-scan/wardyn-verify rather than terminal scraping.
const (
	awsSSOProvider = "aws"     // canonical provider id (secret: wardyn-harness-aws-oauth)
	awsSSOAgent    = "aws-sso" // agent + image the login sandbox runs
)

// awsSSOBlob is the stored AWS SSO credential: the contents of the CLI's cache
// file (~/.aws/sso/cache/<sha1>.json) plus the account/role the derived role
// credentials should be minted for, plus provenance.
//
// Residency note: AccessToken is what a later Bedrock run's SDK exchanges (via
// portal.sso GetRoleCredentials) for SHORT-LIVED role credentials. The role
// credentials are always resident in the sandbox — Bedrock signs SigV4
// in-process, so they can never be proxy-injected (see runs_bedrock.go). What
// Phase B changes is that this AccessToken/RefreshToken pair stops being
// resident too.
type awsSSOBlob struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	// StartURL + Region identify the SSO session; both are required to rebuild a
	// usable ~/.aws/config and to derive the cache filename (sha1 of the session
	// name / start URL).
	StartURL string `json:"start_url"`
	Region   string `json:"region"`
	// AccountID + RoleName are the GetRoleCredentials parameters — captured at
	// login time (aws sso list-accounts / list-account-roles) so later runs need
	// no further interaction.
	AccountID string `json:"account_id,omitempty"`
	RoleName  string `json:"role_name,omitempty"`
	// ExpiresAt is the SSO access token's real, machine-readable expiry. Unlike
	// the Anthropic setup-token (which exposes none, forcing the age heuristic in
	// harnessTokenAging), this lets readiness report TRUE expiry.
	ExpiresAt             time.Time `json:"expires_at"`
	RegistrationExpiresAt time.Time `json:"registration_expires_at,omitempty"`
	CapturedAt            time.Time `json:"captured_at"`
	SourceRunID           string    `json:"source_run_id,omitempty"`
}

// valid reports whether a captured blob is structurally usable. This replaces
// the Anthropic prefix guard (there is no fixed AWS token prefix); like that
// guard it is a SHAPE check, not authentication — real validation happens on
// first use against portal.sso.
func (b awsSSOBlob) valid() bool {
	return b.AccessToken != "" && b.StartURL != "" && b.Region != "" && !b.ExpiresAt.IsZero()
}

// expired reports whether the SSO access token has lapsed. A blob with a refresh
// token can still be renewed (sso-session profiles); one without must be
// re-captured by re-running the login.
func (b awsSSOBlob) expired(now time.Time) bool { return !now.Before(b.ExpiresAt) }

// harnessTokenAging: setup-token tokens live ~1 year and their exact expiry is
// not machine-readable from the token, so readiness warns purely on AGE past
// this threshold (a conservative "likely expiring soon; reconnect").
const harnessTokenAging = 11 * 30 * 24 * time.Hour

// readManagedBlob loads and parses a provider's captured token blob.
// Only secretstore.ErrNotFound means "not connected" (found=false, nil). Any
// other store error is a genuine failure (age-key mismatch after rotation, PG
// down, …) — it is logged and PROPAGATED so a caller never mistakes a wedged
// store for "no credential connected" (which would flip setup status to a false
// "LLM access not configured" for an operator who IS connected). A parse error
// is likewise a real error.
func (s *Server) readManagedBlob(ctx context.Context, provider string) (managedCredBlob, bool, error) {
	if s.cfg.Secrets == nil {
		return managedCredBlob{}, false, nil
	}
	raw, err := s.cfg.Secrets.Get(ctx, harnessCredSecretName(provider))
	if errors.Is(err, secretstore.ErrNotFound) {
		return managedCredBlob{}, false, nil // absent == not connected (not an error)
	}
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: read managed credential from secret store failed",
			slog.String("provider", provider), slog.Any("err", err))
		return managedCredBlob{}, false, fmt.Errorf("read managed credential: %w", err)
	}
	var blob managedCredBlob
	if uerr := json.Unmarshal(raw, &blob); uerr != nil {
		return managedCredBlob{}, false, fmt.Errorf("parse managed credential blob: %w", uerr)
	}
	if strings.TrimSpace(blob.Token) == "" {
		return managedCredBlob{}, false, nil
	}
	return blob, true, nil
}

// readAWSSSOBlob loads the captured AWS SSO credential. Same error discipline as
// readManagedBlob: absent means "not connected", not a failure. A structurally
// invalid blob is treated as absent so a half-written capture can never be
// mistaken for a usable credential.
func (s *Server) readAWSSSOBlob(ctx context.Context) (awsSSOBlob, bool, error) {
	if s.cfg.Secrets == nil {
		return awsSSOBlob{}, false, nil
	}
	raw, err := s.cfg.Secrets.Get(ctx, harnessCredSecretName(awsSSOProvider))
	if errors.Is(err, secretstore.ErrNotFound) {
		return awsSSOBlob{}, false, nil
	}
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: read aws sso credential from secret store failed",
			slog.Any("err", err))
		return awsSSOBlob{}, false, fmt.Errorf("read aws sso credential: %w", err)
	}
	var blob awsSSOBlob
	if uerr := json.Unmarshal(raw, &blob); uerr != nil {
		return awsSSOBlob{}, false, fmt.Errorf("parse aws sso credential blob: %w", uerr)
	}
	if !blob.valid() {
		return awsSSOBlob{}, false, nil
	}
	return blob, true, nil
}

// storeAWSSSOBlob persists a captured AWS SSO credential under the reserved
// harness secret name. Callers must have validated the blob first.
func (s *Server) storeAWSSSOBlob(ctx context.Context, blob awsSSOBlob) error {
	if s.cfg.Secrets == nil {
		return fmt.Errorf("no secret store configured")
	}
	raw, err := json.Marshal(blob)
	if err != nil {
		return fmt.Errorf("marshal aws sso credential blob: %w", err)
	}
	return s.cfg.Secrets.Put(ctx, harnessCredSecretName(awsSSOProvider), raw)
}

// ── Login run launch ─────────────────────────────────────────────────────────

// launchHarnessLoginRun brings up an INTERACTIVE claude-code sandbox scoped to
// exactly the OAuth hosts the login flow needs, so the operator can run
// `claude setup-token` in the attach terminal. It mints nothing and mounts no
// credential — it is a blank, egress-pinned box whose only purpose is to host
// the interactive OAuth. Modeled on launchRecordRun, minus workspace/claim.
//
// RECORDING GATE (harnessLoginTask is never recorded): this run's terminal exists
// to PRINT a ~1yr credential, and because the run mints nothing its mask snapshot
// is empty by construction — liveMaskWriter is a pass-through, and the paste-time
// AddGlobal in handleHarnessCredentialPaste lands too late for the cast (masking
// is write-time). So no masking can protect this session. That gap is CLOSED:
// newSessionRecorder (attach.go) drops the recorder entirely for a run where
// runIsUnrecordable(run) is true (run.Task == harnessLoginTask), so no replayable
// asciicast is ever persisted. The gate lives at that single call site so a future
// second attach path cannot miss it. (harness.login.started and session.attach
// still record who attached, when, and why — no provenance is lost.)
func (s *Server) launchHarnessLoginRun(ctx context.Context, actor string, hl harnessLogin) (types.AgentRun, error) {
	if s.cfg.Runner == nil {
		return types.AgentRun{}, fmt.Errorf("no runner configured")
	}
	caps, cerr := s.cfg.Runner.Capabilities(ctx)
	if cerr != nil {
		return types.AgentRun{}, fmt.Errorf("runner capabilities unavailable: %w", cerr)
	}
	cc := bestClass(caps.ConfinementClasses)
	if cc == "" {
		return types.AgentRun{}, fmt.Errorf("runner declares no confinement class")
	}

	runID := uuid.New()
	id, err := s.cfg.Identity.MintRunIdentity(ctx, runID, actor, actor, internalAudience)
	if err != nil {
		return types.AgentRun{}, fmt.Errorf("mint run identity: %w", err)
	}
	now := s.cfg.Now().UTC()
	run := types.AgentRun{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: actor,
		Agent: hl.agent, Task: harnessLoginTask,
		ConfinementClass: cc, State: types.RunPending, SPIFFEID: id.SPIFFEID,
		RunnerTarget: s.cfg.RunnerTarget,
		Interactive:  true,
	}
	policy := types.RunPolicySpec{
		MinConfinementClass: cc,
		// Default-deny, limited to the OAuth hosts. An off-policy host the login
		// flow dials ESCALATES to the operator (visible in the login pane) rather
		// than a silent hard-deny, so the empirical egress list can be tightened.
		AllowAllEgress:   false,
		AllowedDomains:   append([]string(nil), hl.egress...),
		FirstUseApproval: types.FirstUseDenyWithReview,
		AutoStopAfterSec: int(harnessLoginIdleCap.Seconds()),
	}
	run.AutoStopAfterSec = policy.AutoStopAfterSec // reaper reads the run row
	created, err := s.cfg.Store.CreateRun(ctx, run)
	if err != nil {
		return types.AgentRun{}, fmt.Errorf("create harness login run: %w", err)
	}
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "harness.login.started",
		runID.String(), "success", mustJSON(map[string]any{
			"provider": hl.provider, "egress": hl.egress,
		})))

	image := agentImage(hl.agent, s.cfg.AgentImages)
	// No injections, no repo, no verify plan: a blank interactive box. The `--idle`
	// path installs the MITM CA (unused here) and attaches; the human runs
	// `claude setup-token` and pastes the result into the UI.
	s.dispatchWithVerify(ctx, created, id.Token, image, policy, nil, nil, nil, nil, true, "", nil)
	return s.refreshRun(ctx, runID, created), nil
}

// ── HTTP: setup/harness-* (humanOrAdmin group) ───────────────────────────────

type harnessLoginRequest struct {
	Provider string `json:"provider"`
}
type harnessLoginResponse struct {
	RunID string `json:"run_id"`
}

// handleHarnessLogin launches a container-login sandbox for a provider:
//
//	POST /api/v1/setup/harness-login  {provider}
func (s *Server) handleHarnessLogin(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Secrets == nil {
		writeError(w, http.StatusServiceUnavailable, "no secret store configured; managed harness login unavailable")
		return
	}
	var req harnessLoginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	provider := strings.TrimSpace(req.Provider)
	if provider == "" {
		provider = "anthropic"
	}
	hl, ok := harnessLoginByProvider(provider)
	if !ok {
		writeError(w, http.StatusBadRequest, "provider does not support container login in this version: "+provider)
		return
	}
	_, actor := actorFromRequest(r)
	run, err := s.launchHarnessLoginRun(r.Context(), actor, hl)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "launch login sandbox: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, harnessLoginResponse{RunID: run.ID.String()})
}

type harnessCredRequest struct {
	Token string `json:"token"`
}

// handleHarnessCredentialPaste stores an operator-pasted setup-token:
//
//	PUT /api/v1/setup/harness-credential/{provider}  {token}
//
// Auth is the normal humanOrAdmin group (NOT a sandbox route — there is no
// brokered path to it): the operator pastes into the UI, which posts here. The
// value is write-only (no API ever returns it) and masked from streams.
func (s *Server) handleHarnessCredentialPaste(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Secrets == nil {
		writeError(w, http.StatusServiceUnavailable, "no secret store configured")
		return
	}
	provider := strings.TrimSpace(chi.URLParam(r, "provider"))
	hl, ok := harnessLoginByProvider(provider)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider: "+provider)
		return
	}
	var req harnessCredRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	token := strings.TrimSpace(req.Token)
	// Format guard (NOT authentication — the token is validated for real on first
	// use, when the proxy injects it and Anthropic accepts or rejects it). Reject
	// an obvious paste error early with an actionable message.
	// tokenPrefix == "" means the provider has no fixed prefix to guard on (AWS
	// SSO): its credential arrives structurally validated via the helper-upload
	// path instead, so the paste endpoint is not the capture route for it.
	if hl.tokenPrefix != "" && !strings.HasPrefix(token, hl.tokenPrefix) {
		writeError(w, http.StatusBadRequest,
			"that does not look like a `claude setup-token` output (expected a token starting with "+hl.tokenPrefix+")")
		return
	}
	blob := managedCredBlob{Token: token, CapturedAt: s.cfg.Now().UTC()}
	raw, _ := json.Marshal(blob)
	if err := s.cfg.Secrets.Put(r.Context(), hl.secretName, raw); err != nil {
		writeError(w, http.StatusInternalServerError, "store managed credential: "+err.Error())
		return
	}
	// Register the token PROCESS-GLOBALLY so it is masked out of every run's PTY
	// capture, asciicast and decision log — not just the runs it is injected into.
	// A per-run Add cannot cover it: the value is minted outside any run's mint
	// path, so nothing else ever tells the registry it exists.
	//
	// HONEST RESIDUAL: masking is write-time, never retroactive. The login run's
	// OWN asciicast has already buffered the `claude setup-token` output verbatim
	// by the time this handler runs, so this does not redact that cast — see
	// launchHarnessLoginRun for why the login terminal must not be recorded at all.
	s.cfg.MaskRegistry.AddGlobal([]byte(token)) // nil-safe

	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"harness.credential.captured", hl.secretName, "success",
		mustJSON(map[string]any{"provider": hl.provider, "source": "paste"})))
	writeJSON(w, http.StatusOK, map[string]any{"provider": hl.provider, "captured": true})
}

// handleHarnessDisconnect deletes a stored managed credential:
//
//	DELETE /api/v1/setup/harness-credential/{provider}
func (s *Server) handleHarnessDisconnect(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Secrets == nil {
		writeError(w, http.StatusServiceUnavailable, "no secret store configured")
		return
	}
	provider := strings.TrimSpace(chi.URLParam(r, "provider"))
	hl, ok := harnessLoginByProvider(provider)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider: "+provider)
		return
	}
	if err := s.cfg.Secrets.Delete(r.Context(), hl.secretName); err != nil {
		writeError(w, http.StatusInternalServerError, "delete managed credential: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"harness.credential.disconnected", hl.secretName, "success",
		mustJSON(map[string]any{"provider": hl.provider})))
	writeJSON(w, http.StatusOK, map[string]any{"provider": hl.provider, "captured": false})
}

// ── Managed provider (subscription.Provider over the stored blob) ─────────────

// managedCredProvider serves the Wardyn-managed captured token through the SAME
// subscription.Provider interface the resident host token uses, so the injection
// sink treats them identically. It depends ONLY on the secret store (not the
// Server), so it can be constructed in main.go BEFORE api.New builds the Server
// — no construction cycle.
//
// No refresh path (v1): setup-token tokens are long-lived and Wardyn is not
// their owner, so Current never mutates state — it returns the stored token and
// lets Anthropic reject it on the wire if it has been revoked (fail closed at
// the sink, surfaced as a run failure + an aging warning in setup status).
type managedCredProvider struct {
	store    secretstore.Store
	provider string
}

// NewManagedCredProvider builds a managed subscription provider over store for a
// provider id (e.g. "anthropic"). Returns nil when store is nil (managed mode
// simply unavailable).
func NewManagedCredProvider(store secretstore.Store, provider string) subscription.Provider {
	if store == nil {
		return nil
	}
	return &managedCredProvider{store: store, provider: provider}
}

func (p *managedCredProvider) read() (subscription.Token, error) {
	raw, err := p.store.Get(context.Background(), harnessCredSecretName(p.provider))
	if errors.Is(err, secretstore.ErrNotFound) {
		return subscription.Token{}, fmt.Errorf("no managed %s credential connected", p.provider)
	}
	if err != nil {
		// A store-layer failure (decrypt/age-key mismatch, backend down) is NOT
		// "not connected" — surface it distinctly so the sink fails closed on a
		// real error rather than silently reading as "unconfigured".
		return subscription.Token{}, fmt.Errorf("read managed %s credential: %w", p.provider, err)
	}
	var blob managedCredBlob
	if uerr := json.Unmarshal(raw, &blob); uerr != nil {
		return subscription.Token{}, fmt.Errorf("parse managed credential: %w", uerr)
	}
	if strings.TrimSpace(blob.Token) == "" {
		return subscription.Token{}, fmt.Errorf("managed %s credential is empty; reconnect via container login", p.provider)
	}
	// ExpiresAt zero = "no machine-readable expiry" — the sink omits expires_at so
	// the proxy treats the token as static (setup-token is long-lived; a revoked
	// one fails on the wire, not on a clock).
	return subscription.Token{Value: blob.Token}, nil
}

// Current returns the managed token (no refresh — see type doc).
func (p *managedCredProvider) Current(ctx context.Context) (subscription.Token, error) {
	return p.read()
}

// Peek is identical to Current here (no refresh side effect to avoid).
func (p *managedCredProvider) Peek() (subscription.Token, error) {
	return p.read()
}
