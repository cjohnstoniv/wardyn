// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// AWS SSO account/role PINNING — the control-plane half of finding 1.
//
// The defect: cmd/wardyn-aws-sso took AccountList[0]/RoleList[0], so a cloud
// team granting an unrelated SSO entitlement inserted an element at index 0 and
// silently re-pointed the AWS identity every run in the deployment signed with.
// The helper now verifies a pin or asks the person; THIS file is why it cannot
// be talked out of that from inside the sandbox.
//
// Three things live here, and they are one idea seen from three sides:
//
//	awsSSOPin / awsSSOPinEnv  the admin's roster pin, carried to the login
//	                          sandbox as launch env (never as sandbox input)
//	bindCaptureToPin          the upload's refusal predicate: the blob must
//	                          agree with the LAUNCH-TIME stamp and with the
//	                          account the configured Bedrock model lives in
//	refuseCapture             the audited refusal, so a rejected capture is a
//	                          row in the trail rather than a 400 nobody sees
//
// WHY REFUSE AND NOT REWRITE. The stored blob is baked VERBATIM into every
// later Bedrock run's ~/.aws/config (awsSSOConfigFileContents, runs_bedrock.go).
// Rewriting a disagreeing capture would record a session the person never saw
// and would only move the IAM 403 from capture time back to run time — the
// finding's own complaint, ten retries deep inside somebody else's terminal.
package api

import (
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// awsSSOPin is the admin-owned account/role pin from the roster row, as the
// launch carries it. Both halves or neither — see validateAgentCredentialSource.
type awsSSOPin struct {
	AccountID string
	RoleName  string
}

func (p awsSSOPin) set() bool { return p.AccountID != "" && p.RoleName != "" }

// The env names the login sandbox's helper reads the pin from. They are
// NON-SECRET by construction: an AWS account id and an IAM role name are
// configuration, not credentials, and the sandbox is about to learn both from
// the portal anyway.
const (
	awsSSOPinAccountEnvVar = "WARDYN_AWS_SSO_ACCOUNT_ID"
	awsSSOPinRoleEnvVar    = "WARDYN_AWS_SSO_ROLE_NAME"
)

// awsSSOPinEnv is the pin as sandbox env, or NIL for an unpinned row — so an
// unpinned launch is byte-identical to what it was before this lane.
func awsSSOPinEnv(pin awsSSOPin) map[string]string {
	if !pin.set() {
		return nil
	}
	return map[string]string{
		awsSSOPinAccountEnvVar: pin.AccountID,
		awsSSOPinRoleEnvVar:    pin.RoleName,
	}
}

// loginEnv is the login sandbox's complete NON-SECRET env: the pre-login
// ~/.aws/config (loginConfigEnv) plus the admin's account/role pin.
//
// It exists as its own function for one reason — loginConfigEnv returns a NIL
// map for a non-AWS flow or a half-known config, and maps.Copy into a nil map
// PANICS. Through the HTTP door that is unreachable (handleHarnessLogin 400s an
// empty start URL or region, and validateAgentSSOPin forbids a pin off a
// bedrock_sso row), but the safety was two validators away from the panic,
// which is not where a panic should live. Here it is one line and one test.
//
// An UNPINNED row adds nothing, so its launch is byte-identical to what it was.
// endpointOverride is the TEST hatch (awssso_endpoint.go): `aws sso login`
// inside the login box reads AWS_ENDPOINT_URL_SSO/_SSO_OIDC, so without them it
// dials the real AWS no matter what the egress allowlist says. nil on every
// real deployment, leaving this map byte-identical to before the knob existed.
func (hl harnessLogin) loginEnv(ssoStartURL, ssoRegion string, pin awsSSOPin, endpointOverride string) map[string]string {
	env := hl.loginConfigEnv(ssoStartURL, ssoRegion)
	// awsSSOPinEnv returns NIL for an unset pin, and maps.Copy into a nil map
	// panics — so the endpoint pair is merged into a fresh map, not into it.
	extra := map[string]string{}
	maps.Copy(extra, awsSSOPinEnv(pin))
	maps.Copy(extra, ssoInjectEndpointEnv(endpointOverride))
	if len(extra) == 0 {
		return env
	}
	if env == nil {
		env = map[string]string{}
	}
	maps.Copy(env, extra)
	return env
}

// The FIXED reason vocabulary on a harness.credential.refused row. A closed set
// on purpose: the alternative is sandbox-chosen text in the audit trail, and an
// incident review filtering "why were captures refused last Tuesday" needs to
// GROUP, which free text cannot do.
const (
	refuseReasonBlobShape        = "blob_shape"
	refuseReasonFieldUnsafe      = "field_unsafe"
	refuseReasonFieldShape       = "field_shape"
	refuseReasonRegionMismatch   = "region_mismatch"
	refuseReasonStartURLMismatch = "start_url_mismatch"
	refuseReasonAccountRolePin   = "account_role_pin_mismatch"
	refuseReasonModelAccount     = "model_account_mismatch"
	refuseReasonUnstampedScope   = "unstamped_scope"
	refuseReasonAlreadyCaptured  = "already_captured"
	refuseReasonStampUnreadable  = "stamp_unreadable"
	refuseReasonStoreError       = "store_error"
)

// ── DRAFT (M2 canon pending) ────────────────────────────────────────────────

const (
	// ssoTokenAccountPinRefusal: the sign-in captured a different account/role
	// than the one this deployment pinned at launch. It names BOTH pairs because
	// the person reading it on a terminal is the one who has to pick again.
	//
	// DRAFT (M2 canon pending)
	ssoTokenAccountPinRefusal = "this sign-in captured account %s / role %s, but this deployment pins AWS sign-ins for this agent to account %s / role %s — sign in again and choose that account and role, or ask an admin to change the pin"
	// ssoTokenModelAccountRefusal: ask 3 of the finding, verbatim ("Wardyn holds
	// both halves — say so at capture"). Refused rather than warned: a
	// wrong-account blob pre-empts every other Bedrock lane the moment it is
	// stored, because resolveBedrockAuth selects a stored SSO credential first.
	//
	// DRAFT (M2 canon pending)
	ssoTokenModelAccountRefusal = "this session is for account %s; the configured Bedrock model lives in account %s — a run using this session would be refused by IAM, so it was not stored"
	// ssoTokenAccountShapeRefusal / ssoTokenRoleShapeRefusal: the capture named
	// an account or role that is not shaped like one. It names the SHAPE rather
	// than echoing the value back: the person reading it on the login terminal
	// picked from a portal list, so "that is not a 12-digit account" tells them
	// the pick did not resolve — and the value itself is already in their
	// terminal.
	//
	// DRAFT (M2 canon pending)
	ssoTokenAccountShapeRefusal = "this sign-in did not resolve to a 12-digit AWS account id, so nothing was stored — sign in again and choose an account from the list"
	// DRAFT (M2 canon pending)
	ssoTokenRoleShapeRefusal = "this sign-in did not resolve to an IAM role name, so nothing was stored — sign in again and choose a role from the list"
	// dispatchRosterUnreadableRefusal answers a DISPATCH whose roster read failed
	// (enforceReadableRosterForCredential, runs_dispatch_llm_mechanism.go): the
	// roster is what says whose model credential this run may use, and serving
	// one from a namespace the daemon could not resolve is the outage that cannot
	// be taken back. Surfaces as the run's failure reason, not an HTTP body.
	//
	// DRAFT (M2 canon pending)
	dispatchRosterUnreadableRefusal = "the agent roster could not be read, so Wardyn cannot tell whose model credential this run may use — nothing was started; try again in a moment"
	// harnessLoginRosterUnavailable answers a LAUNCH whose roster read failed
	// (authorizeHarnessLogin, harnesscred.go): the pin and the admin's access
	// portal both come off that row, so there is nothing to bind a capture to.
	// It lives in THIS file, with the rest of the pin's vocabulary, because
	// harnesscred.go sits against the 1000-line file-size gate — the same reason
	// csrf.go was split out of http.go.
	//
	// DRAFT (M2 canon pending)
	harnessLoginRosterUnavailable = "the agent roster could not be read, so this sign-in cannot be bound to the account and access portal it was meant for — try again in a moment"
	// harnessDisconnectRosterUnavailable answers a DISCONNECT whose roster read
	// failed (handleHarnessDisconnect, harnesscred.go): the roster is what says
	// whether captures live per-person or deployment-wide, so without it there is
	// no way to tell which stored session this would remove.
	//
	// DRAFT (M2 canon pending)
	harnessDisconnectRosterUnavailable = "the agent roster could not be read, so Wardyn cannot tell whose stored sign-in this would remove — try again in a moment"
)

// awsAccountID matches an AWS account id. ONE var for the package: the ARN
// parser below and the roster's own pin validation (agent_providers.go) ask the
// same question about the same kind of value, and two copies of `^\d{12}$` is
// two places for it to drift.
var awsAccountID = regexp.MustCompile(`^\d{12}$`)

// bedrockModelAccount returns the AWS account id a Bedrock model ARN names, or
// "" when the configured model names no account at all.
//
// It FAILS OPEN on a malformed ARN too (an 11- or 13-digit account field, an
// uppercase `ARN:`): the account check simply does not run. That is deliberate
// — see the paragraph below — but it is also silent, so wardynd logs once at
// boot when the model LOOKS like an ARN and this still answers "" (see
// cmd/wardynd's bedrockModelAccountWarning).
//
// "" IS A VALID, COMMON ANSWER, and everything downstream must treat it as SKIP
// rather than as a failure: WARDYN_BEDROCK_MODEL is passed verbatim
// (boot_flags.go) and is most often a bare cross-region inference profile id
// ("us.anthropic.claude-…"), which carries no account. Failing closed on that
// would take capture away from every non-ARN deployment on upgrade, to enforce
// a check there is no data for.
func bedrockModelAccount(model string) string {
	fields := strings.Split(strings.TrimSpace(model), ":")
	if len(fields) < 6 || fields[0] != "arn" || fields[2] != "bedrock" {
		return ""
	}
	if !awsAccountID.MatchString(fields[4]) {
		return ""
	}
	return fields[4]
}

// BedrockModelARNNamesNoAccount reports whether the configured Bedrock model
// LOOKS like an ARN and yet yields no account — an 11- or 13-digit account
// field, an uppercase `ARN:`, a truncated paste.
//
// bedrockModelAccount fails OPEN on those, which is deliberate (it is the same
// answer a bare cross-region profile id gets, and it is what keeps an upgrade
// from taking capture away from every non-ARN deployment). But failing open on
// a TYPO is silent: the account check is simply off, at both doors, with
// nothing to see. wardynd logs this once at boot so "off" is a decision
// somebody can read rather than a thing nobody notices.
func BedrockModelARNNamesNoAccount(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "arn:") &&
		bedrockModelAccount(model) == ""
}

// BedrockSSOPinUnenforced reports the configuration on which NOTHING
// server-side constrains which AWS account and role a sign-in may store: an
// enabled `per_user` `bedrock_sso` row with no `sso_account_id` pin, AND a
// configured Bedrock model that names no account (a bare cross-region profile
// id, or nothing at all).
//
// It is not a defect — the pin is optional on purpose, and a single-account
// tenant never had this problem — it is the RESIDUAL of finding 1, and the
// point is that it was inaudible. bindCaptureToPin skips both of its checks on
// this shape, so the in-sandbox chooser is the only remaining defence and it is
// code the sandbox controls; the one boot warning that existed
// (BedrockModelARNNamesNoAccount) fires exclusively when the model LOOKS like
// an ARN — the typo case, never the bare-id case this file calls "most often"
// true. Exported for cmd/wardynd's boot warning; bedrockProviderCheck reads it
// for the console's own row. Warn, never refuse: refusing would take capture
// away from every unpinned deployment on upgrade.
func BedrockSSOPinUnenforced(sc types.SiteConfig, model string) bool {
	row, ok := perUserLoginRow(sc, awsSSOProvider)
	return ok && row.SSOAccountID == "" && bedrockModelAccount(model) == ""
}

// bedrockPinDisagreement names the roster's account PIN and the account the
// configured Bedrock model lives in — but only when both are known AND they
// DIFFER, which is the one case worth a console row. ("", "") otherwise,
// including a roster that could not be read (a zero SiteConfig has no row).
//
// S2-09 made that disagreement legal: the pin is the admin's deliberate answer,
// so the save door takes it and warns. A journal line is not where an admin
// looks, and finding out at run time as an IAM 403 is the original complaint —
// so the posture rides the bedrock_provider row beside its sibling
// BedrockSSOPinUnenforced (R-04).
func bedrockPinDisagreement(sc types.SiteConfig, model string) (pinAccount, modelAccount string) {
	row, ok := perUserLoginRow(sc, awsSSOProvider)
	if !ok || row.SSOAccountID == "" {
		return "", ""
	}
	if ma := bedrockModelAccount(model); ma != "" && ma != row.SSOAccountID {
		return row.SSOAccountID, ma
	}
	return "", ""
}

// awsSSOPinContradiction answers the question the pin was never asked after
// capture time: does the identity a STORED session names still agree with the
// one this deployment's roster row allows?
//
// It is ONE comparison with two doors — dispatch/create read it through
// bedrockBlobPinMismatch below, and the caller's own /setup/status grading
// (setupModelAccess, modelaccess.go) reads it directly off the blob — because a
// second spelling of it would eventually refuse a run the console called fine.
//
// FOUR THINGS MAKE IT ANSWER "NO CONTRADICTION", and each is load-bearing:
//
//   - no ENABLED per_user bedrock_sso row (perUserLoginRow): a `shared` estate
//     has no per-person identity to contradict, and the save door refuses pin
//     fields on such a row anyway.
//   - an UNPINNED row. The pin is optional on purpose and most estates carry
//     none; firing here would take Bedrock away from every one of them on
//     upgrade — the regression BedrockSSOPinUnenforced's doc warns about, in
//     its dispatch form. Both halves or neither, the discipline awsSSOPin.set()
//     already states: a half-pinned row (unreachable through
//     validateAgentSSOPin, reachable through hand-edited JSONB) constrains
//     nothing, so it refuses nothing.
//   - a stored pair Wardyn does not know. account_id/role_name are `omitempty`
//     on the wire (awsSSOBlob, harnesscred.go) and a blob written by an older
//     binary may carry neither; refusing on absence would be that same upgrade
//     regression, for a comparison there is no data for. BOTH HALVES OR
//     NEITHER on this side too, not merely the account: a half-known stored
//     pair (reachable the same way a half-pinned row is — a hand-edited store
//     row) would otherwise return mismatch=true with a pair no caller can name,
//     and every caller here composes a sentence that names both halves.
//   - agreement.
//
// Returns (stored, pinned, true) ONLY on a genuine contradiction, so a caller
// can name BOTH pairs — which is the whole point: the person reading the
// refusal is the one who has to sign in again.
func awsSSOPinContradiction(sc types.SiteConfig, stored awsSSOPin) (awsSSOPin, awsSSOPin, bool) {
	if !stored.set() {
		return awsSSOPin{}, awsSSOPin{}, false
	}
	row, ok := perUserLoginRow(sc, awsSSOProvider)
	if !ok {
		return awsSSOPin{}, awsSSOPin{}, false
	}
	pinned := awsSSOPin{AccountID: row.SSOAccountID, RoleName: row.SSORoleName}
	if !pinned.set() || stored == pinned {
		return awsSSOPin{}, awsSSOPin{}, false
	}
	return stored, pinned, true
}

// bedrockBlobPinMismatch is that comparison asked of a RESOLVED Bedrock auth:
// the captured-SSO lane, and only it, carries a stored identity
// (bedrockAuth.ssoAccountID). Every other lane — the operator's bearer key, the
// host ~/.aws mount, the resident SigV4 keys, and every non-Bedrock transport —
// has no account/role of its own, so there is nothing for a roster pin to
// disagree with and this answers false for all of them.
func bedrockBlobPinMismatch(sc types.SiteConfig, b bedrockAuth) (stored, pinned awsSSOPin, mismatch bool) {
	if !b.ssoInject {
		return awsSSOPin{}, awsSSOPin{}, false
	}
	return awsSSOPinContradiction(sc, awsSSOPin{AccountID: b.ssoAccountID, RoleName: b.ssoRoleName})
}

// bindCaptureToPin is the upload's identity binding: does this blob name the
// account and role this capture was AUTHORIZED to produce?
//
// Two independent checks, both fail-closed, both against trusted server state:
//
//  1. THE LAUNCH-TIME PIN, read back off this run's own harness.login.started
//     row — never off the live roster. A roster edit mid-login must not
//     re-point a capture already in flight, for the same reason the credential
//     scope is stamped rather than re-resolved (see loginRunScope). An empty
//     stamp pin means "launched unpinned", which is accepted: the run was
//     authorized without one.
//  2. THE CONFIGURED MODEL'S ACCOUNT, when the operator gave a full ARN and the
//     run was launched UNPINNED. Wardyn holds both halves of this comparison
//     already — the blob's account_id and the account in WARDYN_BEDROCK_MODEL —
//     and saying so at capture is ask 3 of the finding. A PIN outranks it
//     (S2-09): it is the admin's deliberate answer to the same question, and a
//     resource-shared application inference profile legitimately lives in
//     another account, so a deployment with one had no configuration that
//     worked. Nothing widens — check 1 still binds the pair the sandbox sends.
//
// Returns ("", "") to accept; (sentence, reason) to refuse.
func bindCaptureToPin(blob awsSSOBlob, stamp loginRunStamp, model string) (msg, reason string) {
	pin := awsSSOPin{AccountID: stamp.SSOAccountID, RoleName: stamp.SSORoleName}
	if pin.set() && (blob.AccountID != pin.AccountID || blob.RoleName != pin.RoleName) {
		return fmt.Sprintf(ssoTokenAccountPinRefusal,
			blob.AccountID, blob.RoleName, pin.AccountID, pin.RoleName), refuseReasonAccountRolePin
	}
	if modelAccount := bedrockModelAccount(model); !pin.set() && modelAccount != "" && blob.AccountID != modelAccount {
		return fmt.Sprintf(ssoTokenModelAccountRefusal, blob.AccountID, modelAccount), refuseReasonModelAccount
	}
	return "", ""
}

// refuseCapture answers a refused sso-token upload AND leaves a row naming why.
//
// Every refusal in handleUploadSSOToken used to be a bare writeError: invisible
// in the trail, so "a capture was refused" was a thing only the person watching
// the login terminal ever knew — and under the original defect they did not
// even get that (nothing was printed). The finding asks for "an audit row
// naming the change"; this is it, for the change that did NOT happen.
//
// The DATA carries the reason from the fixed vocabulary above and nothing the
// sandbox chose: the sentence goes to the caller, never into the log.
//
// scope is the caller's credential scope, or NIL. It is non-nil for the
// refusals that happen AFTER loginRunScope has decided one; those rows then
// carry owner + credential_source exactly like the captured row — the pair that
// makes a per_user estate's refusal stream groupable by person instead of a
// join back through each row's run_id to its harness.login.started. The EARLIER
// refusals pass nil: there is no decided scope yet, and their run's stamp
// already carries the same pair. A POINTER rather than a variadic because the
// answer is genuinely zero-or-one and the signature should say so.
func (s *Server) refuseCapture(w http.ResponseWriter, r *http.Request, claims *identity.Claims, status int, reason, msg string, scope *awsSSOScope) {
	data := map[string]any{"provider": awsSSOProvider, "reason": reason}
	if scope != nil {
		data["owner"] = scope.owner
		data["credential_source"] = awsSSOCredentialSourceLabel(*scope)
	}
	s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"harness.credential.refused", harnessCredSecretName(awsSSOProvider), "failure",
		mustJSON(data)))
	writeError(w, status, msg)
}
