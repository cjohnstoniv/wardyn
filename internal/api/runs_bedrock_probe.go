// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"os"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// SetupBedrock is the Amazon Bedrock Anthropic-transport readiness snapshot the
// wizard renders. This file is the PROBE half of the Bedrock lane and
// runs_bedrock.go is the RESOLVE half, split only because the one file hit the
// size gate; they are still one seam, because ready() must accept exactly the
// credential set resolveBedrockAuth accepts: a drift
// between them (readiness omitting a credential lane resolveBedrockAuth
// accepts) would tell an operator whose runs authenticate fine that no
// integration could drive Claude Code. Keep the two lists edited together.
type SetupBedrock struct {
	Region string `json:"region,omitempty"`
	Model  string `json:"model,omitempty"`
	// The four credential SOURCES resolveBedrockAuth accepts, in its precedence
	// order (bearer > captured AWS SSO session > ~/.aws mount > resident SigV4).
	// ANY one is sufficient — a mount-, bearer- or SSO-credentialed host has NO
	// aws-access-key-id/-secret secrets yet is fully ready, so gating readiness on
	// CredsPresent alone wrongly reads "needs setup".
	CredsPresent  bool `json:"creds_present"`  // resident aws-access-key-id + aws-secret-access-key secrets
	AWSMount      bool `json:"aws_mount"`      // host-mode read-only ~/.aws bind-mount (SSO auto-refreshes)
	BearerPresent bool `json:"bearer_present"` // bedrock-api-key bearer token secret (never resident)
	// SSOPresent: a captured container-login AWS SSO session that a run would
	// actually authenticate with — live, OR expired-but-renewable (dispatch
	// renews it). Only a session nothing can heal (no refresh token, or a lapsed
	// client registration) reads false.
	SSOPresent bool `json:"sso_present"`
	// SSOExpired: a session IS captured and nothing can heal it (no refresh
	// token, or a lapsed registration). NOT on the wire, deliberately: it exists
	// so the capability matrix can say "the credential expired" instead of "no
	// credential is configured" — two different things an operator fixes two
	// different ways — and nothing on the console reads a second copy of a fact
	// SetupStatus.ModelAccess already carries per principal.
	SSOExpired bool `json:"-"`
	// SSOAccountID/SSORoleName are the AWS account and IAM role the CALLER's own
	// captured session names. IN-PROCESS only (json:"-"), for the same reason
	// SSOExpired is: nothing on the console renders a second copy of an identity
	// SetupStatus.ModelAccess already speaks for per principal. They exist so
	// bedrockProviderCheck can say, on the ADMIN's own Bedrock row, that a
	// stored capture disagrees with the roster pin — the posture that is
	// otherwise audible only as somebody else's refused run.
	SSOAccountID, SSORoleName string `json:"-"`
	// PerUser/Mechanism: the CALLER's own awsSSOScope, echoed in-process (never
	// on the wire — bedrockProviderCheck's callsite already has bedrock in
	// hand, so no second SetupStatus consumer needs to re-derive it) so
	// bedrockProviderCheck/llmProviderCheck can tell "a real person's own
	// credential is missing" from "the shared admin token was asked a
	// per-person question" without re-resolving scope themselves.
	PerUser bool `json:"-"`
	// Mechanism: this caller IS the shared admin bearer token under a per_user
	// row — see awsSSOScopeIsMechanism. Always false when PerUser is false.
	Mechanism bool `json:"-"`
	// PerUserBearer: the roster row's own MECHANISM (types.AgentMechanism) is
	// bedrock_bearer rather than bedrock_sso, under a per_user row. Meaningless
	// (always false) when PerUser is false. IN-PROCESS only (json:"-"), for the
	// same reason PerUser/Mechanism are: it exists so bedrockProviderRow can
	// tell an SSO caller from a bearer caller and pick the sentence that names
	// the ACTUAL remedy — signing in to AWS is not it for a bearer row; storing
	// a bedrock-api-key of their own is (#153, #320).
	PerUserBearer bool `json:"-"`
	// Ready is the server-computed readiness (region+model+any credential source),
	// echoed so the UI doesn't re-derive — and drift from — this gate.
	Ready bool `json:"ready"`
}

// ready reports whether a claude-code run would actually get the Bedrock
// transport right now — mirrors resolveBedrockAuth's gate: region + model AND at
// least one credential source (a bearer token, a captured AWS SSO session, a
// ~/.aws mount, or resident keys). Presence, not value, is enough here (no live
// secret-store read) — except for the SSO session, which honours the SAME
// predicate resolveBedrockAuth's captured-SSO branch uses (renewable ||
// !expired), so the wizard and the launch gate cannot disagree about whether an
// expired-but-renewable session counts.
func (b SetupBedrock) ready() bool {
	return b.Region != "" && b.Model != "" &&
		(b.CredsPresent || b.AWSMount || b.BearerPresent || b.SSOPresent)
}

// configured reports whether the operator has touched ANY Bedrock knob (region,
// model, or a credential source that is Bedrock's alone) — used to decide
// whether the bedrock_provider check is worth showing at all vs. staying silent
// for the overwhelming majority of operators who never use Bedrock. SSOPresent
// is deliberately NOT a term: a container AWS SSO login on its own says nothing
// about wanting Bedrock, and ready() already implies configured() through Region.
func (b SetupBedrock) configured() bool {
	return b.Region != "" || b.Model != "" || b.CredsPresent || b.AWSMount || b.BearerPresent
}

// credSourceDesc names the winning credential source (resolveBedrockAuth's
// precedence) for honest UI copy — "resident keys" is wrong for a mount/bearer host.
func (b SetupBedrock) credSourceDesc() string {
	switch {
	case b.BearerPresent:
		return "a proxy-injected Bedrock API key (never resident in the sandbox)"
	case b.SSOPresent:
		return credSourceSSODesc
	case b.AWSMount:
		return "your host AWS credentials via a read-only ~/.aws mount (SSO auto-refreshes)"
	default:
		return "resident AWS SigV4 credentials"
	}
}

// setupBedrock reports Bedrock readiness: region/model are boot-time config
// (non-secret, safe to echo to the UI) and each credential flag mirrors the
// matching resolveBedrockAuth branch — presence, never the value. AWSMount
// mirrors its opt-in host-mode path: BedrockAWSConfigDir set AND the dir still
// exists (stat it, so a since-deleted ~/.aws doesn't read ready). SSOPresent
// mirrors its captured-SSO branch, which is why this needs a ctx: the blob is a
// secret-store read, and an expired one is not a credential there either.
//
// sso is the CALLER'S scope, threaded for the reason the resolve threads it:
// under a per_user row the operator's blob is not this caller's credential, and
// reading it here would report a member's Bedrock lane ready off somebody else's
// session — the wizard/launch-gate drift this function's doc opens with, in its
// per-principal form.
//
// sc is the site config the caller already read (setupStatusSSOScope's own
// callers all hold one) — read ONLY to name the roster row's own MECHANISM
// (bedrock_sso vs bedrock_bearer) for PerUserBearer below; every other field
// here is unchanged by it. A caller under the operator scope (sso.perUser ==
// false) never consults sc, so passing its zero value costs it nothing.
func (s *Server) setupBedrock(ctx context.Context, present map[string]bool, sc types.SiteConfig, sso awsSSOScope) SetupBedrock {
	awsMount := false
	if s.cfg.BedrockAWSConfigDir != "" {
		st, err := os.Stat(s.cfg.BedrockAWSConfigDir)
		awsMount = err == nil && st.IsDir()
	}
	// The SAME predicate resolveBedrockAuth's captured-SSO branch applies, for the
	// reason named on SetupBedrock.ready: an expired-but-renewable session IS a
	// credential (dispatch renews it), so reporting it dead here would tell an
	// operator to re-login hourly for a credential that heals itself — and, once
	// a declared mechanism can refuse a run, would refuse a run dispatch heals.
	// Reading NO refresh is done here: this is a read-only probe.
	//
	// renewable(now) is not enough on its own: a
	// refresh token AWS has already retired still reads renewable() == true
	// (a refresh token is PRESENT and the registration has not lapsed) even
	// though redeeming it will fail every time — awsSSOTokenSpentFor is the
	// same spent-set consult setupModelAccess grades against.
	ssoLive, ssoDead := false, false
	ssoAccount, ssoRole := "", ""
	if blob, found, err := s.readAWSSSOBlob(ctx, sso); err == nil && found {
		now := s.cfg.Now()
		ssoLive = (blob.renewable(now) && !s.awsSSOTokenSpentFor(blob)) || !blob.expired(now)
		ssoDead = !ssoLive
		ssoAccount, ssoRole = blob.AccountID, blob.RoleName
	}
	b := SetupBedrock{
		Region:        s.cfg.BedrockRegion,
		Model:         s.cfg.BedrockModel,
		CredsPresent:  present[bedrockAccessKeyIDSecret] && present[bedrockSecretAccessKeySecret],
		AWSMount:      awsMount,
		BearerPresent: present[bedrockAPIKeySecret],
		SSOPresent:    ssoLive,
		SSOExpired:    ssoDead,
		SSOAccountID:  ssoAccount,
		SSORoleName:   ssoRole,
	}
	// Under per_user the OPERATOR lanes are not this caller's credential either
	// (resolveBedrockAuth skips all three), so reporting them would read "ready"
	// for a member whose own session is the only thing that can carry their runs.
	//
	// C4.2 — the asymmetry this creates, stated on purpose so the Agents-tab copy
	// can say it: an ADMIN who declares per_user and has not signed in themselves
	// reads NOT-READY on their own setup page while GET /integrations still shows
	// Bedrock credentialed. Both are true of different questions. This one asks
	// "would MY run authenticate" — and under per_user the admin is a principal
	// like any other, which is the whole point of the declaration; /integrations
	// asks "what connections does this deployment have", and the operator's
	// bearer key and static keys are still connections it has.
	if sso.perUser {
		b.CredsPresent, b.AWSMount = false, false
		// The bearer is the ONE operator lane with a per-principal twin: a member
		// may store a bedrock-api-key of their own, and resolveBedrockAuth reads it
		// from their namespace. So this reports the CALLER's own row, never the
		// operator's — reporting the operator's would read "ready" off a credential
		// the resolve refuses to serve them, and reporting nothing would read "not
		// configured" over a bearer their runs really authenticate with.
		b.BearerPresent = len(s.bedrockBearerFor(ctx, sso)) > 0
	}
	b.PerUser = sso.perUser
	b.Mechanism = awsSSOScopeIsMechanism(sso)
	if sso.perUser {
		if row, ok := agentProviderFor(sc, modelAccessAgent); ok {
			b.PerUserBearer = row.Mechanism == types.AgentMechanismBedrockBearer
		}
	}
	b.Ready = b.ready()
	return b
}
