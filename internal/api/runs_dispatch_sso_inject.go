// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Phase b — the captured AWS SSO session, injected proxy-side instead of
// written into the sandbox.
//
// `portal.sso.<region>` GetRoleCredentials is `authtype:none` (unsigned), so a
// MITM can set the `x-amz-sso_bearer_token` header without the sandbox ever
// holding the token — exactly the Bedrock BEARER shape one file over
// (authorBedrockBearerInjection, runs_dispatch_llm.go), which this is a copy of.
// resolveBedrockAuth stages an inert placeholder in the sandbox's token cache;
// this file authors the grant and the MITM entry that make the wire work.
//
// Everything security-relevant about the lane is decided HERE, at dispatch,
// because dispatch is the only moment at which the run's credential scope is
// still the one the operator's roster meant. See the snapshot below.

// awsSSOProxyInjectDefaultOn is the DEFAULT of the kill switch
// WARDYN_AWS_SSO_PROXY_INJECT.
//
// This constant is the flip. Shipping the lane off by default is one edit here
// and nothing else: every other site reads Config.AWSSSOProxyInject, which
// ResolveAWSSSOProxyInject derives from this. `off` restores the pre-lane
// behavior byte for byte for NEW dispatches — a run already dispatched keeps the lane it was
// authored with (its cache file, its grant and its MITM entry) until it ends,
// which is why the operator's rollback step is "flip it and relaunch the runs
// that matter", not "flip it and the fleet changes under you".
const awsSSOProxyInjectDefaultOn = true

// ResolveAWSSSOProxyInject reads the kill switch. Exported for cmd/wardynd,
// which owns every env read.
//
// A value nobody typed (empty, or anything that is not one of the two words) is
// the DEFAULT rather than a boot refusal: this switch's whole purpose is to be
// reachable in a hurry, and a deployment that mistypes it should get the
// documented default plus an ENV.md row to read, not a crash-looping daemon in
// the middle of an incident. The two words are spelled `on` and `off` — the
// ENV.md convention every other Wardyn mode switch uses — and `true`/`false`
// are accepted because an operator reaching for a boolean is not wrong enough
// to be refused.
func ResolveAWSSSOProxyInject(raw string) bool {
	return resolveAWSSSOProxyInject(raw, awsSSOProxyInjectDefaultOn)
}

// resolveAWSSSOProxyInject takes the default as a PARAMETER so a test can drive
// both positions of the switch without rebuilding the binary — the constant
// above is the ship position, and this is the seam that proves it is the only
// thing that decides.
func resolveAWSSSOProxyInject(raw string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "off", "false", "0":
		return false
	case "on", "true", "1":
		return true
	default:
		return def
	}
}

// AWSSSOProxyInjectFlagDefault is the DEFAULT the boot flag advertises, derived
// from the one constant rather than typed a second time.
//
// It exists because the "one-line flip" was a LIE without it:
// cmd/wardynd's flag hard-coded the string "on", so with nothing set in the
// environment the resolver was handed "on" and never consulted the constant at
// all — flipping awsSSOProxyInjectDefaultOn to false would have shipped the
// lane ON with every document saying off. The flag now advertises this, so the
// constant decides the unset case, the -h text and the ENV.md default together.
func AWSSSOProxyInjectFlagDefault() string {
	if awsSSOProxyInjectDefaultOn {
		return "on"
	}
	return "off"
}

// awsSSOInjectHeader is the ONE header the SSO access token rides, and the ONE
// AWS defines for the call: GetRoleCredentials takes the session as
// `x-amz-sso_bearer_token`, not as an Authorization bearer.
const awsSSOInjectHeader = "x-amz-sso_bearer_token"

// awsSSOGrantTTLSeconds matches the Bedrock bearer precedent
// (runs_dispatch_llm.go). It bounds the MINT, never the hold: a re-resolve
// mints afresh, and the credential the mint resolves to is the live blob.
const awsSSOGrantTTLSeconds = 3600

// awsSSOScopeSnapshot is the IMMUTABLE credential scope this run was dispatched
// with — invariant I3, and the fix for a real substitution hole.
//
// awsSSOScopeFor derives the owner from the ROSTER ROW at call time. A resolve
// happens MID-RUN, minutes or hours after dispatch, so an admin who flips the
// row from per_user to shared (or disables it) while a run is held would have
// that run's next resolve read the OPERATOR's blob and inject it — a credential
// silently changing principal under a running agent, with every existing guard
// green, because every existing guard asks about the run and not about the
// credential's identity.
//
// So the grant carries this snapshot, authored here, and resolveAWSSSOInjection
// re-derives the scope from the live roster and requires EQUALITY on every
// field. Drift is a 403, fail closed, never a different credential. The general
// rule it makes concrete: a recovery event may refresh credential MATERIAL; it
// must never re-authorize the operation against a different principal, source,
// mechanism, account, role, region or host.
//
// It is a SNAPSHOT of facts, not a capability: naming a field here grants
// nothing, because every field must still equal what the roster says at resolve
// time and the per_user owner must still equal the run token's own claims.Sub.
type awsSSOScopeSnapshot struct {
	OwnerSubject     string `json:"owner_subject"`
	CredentialSource string `json:"credential_source"`
	Mechanism        string `json:"mechanism"`
	SSOAccountID     string `json:"sso_account_id"`
	SSORoleName      string `json:"sso_role_name"`
	Region           string `json:"region"`
}

// ssoPortalMITMEntry is the ONE spelling of a Phase-B TLS-MITM entry, and the
// only place the upstream SCHEME is decided.
//
// Bare "host:port" — today's format, byte-for-byte — for a real portal, which
// is every deployment that has not set WARDYN_AWS_SSO_ENDPOINT_OVERRIDE. An
// "http://" prefix only when the operator pointed the override at a PLAIN-HTTP
// server, which is the SSO fake and already refuses to boot without
// WARDYN_ALLOW_TEST_ENDPOINTS.
//
// The prefix is not cosmetic: the proxy TERMINATES this tunnel because Phase B's
// sandbox holds a placeholder rather than a credential, so the request has to be
// visible for the real token to be substituted. Terminating and then dialling
// the origin in TLS — which is what a hard-coded https did — means every CONNECT
// to a plain-HTTP portal ends in builtin:dial-failed and a 502, so no role
// credentials ever reach the sandbox and every model call starves. NOT
// terminating is not the alternative: a blind tunnel carries the placeholder
// through untouched, which is Phase B not happening at all.
func ssoPortalMITMEntry(ssoRegion, endpointOverride string) string {
	entry := net.JoinHostPort(ssoPortalHost(ssoRegion, endpointOverride), ssoPortalPort(endpointOverride))
	if u, err := url.Parse(endpointOverride); err == nil && strings.EqualFold(u.Scheme, "http") {
		return "http://" + entry
	}
	return entry
}

// authorBedrockSSOInjection authors the captured-AWS-SSO injection: an api_key
// grant whose x-amz-sso_bearer_token header injects the run's own SSO access
// token into that run's own portal.sso host, and marks that host TLS-MITM
// eligible for THIS run. Returns the updated injections and the MITM host list;
// ok=false means the grant write failed, the run was marked FAILED (CAS from
// STARTING), and dispatch must stop — authorBedrockBearerInjection's contract,
// deliberately identical.
//
// The injection SCOPE's host is BARE (buildInjector keys byHost verbatim); the
// MITM-eligibility entry is host:PORT (net.JoinHostPort), because a bare MITM
// entry is ANY-PORT in the proxy and an agent that can reach the portal host at
// all could otherwise CONNECT to it on a port nobody configured and have that
// tunnel terminated with the Wardyn leaf and the operator's SSO session injected
// onto whatever answered there.
func (s *Server) authorBedrockSSOInjection(ctx context.Context, run types.AgentRun, t llmTransport,
	sso awsSSOScope, injections []runner.InjectionGrant,
) ([]runner.InjectionGrant, []string, bool) {
	portalHost := ssoPortalHost(t.bedrock.ssoRegion, s.cfg.AWSSSOEndpointOverride)
	// The port the run actually reaches, not a hard-coded 443.
	// ssoEgressHosts already honours the override's port, so an override on any
	// other port was allowlisted and MITM-eligible at 443 only: the CONNECT was
	// never terminated, the header was never injected, and require_tls was off
	// there too — a blind tunnel carrying a placeholder to a 401. That is the
	// shape the plan's TLS-fake test needs.
	mitmHosts := []string{ssoPortalMITMEntry(t.bedrock.ssoRegion, s.cfg.AWSSSOEndpointOverride)}
	scope, _ := json.Marshal(map[string]any{
		"host":        portalHost,
		"header":      awsSSOInjectHeader,
		"format":      "%s",
		"secret_name": types.AWSSSOAccessTokenSecret,
		// I3, authored at dispatch. Every field resolveAWSSSOInjection compares.
		"snapshot": awsSSOScopeSnapshot{
			OwnerSubject:     sso.owner,
			CredentialSource: awsSSOCredentialSourceLabel(sso),
			Mechanism:        string(types.AgentMechanismBedrockSSO),
			SSOAccountID:     t.bedrock.ssoAccountID,
			SSORoleName:      t.bedrock.ssoRoleName,
			Region:           t.bedrock.ssoRegion,
		},
		// Production is TLS-only. The single exception is the deployment that
		// already refused to boot without WARDYN_ALLOW_TEST_ENDPOINTS: the
		// plain-http SSO fake, which serves no TLS at all. Leaving require_tls on
		// there would refuse the walk's own requests; leaving it OFF anywhere
		// else would let a cleartext CONNECT carry the operator's session.
		//
		// require_tls (rather than merely withholding the credential on
		// cleartext) is what makes a silently non-injecting proxy a REFUSAL
		// carrying Wardyn's own sentence instead of a bare AWS 401 the person
		// cannot act on.
		"require_tls": s.cfg.AWSSSOEndpointOverride == "",
		// The path pin. The injected session may ride exactly ONE
		// request: the GetRoleCredentials for the account and role this run was
		// dispatched with. Everything else the sandbox sends to the portal host —
		// `POST /logout`, which AWS documents as invalidating the owner's
		// server-side sign-in session for every run they have; a
		// GetRoleCredentials naming some other account or role the session holds;
		// /assignment/* — is forwarded WITHOUT the header and answered by AWS as
		// an unauthenticated call. Nothing Wardyn holds is exposed either way.
		//
		// A sandbox-resident token cannot do this: its reach would be the
		// agent's. Proxy-side injection is the first point at which
		// the admin-asserted pair becomes an ENFORCED one.
		//
		// The pin is always on. A blob carrying neither field cannot leave the
		// rule unpinned: the upload refuses a blob missing either
		// (awsSSOBlob.missingFields, ssotoken.go — account_id and role_name are
		// required fields, and a short capture is refused as blob_shape), so a
		// STORED session always carries the pair and awsSSOPinQuery always
		// answers one. pin_path is authored unconditionally besides, and
		// InjectionRule.Pinned() reads PinPath alone — so the rule is pinned
		// whatever the query turns out to be. The nil arm in awsSSOPinQuery is a
		// fail-safe for a shape the upload door does not admit, not a supported
		// configuration.
		//
		// The pair is the SNAPSHOT's, which is the same blob the sandbox's own
		// ~/.aws/config is generated from (awsSSOConfigFileContents, one call
		// apart), so it is byte-for-byte what the SDK will ask for — on a roster
		// row that pins account/role AND on one that does not.
		"pin_path":  awsSSORoleCredentialsPath,
		"pin_query": awsSSOPinQuery(t.bedrock.ssoAccountID, t.bedrock.ssoRoleName),
	})
	grantID := uuid.New()
	if _, gerr := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
		ID: grantID, RunID: run.ID, CreatedAt: time.Now(),
		Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: scope, TTLSeconds: awsSSOGrantTTLSeconds},
	}); gerr != nil {
		// CAS from STARTING (claimed at dispatch entry) so a concurrent kill's
		// KILLED state is preserved rather than clobbered back to FAILED.
		// The hint is member-visible: a fixed sentence, never the store's text.
		slog.ErrorContext(ctx, "wardynd: could not record the AWS SSO credential grant",
			slog.String("run_id", run.ID.String()), slog.Any("err", gerr))
		s.failAndRevoke(ctx, run.ID, types.RunStarting, "could not record the AWS SSO credential grant")
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"error": "aws sso inject grant: " + gerr.Error()})))
		return injections, nil, false
	}
	if rule, derr := injectionRuleFromScope(scope); derr == nil {
		injections = append(injections, runner.InjectionGrant{GrantID: grantID, Rule: rule})
	}
	return injections, mitmHosts, true
}

// awsSSORoleCredentialsPath is the ONE portal path a Phase-B run's session is
// allowed on: the AWS SSO portal's GetRoleCredentials.
const awsSSORoleCredentialsPath = "/federation/credentials"

// awsSSOPinQuery is the query the pinned request must carry.
//
// Nil is UNREACHABLE for a stored session — the upload requires account_id and
// role_name (awsSSOBlob.missingFields) — and it is here as a fail-safe rather
// than as a configuration: pinning to an EMPTY pair would be a pin nothing could
// ever match, withholding the credential from the one call that needs it. A
// nil query still leaves pin_path set, so the rule stays pinned to
// GET /federation/credentials and every other path is still refused the header.
func awsSSOPinQuery(accountID, roleName string) map[string]string {
	if accountID == "" || roleName == "" {
		return nil
	}
	return map[string]string{"account_id": accountID, "role_name": roleName}
}
