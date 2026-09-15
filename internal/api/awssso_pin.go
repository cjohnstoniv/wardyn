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
func (hl harnessLogin) loginEnv(ssoStartURL, ssoRegion string, pin awsSSOPin) map[string]string {
	env := hl.loginConfigEnv(ssoStartURL, ssoRegion)
	pinEnv := awsSSOPinEnv(pin)
	if len(pinEnv) == 0 {
		return env
	}
	if env == nil {
		env = map[string]string{}
	}
	maps.Copy(env, pinEnv)
	return env
}

// The FIXED reason vocabulary on a harness.credential.refused row. A closed set
// on purpose: the alternative is sandbox-chosen text in the audit trail, and an
// incident review filtering "why were captures refused last Tuesday" needs to
// GROUP, which free text cannot do.
const (
	refuseReasonBlobShape        = "blob_shape"
	refuseReasonFieldUnsafe      = "field_unsafe"
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
//  2. THE CONFIGURED MODEL'S ACCOUNT, when the operator gave a full ARN. Wardyn
//     holds both halves of this comparison already — the blob's account_id and
//     the account in WARDYN_BEDROCK_MODEL — and saying so at capture is ask 3
//     of the finding.
//
// Returns ("", "") to accept; (sentence, reason) to refuse.
func bindCaptureToPin(blob awsSSOBlob, stamp loginRunStamp, model string) (msg, reason string) {
	pin := awsSSOPin{AccountID: stamp.SSOAccountID, RoleName: stamp.SSORoleName}
	if pin.set() && (blob.AccountID != pin.AccountID || blob.RoleName != pin.RoleName) {
		return fmt.Sprintf(ssoTokenAccountPinRefusal,
			blob.AccountID, blob.RoleName, pin.AccountID, pin.RoleName), refuseReasonAccountRolePin
	}
	if modelAccount := bedrockModelAccount(model); modelAccount != "" && blob.AccountID != modelAccount {
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
func (s *Server) refuseCapture(w http.ResponseWriter, r *http.Request, claims *identity.Claims, status int, reason, msg string) {
	s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"harness.credential.refused", harnessCredSecretName(awsSSOProvider), "failure",
		mustJSON(map[string]any{"provider": awsSSOProvider, "reason": reason})))
	writeError(w, status, msg)
}
