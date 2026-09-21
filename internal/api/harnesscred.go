// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
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
	// harnessLoginTask discriminates a managed-harness run from ordinary runs
	// (precedent: "workspace record" / "workspace verify"). It is set
	// SERVER-SIDE, never from client input, and gates the credential
	// upload/seed endpoints.
	harnessLoginTask = "harness login"

	// harnessLoginIdleCap bounds an abandoned login sandbox (self-terminates +
	// revokes rather than living forever).
	harnessLoginIdleCap = 30 * time.Minute
)

// harnessLogin is the per-agent container-login convention. Adding a provider is
// a new row here (house style: one more table entry, not a new interface). v1
// ships Anthropic/claude-code only; codex ChatGPT-login capture is the
// documented v2 seam (needs ~/.codex/auth.json capture + a chatgpt.com sink).
type harnessLogin struct {
	provider string // canonical provider id, e.g. "anthropic"
	agent    string // agent (and thus image) the login sandbox runs
	// loginImageKey overrides the catalog ImageKey for THIS lane's ghcr
	// fallback. claude-code's catalog ImageKey is "base" so that `--agent
	// claude-code` resolves to an image that is actually published — but a
	// login sandbox must carry the VENDOR CLI it is logging into, and
	// agent-base ships none. Empty = follow the catalog.
	loginImageKey string
	secretName    string   // reserved store name holding the captured token blob
	sentinel      string   // injection sentinel (types.ManagedOAuthSecret); "" = no injection
	injectHost    string   // the ONLY host the sentinel may inject to
	tokenPrefix   string   // accepted setup-token prefix (format guard, not auth); "" = validate structurally
	egress        []string // region-free hosts the interactive login flow must reach
	// regionalSSOEgress: the flow also dials region-scoped AWS SSO endpoints,
	// which no static allowlist entry can express (see loginEgress) — they are
	// derived from the operator's configured SSO region at launch.
	regionalSSOEgress bool
	// captureViaHelper: the credential is written to a FILE in the sandbox and
	// uploaded by an in-sandbox helper (wardyn-aws-sso), not printed to the PTY
	// and scraped. Also means the run is safe to record: the terminal only ever
	// shows a short-lived device code + verification URL, never a live secret.
	captureViaHelper bool
}

// agentHarnessLogin returns the login convention for an agent, if it supports
// container login. claude-code (and any future catalog row that gains a Login
// convention) resolves through the harness catalog (harness.go); aws-sso is a
// login-only auxiliary provider that names no coding-agent harness, so it
// stays a direct case here exactly as before the catalog existed.
func agentHarnessLogin(agent string) (harnessLogin, bool) {
	switch agent {
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
			// `aws sso login --no-browser --use-device-code` (RFC 8628): oidc.<r> does
			// RegisterClient/StartDeviceAuthorization/CreateToken, device.sso.<r>
			// serves the verification page, portal.sso.<r> answers
			// list-accounts/list-account-roles (wardyn-aws-sso resolves account+role
			// at capture time), and *.awsapps.com is the org access portal.
			// The first three are REGION-SCOPED and the region is not knowable in
			// this static table, so they are derived at launch from the operator's
			// configured SSO region — see loginEgress for why a wildcard cannot
			// express them. Only the region-free portal host is listed here;
			// anything else the flow dials surfaces as a deny_with_review rather
			// than a silent deny.
			egress:            []string{"*.awsapps.com"},
			regionalSSOEgress: true,
			captureViaHelper:  true,
		}, true
	default:
		def, ok := harnessByID(agent)
		if !ok || def.Login == nil {
			return harnessLogin{}, false
		}
		return *def.Login, true
	}
}

// loginConfigEnv is the sandbox env that seeds the login box's NON-SECRET
// configuration — for AWS, the pre-login ~/.aws/config the auto-typed
// `aws sso login --sso-session wardyn` reads. Delivered through the SAME
// WARDYN_AWS_SSO_CONFIG_B64 channel a Bedrock run uses (materialize_aws_sso_config
// in deploy/images/common/agent-run-lib.sh), so there is one materializer, not two.
// nil for every other provider, and nil when either half is unknown (a
// half-written sso-session block is worse than none: the operator can still run
// `aws configure sso` by hand).
func (hl harnessLogin) loginConfigEnv(ssoStartURL, ssoRegion string) map[string]string {
	if !hl.regionalSSOEgress || ssoStartURL == "" || ssoRegion == "" {
		return nil
	}
	return map[string]string{
		awsSSOConfigEnvVar: encodeArtifactConfig(map[string]string{
			".aws/config": awsSSOLoginConfigFileContents(ssoStartURL, ssoRegion),
		}),
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

// AWS IAM Identity Center (SSO) container login
// A second container-login provider, for Bedrock. Unlike the Anthropic row it
// captures a STRUCTURED credential written to a file by `aws sso login`, not a
// single opaque token printed to the PTY — so managedCredBlob doesn't fit and
// the capture path is an in-sandbox helper upload (see cmd/wardyn-aws-sso),
// mirroring wardyn-scan rather than terminal scraping.
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
//
// AccountID/RoleName are required, not merely nice-to-have: resolveBedrockAuth
// selects this credential (ssoInject) the instant a blob is stored, ahead of
// the host-mode ~/.aws mount and static-key lanes, and awsSSOConfigFileContents
// bakes account_id/role_name VERBATIM into the generated ~/.aws/config INI. A
// blob missing either (wardyn-aws-sso's best-effort `aws sso list-accounts` /
// list-account-roles resolution came up empty — no accounts, a timeout, a
// malformed response) can never satisfy GetRoleCredentials, so accepting it as
// "connected" would silently pre-empt a lane that might have actually worked.
// Rejecting it here (before storage) means the operator sees the upload fail
// and can re-run the login, rather than a stored-but-useless credential
// quietly winning every later Bedrock run.
func (b awsSSOBlob) valid() bool { return len(b.missingFields()) == 0 }

// expired reports whether the SSO access token has lapsed. A blob with a refresh
// token can still be renewed (sso-session profiles); one without must be
// re-captured by re-running the login.
func (b awsSSOBlob) expired(now time.Time) bool { return !now.Before(b.ExpiresAt) }

// harnessTokenAging: setup-token tokens live ~1 year and their exact expiry is
// not machine-readable from the token, so readiness warns purely on AGE past
// this threshold (a conservative "likely expiring soon; reconnect").
const harnessTokenAging = 11 * 30 * 24 * time.Hour

// readHarnessBlob loads and parses a provider's reserved harness credential
// blob under the ONE error discipline both harness lanes need.
//
// Only secretstore.ErrNotFound means "not connected" (found=false, nil). Any
// other store error is a genuine failure (age-key mismatch after rotation, PG
// down, …) — it is logged and PROPAGATED so a caller never mistakes a wedged
// store for "no credential connected" (which would flip setup status to a false
// "LLM access not configured" for an operator who IS connected). A parse error
// is likewise a real error.
//
// usable is the lane's shape check: a structurally incomplete blob reads as
// ABSENT, never as an error, so a half-written capture can never be mistaken
// for a usable credential. label names the credential in the log line and in
// both error strings. st is always the operator-wide store (harness names are
// RESERVED — secrets.go — never per-principal), so no caller scopes it by owner.
func readHarnessBlob[T any](ctx context.Context, st secretstore.Store, provider, label string, usable func(T) bool) (T, bool, error) {
	var zero T
	if st == nil {
		return zero, false, nil
	}
	raw, err := st.Get(ctx, harnessCredSecretName(provider))
	if errors.Is(err, secretstore.ErrNotFound) {
		return zero, false, nil // absent == not connected (not an error)
	}
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: read "+label+" from secret store failed",
			slog.String("provider", provider), slog.Any("err", err))
		return zero, false, fmt.Errorf("read %s: %w", label, err)
	}
	var blob T
	if uerr := json.Unmarshal(raw, &blob); uerr != nil {
		return zero, false, fmt.Errorf("parse %s blob: %w", label, uerr)
	}
	if !usable(blob) {
		return zero, false, nil
	}
	return blob, true, nil
}

// readManagedBlob loads a provider's captured setup-token blob; usable = a
// non-blank token.
func (s *Server) readManagedBlob(ctx context.Context, provider string) (managedCredBlob, bool, error) {
	return readHarnessBlob(ctx, s.cfg.Secrets, provider, "managed credential",
		func(b managedCredBlob) bool { return strings.TrimSpace(b.Token) != "" })
}

// readAWSSSOBlob loads the captured AWS SSO credential from the namespace scope
// names; usable = the full shape GetRoleCredentials needs (awsSSOBlob.valid).
//
// The ZERO scope is the operator namespace — byte-for-byte the read this was
// before per_user existed, and what a `shared` row (or no roster) resolves to.
//
// A PER-USER scope reads that principal's OWN row and never the operator's, and
// the List-then-Get shape is the whole reason this is not a one-liner:
// Store.For(owner).Get FALLS BACK to the operator's row by contract
// (internal/secretstore/pg), so the obvious For(owner).Get would serve the
// ADMIN'S session to a member who has captured nothing — the exact substitution
// per_user exists to refuse. For("").List() is never consulted, so the owner's
// own rows are all this can see. A per-user scope with NO owner — or one read
// inside the no-credential member preview — is ABSENT (previewHidesOwnCredential).
func (s *Server) readAWSSSOBlob(ctx context.Context, scope awsSSOScope) (awsSSOBlob, bool, error) {
	st := s.cfg.Secrets
	if scope.perUser {
		if st == nil || !scope.namespaced() || previewHidesOwnCredential(ctx) {
			return awsSSOBlob{}, false, nil
		}
		st = st.For(scope.owner)
		own, lerr := st.List(ctx)
		if lerr != nil {
			return awsSSOBlob{}, false, fmt.Errorf("list own aws sso credential: %w", lerr)
		}
		if !slices.Contains(own, harnessCredSecretName(awsSSOProvider)) {
			return awsSSOBlob{}, false, nil
		}
	}
	return readHarnessBlob(ctx, st, awsSSOProvider, "aws sso credential", awsSSOBlob.valid)
}

// storeAWSSSOBlob persists a captured AWS SSO credential under the reserved
// harness secret name, in the namespace scope names. Callers must have validated
// the blob first.
//
// Put never falls back (it is scoped to the view's own row by contract), so the
// per-user arm needs no List dance — but it DOES need a non-empty owner, or the
// write would land in the operator namespace and hand every member's runs one
// person's session.
func (s *Server) storeAWSSSOBlob(ctx context.Context, scope awsSSOScope, blob awsSSOBlob) error {
	if s.cfg.Secrets == nil {
		return fmt.Errorf("no secret store configured")
	}
	st := s.cfg.Secrets
	if scope.perUser {
		if !scope.namespaced() {
			return fmt.Errorf("a per-user aws sso credential has no owner to store it under")
		}
		st = st.For(scope.owner)
	}
	raw, err := json.Marshal(blob)
	if err != nil {
		return fmt.Errorf("marshal aws sso credential blob: %w", err)
	}
	return st.Put(ctx, harnessCredSecretName(awsSSOProvider), raw)
}

// Login run launch

// launchHarnessLoginRun brings up an INTERACTIVE claude-code sandbox scoped to
// exactly the OAuth hosts the login flow needs, so the operator can run
// `claude setup-token` in the attach terminal. It mints nothing and mounts no
// credential — it is a blank, egress-pinned box whose only purpose is to host
// the interactive OAuth. Modeled on launchRecordRun, minus workspace/claim.
//
// It stops at the audit stamp. Everything up to and including CreateRun +
// harness.login.started is what the CALLER must have before it answers — the
// run id, and the launch-time scope/pin the capture upload binds to. The rest
// of the launch (the dispatch ceiling, dispatchRun's blocking CreateSandbox) is
// finishHarnessLoginLaunch's, on a detached context, in harnesscred_launch.go.
//
// Recording gate (harnessLoginTask is never recorded): this run's terminal exists
// to PRINT a ~1yr credential, and because the run mints nothing its mask snapshot
// is empty by construction — liveMaskWriter is a pass-through, and the paste-time
// AddGlobal in handleHarnessCredentialPaste lands too late for the cast (masking
// is write-time). So no masking can protect this session. That gap is CLOSED:
// newSessionRecorder (attach.go) drops the recorder entirely for a run where
// runIsUnrecordable(run) is true (run.Task == harnessLoginTask), so no replayable
// asciicast is ever persisted. The gate lives at that single call site so a future
// second attach path cannot miss it. (harness.login.started and session.attach
// still record who attached, when, and why — no provenance is lost.)
// ssoStartURL (AWS only, "" for every other provider) is the operator's IAM
// Identity Center access-portal URL. It is seeded into the sandbox as a
// pre-login ~/.aws/config so the auto-typed `aws sso login --sso-session wardyn`
// has an sso_start_url/sso_region to read — see awsSSOLoginConfigFileContents.
// There is no server-side start-URL config to read it from (deliberately: it is
// per-organization and this is the only flow that needs it), so it arrives with
// the login request and is validated by the caller.
func (s *Server) launchHarnessLoginRun(ctx context.Context, actor string, hl harnessLogin, ssoStartURL string, pin awsSSOPin, scope awsSSOScope) (types.AgentRun, harnessLoginDispatch, error) {
	if s.cfg.Runner == nil {
		return types.AgentRun{}, harnessLoginDispatch{}, fmt.Errorf("no runner configured")
	}
	caps, cerr := s.cfg.Runner.Capabilities(ctx)
	if cerr != nil {
		return types.AgentRun{}, harnessLoginDispatch{}, fmt.Errorf("runner capabilities unavailable: %w", cerr)
	}
	if len(caps.ConfinementClasses) == 0 {
		return types.AgentRun{}, harnessLoginDispatch{}, fmt.Errorf("runner declares no confinement class")
	}

	runID := uuid.New()
	// The acting principal's ceiling binds, with one exemption: the SHARED
	// subscription every run inherits is the deployment's credential, not a
	// principal's work, so it is not limited. Under a per_user roster row a
	// MEMBER launches it to capture THEIR OWN credential, which is their own
	// work, so the limits are read; for an operator effectiveCeiling
	// short-circuits with no profile and applies no limit.
	ceiling, cerr := s.effectiveCeiling(ctx)
	if cerr != nil {
		return types.AgentRun{}, harnessLoginDispatch{}, cerr
	}
	// Strongest advertised at or above the floor: a ~1yr credential deserves the
	// best available isolation, so the capture must never run weaker than the
	// admin floor demands. ceilingFloorClass is the same admin floor
	// source_scan.go's scan lane uses.
	cc := strongestAdvertisedAtOrAbove(caps.ConfinementClasses, ceilingFloorClass(ceiling))
	// Refuse before superseding, not after. strongestAdvertisedAtOrAbove falls
	// back to the floor itself when nothing advertised meets it, so a CC1-only
	// host under an admin floor of CC2 lands here holding a class this runner
	// cannot enforce. The dispatch would fail anyway — deep in the driver, as a
	// raw runtime error on a created run — but supersedeCallerLoginRuns is five
	// lines below, so failing late would first kill the person's existing
	// sign-in sandbox and then refuse to give them a new one. The same sentence
	// resolveEnforcedConfinement answers a run request with, before anything is
	// destroyed.
	if !slices.Contains(caps.ConfinementClasses, cc) {
		return types.AgentRun{}, harnessLoginDispatch{}, fmt.Errorf(
			"runner %q cannot enforce confinement_class %s (available: %s)",
			s.cfg.Runner.Name(), cc, classesOrNone(caps.ConfinementClasses))
	}
	// Serialized per person, across replicas, for the whole span below: the
	// supersede pass, the insert, and the SECOND pass after it are independent
	// statements, and two launches interleaving through them leave two live
	// sandboxes each holding a captured AWS SSO session. lockLoginSupersede
	// carries the interleaving and why the lock fails open; released on every
	// path, including the refusals and the error returns between here and the
	// second pass.
	releaseLoginLock := s.lockLoginSupersede(ctx, actor)
	defer releaseLoginLock()
	// One live sign-in sandbox per person, and it happens HERE — before
	// newStepRun, where the concurrency quota is counted — so a member capped at
	// one run is never refused by their own abandoned sign-in. See
	// supersedeCallerLoginRuns (harnesscred_supersede.go) for why the old run has
	// to end server-side at all.
	s.supersedeCallerLoginRuns(ctx, actor, hl.agent, runID)
	run, token, err := s.newStepRun(ctx, runID, actor, harnessLoginTask, cc, harnessLoginGovernance(ceiling), func(run *types.AgentRun) {
		run.Agent = hl.agent // the vendor CLI being logged into, never the catalog default
		run.Interactive = true
	})
	if err != nil {
		return types.AgentRun{}, harnessLoginDispatch{}, err
	}
	// Region-scoped SSO endpoints are resolved from the operator's boot config —
	// the SSO region if set, else the Bedrock region (same precedence
	// resolveBedrockAuth uses). Empty means "not configured": the flow's regional
	// hosts are then left to first-use approval rather than pre-allowed wide.
	ssoRegion := cmp.Or(s.cfg.BedrockAWSSSORegion, s.cfg.BedrockRegion)
	egress := hl.loginEgress(ssoRegion, s.cfg.AWSSSOEndpointOverride)
	policy := types.RunPolicySpec{
		MinConfinementClass: cc,
		// Default-deny, limited to the OAuth hosts. An off-policy host the login
		// flow dials ESCALATES to the operator (visible in the login pane) rather
		// than a silent hard-deny, so the empirical egress list can be tightened.
		AllowAllEgress:   false,
		AllowedDomains:   egress,
		FirstUseApproval: types.FirstUseDenyWithReview,
		AutoStopAfterSec: int(harnessLoginIdleCap.Seconds()),
	}
	run.AutoStopAfterSec = policy.AutoStopAfterSec // reaper reads the run row
	created, err := s.cfg.Store.CreateRun(ctx, run)
	if err != nil {
		s.cfg.Identity.RevokeRun(ctx, runID) //nolint:errcheck // best-effort cleanup of the minted-but-unused token
		return types.AgentRun{}, harnessLoginDispatch{}, fmt.Errorf("create harness login run: %w", err)
	}
	// And again, now that this run's row EXISTS. The pass above cannot see a
	// sibling launch whose row is not written yet, so two in flight for one
	// person each read the other as absent and BOTH survive. This one ends the
	// caller's login runs that come before this one in a deterministic total
	// order, which needs no lock and holds across replicas — see
	// supersedeOlderLoginRuns for why that makes two survivors UNLIKELY rather
	// than impossible (created_at is stamped before the insert, so with replica
	// clock skew the timestamp order and the insert order can disagree), and why
	// it never leaves zero.
	s.supersedeOlderLoginRuns(ctx, actor, hl.agent, created)
	// Pre-login ~/.aws/config for the AWS flow, delivered through the SAME
	// WARDYN_AWS_SSO_CONFIG_B64 channel a Bedrock run uses (materialized by
	// materialize_aws_sso_config in agent-run-lib.sh). Non-secret: an sso-session
	// block with the start URL + region and NO token cache — the login sandbox
	// must not receive a credential, it exists to produce one.
	extraEnv := hl.loginEnv(ssoStartURL, ssoRegion, pin, s.cfg.AWSSSOEndpointOverride)

	// The launch-time credential scope, stamped — the one authorizeHarnessLogin's
	// roster read PROVED, passed in, never re-resolved. Re-resolving it at upload
	// time would let a roster edit mid-run re-point a member's PUT at the
	// OPERATOR-WIDE credential; re-resolving it HERE, through the fail-open
	// resolver, would let a mere store blip stamp the same `shared`/"" pair the
	// stamp exists to prevent.
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "harness.login.started",
		runID.String(), "success", mustJSON(map[string]any{
			"provider": hl.provider, "egress": egress,
			"sso_start_url": ssoStartURL, // operator config, not a credential
			// WHOSE credential this run may capture, decided HERE and read back by
			// handleUploadSSOToken — never recomputed there.
			"credential_source": awsSSOCredentialSourceLabel(scope),
			"owner":             scope.owner,
			// The pin as it read at launch — what the upload binds to.
			"sso_account_id": pin.AccountID,
			"sso_role_name":  pin.RoleName,
		})))

	image := agentImage(hl.agent, s.cfg.AgentImages)
	if hl.loginImageKey != "" {
		image = agentImageForKey(hl.agent, hl.loginImageKey, s.cfg.AgentImages)
	}
	// No injections, no repo, no verify plan: a blank interactive box (plus, for
	// AWS, the non-secret ~/.aws/config above). The `--idle` path installs the MITM
	// CA; the aws-sso image then runs the chained login itself in its tmux session.
	return created, harnessLoginDispatch{
		RunToken: token, Image: image, Policy: policy, ExtraEnv: extraEnv,
	}, nil
}

// maxLoginStartAuditScan bounds the read-back below. harness.login.started is
// written by launchHarnessLoginRun immediately after CreateRun, so it is among
// the FIRST events a login run ever has and QueryAuditEvents returns a run's
// events chronologically (store.QueryAuditEventsPage) — a small window always
// contains it. It is a bound on a hostile/wedged read, not a sizing.
const maxLoginStartAuditScan = 100

// loginRunStamp is the launch-time state launchHarnessLoginRun wrote onto THIS
// login run's harness.login.started audit row (ActorSystem/"wardynd" —
// server-set at launch, never sandbox input): the operator-declared AWS
// access-portal URL, the credential scope this run may capture under, and the
// account/role pin. The audit log is the system of record (Invariant 6), and
// the same read-back-your-own-run's-trail shape already serves execSucceeded.
//
// This exists so handleUploadSSOToken can bind WHAT a login sandbox uploads to
// what the operator asked for at launch, without a new run column or a new
// trust source. A missing/blank SSOStartURL is NOT an error: the caller
// compares it to the uploaded value, so "no operator declaration on record"
// fails the comparison and the upload is refused — fail-closed by construction.
//
// A zero CredentialSource means "no stamp on record" — a login run launched
// before this field existed. The caller decides what that is worth; it is never
// silently read as `shared`, because `shared` IS the operator namespace.
type loginRunStamp struct {
	SSOStartURL      string `json:"sso_start_url"`
	CredentialSource string `json:"credential_source"`
	Owner            string `json:"owner"`
	// SSOAccountID/SSORoleName are the roster row's pin as it read at launch:
	// a roster edit while a login sandbox is alive must not re-point a capture
	// in flight. Empty means "launched unpinned", which the upload accepts.
	SSOAccountID string `json:"sso_account_id,omitempty"`
	SSORoleName  string `json:"sso_role_name,omitempty"`
}

func (s *Server) loginRunStamp(ctx context.Context, runID uuid.UUID) (loginRunStamp, error) {
	var out loginRunStamp
	if s.cfg.Store == nil {
		return out, fmt.Errorf("no store configured")
	}
	events, err := s.cfg.Store.QueryAuditEvents(ctx, runID, maxLoginStartAuditScan)
	if err != nil {
		return out, fmt.Errorf("read login run audit trail: %w", err)
	}
	for _, ev := range events {
		if ev.Action != "harness.login.started" || ev.Outcome != "success" {
			continue
		}
		var data loginRunStamp
		if uerr := json.Unmarshal(ev.Data, &data); uerr != nil {
			continue
		}
		return data, nil
	}
	return out, nil
}

// HTTP: setup/harness-* (humanOrAdmin group)

type harnessLoginRequest struct {
	Provider string `json:"provider"`
	// SSOStartURL is required by (and only by) the AWS flow — see
	// validateSSOStartURL and launchHarnessLoginRun.
	SSOStartURL string `json:"sso_start_url"`
}

// validateSSOStartURL guards the one operator-supplied value that gets written
// into a file inside the sandbox: it must be a plain https URL with no
// whitespace (a newline would let a paste smuggle extra keys into the generated
// INI). Not an authorization check — the egress policy still decides what the
// sandbox may dial.
func validateSSOStartURL(raw string) error {
	if strings.ContainsAny(raw, " \t\r\n") {
		return fmt.Errorf("the AWS access portal URL must not contain whitespace")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("that does not look like an AWS access portal URL (expected e.g. https://my-org.awsapps.com/start)")
	}
	return nil
}

type harnessLoginResponse struct {
	RunID string `json:"run_id"`
	// State the run is in AS ANSWERED — PENDING, because the answer now
	// precedes dispatch. The pane polls GET /runs/{id} from here rather
	// than mounting a terminal on a run the attach-ticket route would 409.
	State string `json:"state"`
}

// harnessLoginMechanism is the agent mechanism a login provider's flow actually
// captures — one row per lane, the house style of this file. "" means no
// declared mechanism is captured by that flow, which is every provider but AWS
// (per_user is bedrock_sso-only, types.AgentProvider).
func harnessLoginMechanism(provider string) types.AgentMechanism {
	if provider == awsSSOProvider {
		return types.AgentMechanismBedrockSSO
	}
	return ""
}

// perUserLoginRow finds the ENABLED per_user roster row whose declared mechanism
// this login flow captures — the row that makes a member's sign-in a thing the
// org asked for rather than a member launching a sandbox on their own authority.
//
// It also carries the ADMIN-OWNED start URL the launch must use: a member's
// login ignores whatever start URL arrived with the request (see the launch
// below), so the row is the only source of it.
//
// Keyed by agent, not only by mechanism, because the capture that follows this
// door is scoped by agent (awsSSOScopeFor reads the modelAccessAgent row). If
// the two ever disagreed — a per_user bedrock_sso row on some OTHER agent id —
// this door would admit a member whose upload then resolved the ZERO scope and
// wrote into the OPERATOR namespace. Unreachable today (bedrock_sso is
// claude-code's lane alone, and validation refuses per_user anywhere else), and
// closed here rather than left to stay that way by coincidence.
func perUserLoginRow(sc types.SiteConfig, provider string) (types.AgentProvider, bool) {
	mech := harnessLoginMechanism(provider)
	if mech == "" {
		return types.AgentProvider{}, false
	}
	row, ok := agentProviderFor(sc, modelAccessAgent)
	if !ok || row.Disabled || row.Mechanism != mech || row.CredentialSource != types.CredentialSourcePerUser {
		return types.AgentProvider{}, false
	}
	return row, true
}

// harnessLoginMemberRefusal / harnessLoginAgentRefusal are the two doors a
// non-operator meets here. DRAFT (M2 canon pending) — server-refusal shape
// (docs/design/workspace-providers-prompt.md §7): a lowercase-opening clause
// naming what was refused, rendered verbatim by the console.
const (
	harnessLoginNotPerUserRefusal = "signing in to a model provider yourself is not how this deployment is set up — its model credential is one an admin connects for everyone"
	// DRAFT (M2 canon pending)
	harnessLoginAgentRefusal = "you are not granted agent %s — ask an admin to grant it before signing in to its model provider"
)

// authorizeHarnessLogin decides whether THIS caller may launch a container-login
// sandbox for provider, and returns the per_user roster row when one governs it.
//
// An OPERATOR always may — the route's whole original purpose is an admin
// connecting the shared credential, and under a per_user row the admin captures
// their OWN session exactly as anyone else does (it is the one they will be
// asked about first).
//
// Anyone else — a member, and a security admin, who owns their own secrets like
// any other principal — needs TWO things, and the predicate lives HERE rather
// than at the router because both are per-request facts the router cannot see:
// an enabled per_user row for this provider (the org saying "each person signs
// in"), and capAgent on that row's agent. capAgent, not the login sandbox's own
// aws-sso image: what a grant bounds is which agent's runs a member may launch,
// and the credential this captures is for the row's agent.
//
// The login lane never passes denyMemberRequest: there is no policy, image,
// workspace or integration in this request to narrow.
//
// Returns ok=false when it has already written the refusal.
func (s *Server) authorizeHarnessLogin(w http.ResponseWriter, r *http.Request, provider string) (types.AgentProvider, awsSSOScope, bool) {
	// Fail closed on an unreadable roster: dropping ok read a store blip as "no
	// per_user row", so the launch stamped an EMPTY pin ("launched unpinned" at
	// capture) and the caller's own start URL became the bound portal. A NIL
	// store is not that blip — no store, no roster, operator-only door — and
	// awsSSOScopeForAgent reads a missing store the same way.
	sc, ok := s.siteConfigSnapshot(r.Context())
	if !ok && s.cfg.Store != nil {
		writeError(w, http.StatusServiceUnavailable, harnessLoginRosterUnavailable)
		return types.AgentProvider{}, awsSSOScope{}, false
	}
	// WHOSE credential this may capture, from THIS proved read.
	scope := awsSSOScopeFor(sc, modelAccessAgent, runIdentitySubject(r.Context(), principalFromRequest(r)))
	row, perUser := perUserLoginRow(sc, provider)
	mechanismCaller := s.cfg.OIDC != nil && runIdentitySubject(r.Context(), principalFromRequest(r)) == adminTokenPrincipal
	if perUser && mechanismCaller {
		return types.AgentProvider{}, awsSSOScope{}, s.refuseHarnessLoginMechanismPrincipal(w, r)
	}
	if s.isOperator(r.Context()) {
		return row, scope, true
	}
	if !perUser {
		return types.AgentProvider{}, awsSSOScope{}, !s.denyMemberField(w, r, "setup.harness_login",
			"harness_login_not_per_user", harnessLoginNotPerUserRefusal)
	}
	if s.denyMemberCapability(w, r, capAgent, row.ID, "setup.harness_login",
		fmt.Sprintf(harnessLoginAgentRefusal, row.ID)) {
		return types.AgentProvider{}, awsSSOScope{}, false
	}
	return row, scope, true
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
	// Shape + lane guards, all of them (harnesscred_paste.go): a
	// captureViaHelper provider is not pasteable at all, and an empty or
	// over-long token is refused before it reaches the store or the
	// process-global mask corpus.
	if msg := harnessPasteRefusal(hl, token); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	blob := managedCredBlob{Token: token, CapturedAt: s.cfg.Now().UTC()}
	raw, _ := json.Marshal(blob)
	if err := s.cfg.Secrets.Put(r.Context(), hl.secretName, raw); err != nil { // operator-wide route (operatorOnly), not per-principal
		writeServerError(w, r, "store managed credential", err)
		return
	}
	// Register the token PROCESS-GLOBALLY so it is masked out of every run's PTY
	// capture, asciicast and decision log — not just the runs it is injected into.
	// A per-run Add cannot cover it: the value is minted outside any run's mint
	// path, so nothing else ever tells the registry it exists.
	//
	// Honest residual: masking is write-time, never retroactive. The login run's
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
//
// Scoped the way the capture was. Under a `per_user` row every AWS SSO capture
// — the operator's own included — lives in For(subject) (storeAWSSSOBlob), so
// the delete must go through the same scope the write did — an unscoped
// Delete would remove NOTHING anybody had captured while still answering
// {"captured": false}, a no-op on a per-user estate. Scoping the delete makes
// this the CALLER's own blob.
//
// Honest ceiling: the route stays operatorOnly, so this revokes the
// OPERATOR's own captured session, never a named member's. A member's stored
// session is superseded by their next sign-in, ends at the IdP when an admin
// revokes the session there, and expires with its OIDC client registration.
// Self-service member Disconnect and an admin "revoke this person's session"
// arm are 0.8 items — see docs/OPERATIONS.md "AWS SSO per-user" and the
// THREAT-MODEL residency row, which say so in those words.
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
	st := s.cfg.Secrets
	if hl.provider == awsSSOProvider {
		// Only the AWS lane can be per-user (per_user is bedrock_sso-only,
		// types.AgentProvider); every other provider keeps the operator-wide row
		// this route has always deleted.
		// Fail closed on an unreadable roster: read as "not per-user", a store blip
		// would point this Delete at the OPERATOR-WIDE row, not the caller's own.
		scope, ok := s.awsSSOScopeForAgent(r.Context(), modelAccessAgent,
			runIdentitySubject(r.Context(), principalFromRequest(r)))
		if !ok {
			writeError(w, http.StatusServiceUnavailable, harnessDisconnectRosterUnavailable)
			return
		}
		if scope.namespaced() {
			st = st.For(scope.owner)
		}
	}
	if err := st.Delete(r.Context(), hl.secretName); err != nil {
		writeServerError(w, r, "delete managed credential", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"harness.credential.disconnected", hl.secretName, "success",
		mustJSON(map[string]any{"provider": hl.provider})))
	writeJSON(w, http.StatusOK, map[string]any{"provider": hl.provider, "captured": false})
}

// Managed provider (subscription.Provider over the stored blob)

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
	// p.store is the operator-wide managed credential (NewManagedCredProvider's
	// caller passes the raw, unscoped store) — not per-principal.
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
