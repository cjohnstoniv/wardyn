// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// AWS SSO account/role PINNING — the control-plane half of the defect below.
//
// The defect: cmd/wardyn-aws-sso took AccountList[0]/RoleList[0], so a cloud
// team granting an unrelated SSO entitlement inserted an element at index 0 and
// silently re-pointed the AWS identity every run in the deployment signed with.
// The helper now verifies a pin or asks the person; THIS file is why it cannot
// be talked out of that from inside the sandbox.
//
// Three things live here, and they are one idea seen from three sides:
//
//	awsSSOPin / awsSSOPinEnv  the admin's provider pin, carried to the login
//	                          sandbox as launch env (never as sandbox input)
//	bindCaptureToPin          the upload's refusal predicate: the blob must
//	                          agree with the LAUNCH-TIME stamp and with the
//	                          account the configured Bedrock model lives in
//	refuseCapture             the audited refusal, so a rejected capture is a
//	                          row in the trail rather than a 400 nobody sees
//
// Why refuse and not rewrite. The stored blob is baked VERBATIM into every
// later Bedrock run's ~/.aws/config (awsSSOConfigFileContents).
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

// awsSSOPin is the admin-owned account/role pin from a bedrock_sso provider, as
// the launch carries it. Both halves or neither — see validateProviderPin.
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
// PANICS. Through the HTTP door that is unreachable (validateProviderBedrock
// requires a start URL and region on a bedrock_sso provider, and forbids a pin
// elsewhere), but the safety was two validators away from the panic, which is
// not where a panic should live. Here it is one line and one test.
//
// An UNPINNED row adds nothing, so its launch is byte-identical to what it was.
// endpointOverride is the TEST hatch (awssso_endpoint.go): `aws sso login`
// inside the login box reads AWS_ENDPOINT_URL_SSO/_SSO_OIDC, so without them it
// dials the real AWS no matter what the egress allowlist says. Both consumers
// in that sandbox are covered: the AWS CLI's device-code half reads the pair,
// and cmd/wardyn-aws-sso's own SSO PORTAL reads (ssoPortalBase) honour
// AWS_ENDPOINT_URL_SSO — the helper makes no SSO-OIDC call of its own. Those
// portal reads went to the real portal until the helper read the var. nil on
// every real deployment, leaving this map byte-identical to before the knob.
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

// The FIXED reason vocabulary on a harness.credential.refuse row. A closed set
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
	// refuseReasonRunKilled: the login run this upload comes from has been
	// KILLED — by its own Cancel, or by the person's next sign-in superseding it
	// (harnesscred_supersede.go). See ssoTokenRunKilledRefusal.
	refuseReasonRunKilled = "run_killed"
	// refuseReasonProviderChanged: a sign-in through a model provider's own
	// door whose provider was removed, re-kinded or re-addressed while the
	// login sandbox was open (storeProviderSignIn).
	refuseReasonProviderChanged = "provider_changed"
	// refuseReasonSignInBusy: the per-person sign-in lock could not be taken in
	// time (lockLoginSupersede), so the capture was not serialized and is
	// refused rather than stored.
	refuseReasonSignInBusy = "signin_busy"
)

// DRAFT (M2 canon pending)

const (
	// ssoTokenAccountPinRefusal: the sign-in captured a different account/role
	// than the one this deployment pinned at launch. It names BOTH pairs because
	// the person reading it on a terminal is the one who has to pick again.
	ssoTokenAccountPinRefusal = "This sign-in captured account %s / role %s, but this deployment pins AWS sign-ins for this agent to account %s / role %s — sign in again and choose that account and role, or ask an admin to change the pin"
	// ssoTokenModelAccountRefusal: "Wardyn holds both halves — say so at
	// capture". Refused rather than warned: a wrong-account blob is what every
	// later run on its provider signs with the moment it is stored
	// (bedrockSSOAuth).
	ssoTokenModelAccountRefusal = "This session is for account %s; the configured Bedrock model lives in account %s — a run using this session would be refused by IAM, so it was not stored"
	// ssoTokenAccountShapeRefusal / ssoTokenRoleShapeRefusal: the capture named
	// an account or role that is not shaped like one. It names the SHAPE rather
	// than echoing the value back: the person reading it on the login terminal
	// picked from a portal list, so "that is not a 12-digit account" tells them
	// the pick did not resolve — and the value itself is already in their
	// terminal.
	ssoTokenAccountShapeRefusal = "This sign-in did not resolve to a 12-digit AWS account id, so nothing was stored — sign in again and choose an account from the list"
	// DRAFT (M2 canon pending)
	ssoTokenRoleShapeRefusal = "This sign-in did not resolve to an IAM role name, so nothing was stored — sign in again and choose a role from the list"
	// credentialConfinementAdvisorySentence (0.8 #150): the confinement-visibility
	// WARNING for a run whose model credential is a stored AWS SSO session
	// delivered to the sandbox at dispatch, under a confinement class weaker than
	// the CC3 floor such a credential would otherwise require — see
	// credentialConfinementAdvisory (runs_create.go). It names the enforced class
	// so the person reading it on the New Run rail or a preflight Review knows
	// exactly what "weaker" means for this run, and it is a WARNING, never a
	// refusal: a deployment offering only the weakest confinement class must
	// still be able to launch.
	credentialConfinementAdvisorySentence = "This run's model credential is a stored AWS SSO session, delivered to the sandbox at dispatch — it is not counted toward the confinement floor, and the enforced class %s is weaker than the Vault (CC3) floor a credential like this would otherwise require"
)

// awsAccountID matches an AWS account id. ONE var for the package: the ARN
// parser below and the provider's own pin validation (validateProviderPin) ask
// the same question about the same kind of value, and two copies of `^\d{12}$`
// is two places for it to drift.
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
// "" is a valid, COMMON ANSWER, and everything downstream must treat it as SKIP
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

// bindCaptureToPin is the upload's identity binding: does this blob name the
// account and role this capture was AUTHORIZED to produce?
//
// Two independent checks, both fail-closed, both against trusted server state:
//
//  1. The launch-time pin, read back off this run's own harness.login.start
//     row — never off the live provider. An edit mid-login must not re-point a
//     capture already in flight, for the same reason the credential scope is
//     stamped rather than re-resolved (see loginRunScope). An empty stamp pin
//     means "launched unpinned", which is accepted: the run was authorized
//     without one.
//  2. The provider's model account, when its model is a full ARN and the run
//     was launched UNPINNED. Wardyn holds both halves of this comparison
//     already — the blob's account_id and the account in the stamped model.
//     A PIN outranks it: it is the admin's deliberate answer to the same
//     question, and a resource-shared application inference profile
//     legitimately lives in another account, so a deployment with one had no
//     configuration that worked. Nothing widens — check 1 still binds the pair
//     the sandbox sends.
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
// A bare writeError would leave a refusal invisible in the trail: "a capture
// was refused" would be a thing only the person watching the login terminal
// ever knew. This is the audit row naming the change, for the change that
// did NOT happen.
//
// The DATA carries the reason from the fixed vocabulary above and nothing the
// sandbox chose: the sentence goes to the caller, never into the log.
//
// scope is the caller's credential scope, or NIL. It is non-nil for the
// refusals that happen AFTER loginRunScope has decided one; those rows then
// carry owner + credential_source exactly like the captured row — the pair that
// makes a per_user estate's refusal stream groupable by person instead of a
// join back through each row's run_id to its harness.login.start. The EARLIER
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
		"harness.credential.refuse", harnessCredSecretName(awsSSOProvider), "failure",
		mustJSON(data)))
	writeError(w, status, msg)
}
