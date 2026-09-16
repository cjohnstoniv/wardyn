// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// awssso_refresh.go renews a captured AWS IAM Identity Center (SSO) credential
// CONTROL-PLANE SIDE, at dispatch, instead of shipping the refresh token into
// the sandbox and hoping the in-sandbox AWS SDK renews it.
//
// Why the control plane owns this. `CreateToken(grant_type=refresh_token)`
// ROTATES the refresh token: the response carries a new one and the old one must
// be assumed spent. A sandbox that refreshes does so into an EPHEMERAL ~/.aws
// and then dies, so the rotated pair is lost and the stored pair is already
// spent — one heal, then a permanently dead credential that setup status keeps
// reporting as "renewable". Wardyn is the only party that can PERSIST the
// rotated pair, so Wardyn is the only party that may redeem it: ONE refresher
// per token. That is the same single-owner discipline the managed setup-token
// lane already states for itself (see managedCredBlob) — and the reason
// awsSSOCacheFileContents withholds clientId/clientSecret/refreshToken from the
// sandbox cache whenever a refresh token exists.
//
// Widens nothing: the refresh token is ALREADY resident in the sandbox today, so
// redeeming it here narrows residency rather than adding a new exposure — see
// the Bedrock captured-AWS-SSO row of threatmodel/THREAT-MODEL.md.
//
// No background renewer in 0.7.2: dispatch-time refresh with the skew window
// below covers every run a timer could have saved, and a timer adds a scheduler,
// a lock and an unwatched failure mode. A run that outlives its refreshed access
// token fails visibly at its first model call; a mid-run renewal channel is a
// later change.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// awsSSORefreshSkew is how far AHEAD of the access token's expiry dispatch
// renews it, so a run never boots inside an SDK's own pre-expiry refresh window.
//
// The SDKs do not agree on that window, so the number is chosen against the
// SMALLER one and the larger is shown to be harmless:
//   - aws-sdk-js-v3 refreshes within 5 minutes of expiry. 10 > 5, so a run
//     dispatched now starts outside it.
//   - botocore's SSOTokenProvider uses 15 MINUTES, which no skew below 15 could
//     clear. It does not matter: botocore does not THROW on a cache that lacks
//     the refresh fields — a cache carrying only accessToken/expiresAt/startUrl/
//     region loads normally both at 50 minutes and at 3 minutes of remaining
//     validity (verified against botocore 1.43.93), and raises only once the
//     token is actually EXPIRED. So inside its window it simply keeps using the
//     token it has, which is the behaviour this lane wants.
//
// Either way the control plane stays the only party that redeems the refresh
// token, because awsSSOCacheFileContents withholds the fields an SDK would need
// to redeem it.
const awsSSORefreshSkew = 10 * time.Minute

// awsSSORefreshRetryDelay is the single backoff between the two CreateToken
// attempts a TRANSIENT failure gets (throttling, 5xx, a transport error). One
// retry, not a loop: N members dispatching at the top of the hour on a
// one-hour-token estate must not turn one throttle into a retry storm.
const awsSSORefreshRetryDelay = 400 * time.Millisecond

// awsSSORefreshTimeout bounds ONE CreateToken attempt. Dispatch is waiting on
// it, so an unresponsive OIDC endpoint must fail the lane rather than hang the
// launch.
const awsSSORefreshTimeout = 10 * time.Second

// awsSSOTokenURL builds the SSO-OIDC CreateToken endpoint for an SSO region.
// Derived from the credential's OWN region — there is no configuration knob and
// no environment variable for it, by design: the blob names the region it was
// captured in, and a second spelling could only ever disagree with it.
//
// A var, not a func, for exactly one reason: the tests point it at an httptest
// server. Nothing in the daemon reassigns it. A DEPLOYMENT redirects it through
// Server.awsSSOTokenEndpoint instead (Config.AWSSSOEndpointOverride, the gated
// test hatch in awssso_endpoint.go) — configuration belongs on Config, where a
// boot log and the ENV.md registry can name it, not in a package var.
var awsSSOTokenURL = func(ssoRegion string) string {
	return "https://oidc." + ssoRegion + ".amazonaws.com/token"
}

// awsSSORegionPattern is the AWS region grammar, and it is a HOST-SHAPE check:
// the region is concatenated into `oidc.<region>.amazonaws.com`, and the only
// guard a stored region had passed was repoFieldSafe (control characters and
// whitespace, ssotoken.go) — which admits `/` and `@`, so a region of
// `x.attacker.com/` yields the host `oidc.x.attacker.com` and POSTs the client
// secret and the refresh token to it. Not reachable from a sandbox today (the
// F006 binding pins an uploaded blob's region to the operator's own boot
// config), so this is defence in depth for a pre-0.7.2 blob, a direct store
// write, or an operator typo. Covers the commercial, GovCloud and ISO
// partitions (us-east-1, us-gov-west-1, us-iso-east-1, us-isob-east-1).
var awsSSORegionPattern = regexp.MustCompile(`^[a-z]{2}(-gov|-iso[a-z]?)?-[a-z]+-\d$`)

// errAWSSSOCredentialSpent marks the failure class that means the REFRESH TOKEN
// itself is gone — not that the call failed. Wrapped around the OIDC error code
// so a log line still names which code said so.
var errAWSSSOCredentialSpent = errors.New("aws sso refresh credential is spent")

// ── DRAFT (M2 canon pending) ────────────────────────────────────────────────
// The three sentences below are the user-facing copy this lane introduces. They
// are held in ONE block so the canon swap is a single-file diff, and every test
// asserts through the constants rather than the literals.
const (
	// awsSSORefreshSpentSentence: the refresh token is gone. The person must
	// sign in again; nothing Wardyn can do renews this credential, and Wardyn
	// does not quietly bill a different model provider instead.
	//
	// DRAFT (M2 canon pending)
	awsSSORefreshSpentSentence = "this run's model access is configured as Amazon Bedrock (captured AWS SSO session), " +
		"and that session can no longer be renewed — sign in again under Settings → Model provider. " +
		"Wardyn does not substitute a different model provider."

	// awsSSORefreshUnavailableSentence: the renewal could not be completed
	// (throttled, or the OIDC endpoint was unreachable) AND the access token has
	// already expired, so there is nothing left to serve this run with. The
	// refresh token is NOT spent — the next dispatch redeems it normally — so the
	// copy says "try again", never "sign in again". A renewal that fails while
	// the access token is still valid never reaches this sentence at all: that
	// run is served from the token in hand.
	//
	// DRAFT (M2 canon pending)
	awsSSORefreshUnavailableSentence = "this run's model access is configured as Amazon Bedrock (captured AWS SSO session), " +
		"and renewing that session did not complete — AWS did not answer the token request. " +
		"Your sign-in is still good; launch again in a moment. Wardyn does not substitute a different model provider."

	// credSourceSSODesc names the captured-SSO lane in setup copy. It replaces
	// "re-login when it expires", which stopped being true the moment dispatch
	// started renewing the credential itself.
	//
	// DRAFT (M2 canon pending)
	credSourceSSODesc = "your captured AWS SSO session (container login; Wardyn renews it at launch while its refresh token lives)"
)

// harnessCredentialAWSRenewingDetail / Fix are the admin setup row for a
// captured SSO session whose ACCESS token has lapsed but whose refresh token has
// not: there is nothing for the operator to do, so the row stopped being a warn.
// %s is the expiry timestamp.
//
// DRAFT (M2 canon pending)
const (
	harnessCredentialAWSRenewingDetail = "A captured AWS SSO session is connected. Its access token lapsed at %s, " +
		"and Wardyn renews it automatically at launch while its refresh token lives — no re-login needed."
	harnessCredentialAWSRenewingFix = "Nothing to do. Re-run the containerized AWS SSO login only if a run reports that the session can no longer be renewed."
)

// awsSSOTokenResponse is the subset of the SSO-OIDC CreateToken response this
// lane consumes, plus the error shape a refusal answers with. `refreshToken` is
// documented as present "if present", so an ABSENT one means KEEP the stored
// token — never blank it.
type awsSSOTokenResponse struct {
	AccessToken  string `json:"accessToken"`
	ExpiresIn    int    `json:"expiresIn"` // seconds from now
	RefreshToken string `json:"refreshToken"`
	// Error is the classification oracle. The STATUS CODE alone is not: a
	// throttle (`slow_down`) arrives as HTTP 400, exactly like `invalid_grant`,
	// and treating 400 as "spent" would sign a whole fleet out on one throttle.
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// awsSSOErrorIsSpent classifies a CreateToken error code. These four mean the
// grant or the client registration behind it is gone, so no retry and no other
// refresh token will help: the lane is dead until the person signs in again.
// Everything else — `slow_down`, `internal_failure`, an unparseable body, a
// transport error — is transient.
func awsSSOErrorIsSpent(code string) bool {
	switch code {
	case "invalid_grant", "expired_token", "invalid_client", "unauthorized_client":
		return true
	}
	return false
}

// needsRefresh reports whether the access token is expired OR expires inside
// awsSSORefreshSkew.
func (b awsSSOBlob) needsRefresh(now time.Time) bool {
	return !b.ExpiresAt.After(now.Add(awsSSORefreshSkew))
}

// registrationLapsed reports whether the OIDC client registration behind the
// refresh token has expired.
//
// A ZERO RegistrationExpiresAt counts as LIVE, deliberately: the field is a
// time.Time under omitempty, which encoding/json never omits, so a helper that
// saw no registration expiry at capture serialises the zero time rather than
// leaving the key out. Treating that as "lapsed" would refuse to renew every
// credential captured by such a helper. With a refresh token and no registration
// timestamp, dispatch attempts the refresh and the OIDC response is the oracle.
func (b awsSSOBlob) registrationLapsed(now time.Time) bool {
	return !b.RegistrationExpiresAt.IsZero() && !now.Before(b.RegistrationExpiresAt)
}

// renewable reports whether this credential can be healed without a human: it
// carries a refresh token and its client registration has not lapsed. An
// EXPIRED access token on a renewable blob is not a dead credential — dispatch
// renews it — which is why readiness is (renewable || !expired), never !expired.
func (b awsSSOBlob) renewable(now time.Time) bool {
	return b.RefreshToken != "" && !b.registrationLapsed(now)
}

// awsSSOTokenFingerprint identifies a refresh token without storing it: the
// spent-marks map is keyed by this, so the map holds no credential material.
func awsSSOTokenFingerprint(refreshToken string) string {
	sum := sha256.Sum256([]byte(refreshToken))
	return hex.EncodeToString(sum[:8])
}

// lockAWSSSOOwner takes the per-owner single-flight lock and returns its
// release. Owner is the secret-store namespace the blob was read from: "" is
// the operator-wide one (a `shared` credential), and under a per_user row it is
// the principal's own — so two people renewing at once never serialise behind
// each other, and two dispatches of the SAME person's runs always do.
//
// Also taken by the login-capture upload (handleUploadSSOToken), whose once-only
// guard is a read-then-put over the same namespace: one lock per namespace, so
// every read-modify-write of a stored AWS SSO credential is serialised, whichever
// path makes it.
func (s *Server) lockAWSSSOOwner(owner string) func() {
	s.ssoRefreshMu.Lock()
	if s.ssoRefreshLocks == nil {
		s.ssoRefreshLocks = map[string]*sync.Mutex{}
	}
	mu := s.ssoRefreshLocks[owner]
	if mu == nil {
		mu = &sync.Mutex{}
		s.ssoRefreshLocks[owner] = mu
	}
	s.ssoRefreshMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// awsSSOTokenSpent / markAWSSSOTokenSpent read and write the spent-marks map.
func (s *Server) awsSSOTokenSpent(fingerprint string) bool {
	s.ssoRefreshMu.Lock()
	defer s.ssoRefreshMu.Unlock()
	return s.ssoRefreshSpent[fingerprint]
}

func (s *Server) markAWSSSOTokenSpent(fingerprint string) {
	s.ssoRefreshMu.Lock()
	defer s.ssoRefreshMu.Unlock()
	if s.ssoRefreshSpent == nil {
		s.ssoRefreshSpent = map[string]bool{}
	}
	s.ssoRefreshSpent[fingerprint] = true
}

// refreshAWSSSOBlob renews `blob` when it needs renewing and returns the blob
// the caller should USE plus, on failure, the sentence the caller should refuse
// with ("" means "use the returned blob").
//
// It returns (blob, "") untouched — no call, no lock — whenever there is nothing
// to do: no refresh token, a lapsed registration (the caller's own
// renewable/expired predicate then decides), or an access token still comfortably
// inside the skew window.
//
// READ-CHECK-ACT under the owner lock: the expiry re-check and the CreateToken
// are both INSIDE the lock, and the blob is re-read there, because a second
// dispatch of the same principal may have read the pre-refresh blob before the
// first flight even started. Without the re-read it would redeem the stale token,
// get invalid_grant, and clobber the pair the first flight just persisted.
//
// A Put that fails AFTER a successful redeem does NOT fail this run: the rotated
// pair is already spent at AWS, so re-redeeming is impossible and refusing would
// throw away a credential we hold. This run is served from the in-memory blob and
// the persist failure is audited — the NEXT dispatch reads the old, now-spent
// pair and asks the person to sign in again rather than redeeming it twice.
func (s *Server) refreshAWSSSOBlob(ctx context.Context, scope awsSSOScope, blob awsSSOBlob) (awsSSOBlob, string) {
	now := s.cfg.Now()
	if !blob.renewable(now) || !blob.needsRefresh(now) {
		return blob, ""
	}
	if s.awsSSOTokenSpent(awsSSOTokenFingerprint(blob.RefreshToken)) {
		return blob, awsSSORefreshSpentSentence
	}

	unlock := s.lockAWSSSOOwner(scope.owner)
	defer unlock()

	// Re-read inside the lock; a store error here is not fatal (we still hold a
	// blob), an absent credential is (it was disconnected mid-flight). Through
	// the SAME scope the caller read it with: re-reading the operator's row for a
	// per_user principal would renew — and re-persist — the wrong credential.
	if cur, found, rerr := s.readAWSSSOBlob(ctx, scope); rerr == nil {
		if !found {
			return blob, awsSSORefreshSpentSentence
		}
		blob = cur
	}
	now = s.cfg.Now()
	if !blob.renewable(now) || !blob.needsRefresh(now) {
		return blob, "" // another flight already renewed it
	}
	fingerprint := awsSSOTokenFingerprint(blob.RefreshToken)
	if s.awsSSOTokenSpent(fingerprint) {
		return blob, awsSSORefreshSpentSentence
	}

	resp, err := s.createAWSSSOTokenWithRetry(ctx, blob)
	if err != nil {
		spent := errors.Is(err, errAWSSSOCredentialSpent)
		if spent {
			s.markAWSSSOTokenSpent(fingerprint)
		}
		slog.ErrorContext(ctx, "wardynd: renewing the captured AWS SSO credential failed",
			slog.Bool("credential_spent", spent), slog.Any("err", err))
		s.auditAWSSSORefresh(ctx, scope, "failure", map[string]any{
			"provider": awsSSOProvider, "spent": spent, "error": err.Error(),
		})
		if spent {
			return blob, awsSSORefreshSpentSentence
		}
		// A TRANSIENT failure is not a reason to stop using a token we still hold.
		// needsRefresh fires a whole skew window (10 min) AHEAD of expiry, so most
		// renewals run against a still-valid access token — failing the lane there
		// would turn one AWS throttle into a dead credential for a run that had
		// minutes of headroom, and (until the dispatch-time mechanism gate lands)
		// let it cross silently to the api-key placeholder. Serve the token; the
		// next dispatch renews it, and the failure is audited either way.
		if !blob.expired(s.cfg.Now()) {
			slog.WarnContext(ctx, "wardynd: renewing the captured AWS SSO credential failed, but the current token is still valid; serving it")
			return blob, ""
		}
		return blob, awsSSORefreshUnavailableSentence
	}

	next := blob
	next.AccessToken = resp.AccessToken
	next.ExpiresAt = s.cfg.Now().Add(time.Duration(resp.ExpiresIn) * time.Second).UTC()
	rotated := resp.RefreshToken != "" && resp.RefreshToken != blob.RefreshToken
	if resp.RefreshToken != "" {
		next.RefreshToken = resp.RefreshToken
	}
	// Mask BEFORE anything can persist or log the new values, and globally for
	// the same reason the capture path masks globally: one credential is reused
	// across every run that selects this lane.
	s.cfg.MaskRegistry.AddGlobal([]byte(next.AccessToken))
	s.cfg.MaskRegistry.AddGlobal([]byte(next.RefreshToken))

	data := map[string]any{
		"provider":   awsSSOProvider,
		"expires_at": next.ExpiresAt.Format(time.RFC3339),
		"rotated":    rotated,
	}
	// OMITTED WHEN ZERO (B2-F8), as awsSSOCacheFileContents already does for the
	// same field: a zero RegistrationExpiresAt means the capturing helper saw no
	// registration expiry, which registrationLapsed reads as LIVE. Formatting it
	// rendered 0001-01-01T00:00:00Z into the trail — a date that reads as
	// "lapsed long ago", i.e. the exact opposite of what it means.
	if !next.RegistrationExpiresAt.IsZero() {
		data["registration_expires_at"] = next.RegistrationExpiresAt.UTC().Format(time.RFC3339)
	}
	outcome := "success"
	if perr := s.storeAWSSSOBlob(ctx, scope, next); perr != nil {
		// Redeemed but not stored — see the doc comment. Audited as a failure so
		// the row is not read as "the rotated pair is safe", and the run still
		// gets its credential.
		outcome = "failure"
		data["persist_error"] = perr.Error()
		slog.ErrorContext(ctx, "wardynd: persisting the renewed AWS SSO credential failed; serving this run from memory",
			slog.Any("err", perr))
	}
	s.auditAWSSSORefresh(ctx, scope, outcome, data)
	return next, ""
}

// auditAWSSSORefresh emits the harness.credential.refresh row. Run-less: the
// credential is per-principal, not per-run, and resolveBedrockAuth is reached
// from create and preflight as well as dispatch.
//
// owner + credential_source are what make the row answerable under per_user:
// "" and "shared" is the one credential everybody's runs use, a subject and
// "per_user" is that person's own — and the pair is how a reader tells a
// fleet-wide outage from one lapsed sign-in.
func (s *Server) auditAWSSSORefresh(ctx context.Context, scope awsSSOScope, outcome string, data map[string]any) {
	data["owner"] = scope.owner
	data["credential_source"] = awsSSOCredentialSourceLabel(scope)
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "wardynd",
		"harness.credential.refresh", harnessCredSecretName(awsSSOProvider), outcome, mustJSON(data)))
}

// createAWSSSOTokenWithRetry is createAWSSSOToken plus the ONE backoff retry a
// transient failure gets. A spent credential is never retried — the answer will
// not change, and hammering it is how a throttle becomes a fleet outage.
func (s *Server) createAWSSSOTokenWithRetry(ctx context.Context, blob awsSSOBlob) (awsSSOTokenResponse, error) {
	resp, err := s.createAWSSSOToken(ctx, blob)
	if err == nil || errors.Is(err, errAWSSSOCredentialSpent) {
		return resp, err
	}
	select {
	case <-ctx.Done():
		return resp, err
	case <-time.After(awsSSORefreshRetryDelay):
	}
	return s.createAWSSSOToken(ctx, blob)
}

// createAWSSSOToken performs the SSO-OIDC CreateToken refresh_token grant.
//
// Hand-rolled over net/http on purpose: the call is `authtype:none` (unsigned,
// no SigV4), the request and response are four JSON fields each, and the module
// that would sign it is not a dependency of this repo. http.DefaultTransport is
// the transport so the call honours the PROCESS proxy environment exactly as the
// GitHub broker's client does — wardynd's own egress is a separate channel from
// the sandbox proxy's, and a deployment behind a corporate proxy needs this hop
// to follow it.
func (s *Server) createAWSSSOToken(ctx context.Context, blob awsSSOBlob) (awsSSOTokenResponse, error) {
	var out awsSSOTokenResponse
	// BEFORE the URL is composed, never after: a region that is not a region is
	// a hostname, and this request body carries the client secret and the refresh
	// token. Returned as an ordinary (non-spent) error, so the credential is left
	// intact and the failure surfaces on the existing visible-failure path rather
	// than as a request to a composed host.
	if !awsSSORegionPattern.MatchString(blob.Region) {
		return out, fmt.Errorf("aws sso create-token: stored credential region %q is not an AWS region", blob.Region)
	}
	body, err := json.Marshal(map[string]string{
		"clientId":     blob.ClientID,
		"clientSecret": blob.ClientSecret,
		"grantType":    "refresh_token",
		"refreshToken": blob.RefreshToken,
	})
	if err != nil {
		return out, fmt.Errorf("aws sso create-token: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.awsSSOTokenEndpoint(blob.Region), bytes.NewReader(body))
	if err != nil {
		return out, fmt.Errorf("aws sso create-token: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// CheckRedirect refuses to FOLLOW anything: this request body carries the
	// client secret and the refresh token, and net/http replays a POST body on a
	// 307/308. A redirect to another host would hand both to it. The endpoint
	// does not redirect, so a 3xx is answered as the unusable response it is.
	client := &http.Client{
		Transport:     http.DefaultTransport,
		Timeout:       awsSSORefreshTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return out, fmt.Errorf("aws sso create-token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Bounded read: the body is four fields, and an unbounded read from a host
	// this process dials directly is an easy way to eat the daemon's memory.
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if rerr != nil {
		return out, fmt.Errorf("aws sso create-token: read response: %w", rerr)
	}
	// An unparseable body is a TRANSIENT failure, not a spent credential: only
	// an error code we recognise may retire a credential.
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return out, fmt.Errorf("aws sso create-token: parse response (status %d): %w", resp.StatusCode, uerr)
	}
	if out.Error != "" {
		if awsSSOErrorIsSpent(out.Error) {
			return out, fmt.Errorf("%w: %s", errAWSSSOCredentialSpent, out.Error)
		}
		return out, fmt.Errorf("aws sso create-token refused: %s", out.Error)
	}
	// Any 2xx, not 200 alone — the success contract is a usable body, and pinning
	// the exact code buys nothing while a 201/202 would read as a failure.
	if resp.StatusCode < 200 || resp.StatusCode > 299 || out.AccessToken == "" || out.ExpiresIn <= 0 {
		return out, fmt.Errorf("aws sso create-token: unusable response (status %d)", resp.StatusCode)
	}
	return out, nil
}
