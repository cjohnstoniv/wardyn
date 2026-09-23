// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// provider_access.go generalises the single hardcoded AWS-SSO-only grading in
// modelaccess.go (SetupModelAccess) to every model provider a person is
// granted (MP-12). It reuses that file's vocabulary and its Bedrock/subscription
// dispatch siblings' scope and read helpers (provider_bedrock.go,
// provider_subscription.go) rather than re-deriving them, so a state here and
// the refusal dispatch gives for the same credential can never disagree.
package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// DRAFT (M2 canon pending): the action sentences provider_access composes for
// the kinds SetupModelAccess's AWS-only vocabulary does not already cover.
const (
	providerAccessAddKeyAction   = "Add your key"
	providerAccessAddTokenAction = "Add your token"
	providerAccessSignInClaude   = "Sign in to Claude"
	// providerAccessClaudeAgingAction mirrors the Getting-started aging line
	// (multi-provider-design.md 5.4, row C10) byte for byte, since the console
	// may render it verbatim like every other composed action.
	providerAccessClaudeAgingAction = "Your Claude sign-in is over 11 months old and may stop working — sign in again."
)

// SetupProviderAccess is one provider's connection state for the caller — the
// wire shape provider_access: [{provider, state, action, deadline}] the design
// names. Member-safe by construction, the same discipline SetupModelProvider
// already follows: a provider id, one of the five SetupModelAccess states, an
// already-composed sentence, and (only when the state names one) a deadline
// instant. No secret name, no account pin, no start URL, no host.
type SetupProviderAccess struct {
	Provider string `json:"provider"`
	State    string `json:"state"`
	Action   string `json:"action,omitempty"`
	Deadline string `json:"deadline,omitempty"`
}

// setupModelProviderState is handleSetupStatus's one call site for both
// SetupStatus.ModelProviders and its ProviderAccess sibling: the second is
// graded per row of the first, so bundling them keeps the handler to one
// statement instead of two (its own funlen ratchet, setup.go's doc comment).
func (s *Server) setupModelProviderState(ctx context.Context, sc types.SiteConfig, owner string) ([]SetupModelProvider, []SetupProviderAccess) {
	mp := s.setupModelProviders(ctx, sc)
	return mp, s.setupProviderAccess(ctx, sc, mp, owner)
}

// setupProviderAccess is SetupStatus.ProviderAccess: one row per provider in
// providers (already filtered to this caller's launchable harnesses by
// setupModelProviders), graded against the caller's OWN credential for it.
// nil with no provider block, same as its input.
func (s *Server) setupProviderAccess(ctx context.Context, sc types.SiteConfig, providers []SetupModelProvider, owner string) []SetupProviderAccess {
	if len(providers) == 0 {
		return nil
	}
	out := make([]SetupProviderAccess, 0, len(providers))
	for _, sp := range providers {
		p, ok := modelProviderByID(sc.ModelProviders, sp.ID)
		if !ok {
			continue
		}
		out = append(out, s.providerAccessFor(ctx, p, owner))
	}
	return out
}

// providerAccessMechanism reports whether owner is the shared admin bearer
// token rather than a person — no credential of its own to grade for ANY
// provider kind, mirroring credentialOwner's / perPersonSubscriptionPosture's
// refusal for the write doors this state describes the read side of.
func (s *Server) providerAccessMechanism(owner string) bool {
	return owner == "" || (s.cfg.OIDC != nil && owner == adminTokenPrincipal)
}

// providerAccessFor grades one provider for owner. Every read is owner's own
// namespace only (ownSecret, readAWSSSOBlob under a per-user scope) — never
// the operator's — the same strict discipline dispatch itself applies before
// it ever wires a credential onto a run.
func (s *Server) providerAccessFor(ctx context.Context, p types.ModelProvider, owner string) SetupProviderAccess {
	row := SetupProviderAccess{Provider: p.ID}
	if s.providerAccessMechanism(owner) {
		row.State = modelAccessNotApplicable
		return row
	}
	switch p.Kind {
	case types.ModelProviderBedrockSSO:
		s.gradeProviderBedrockSSO(ctx, &row, p, owner)
	case types.ModelProviderAnthropicSubscription:
		s.gradeProviderSubscription(ctx, &row, p, owner)
	default:
		// Every other closed kind is a typed key or token: providerTypedKinds
		// (model_provider_credentials.go) — anthropic_api_key, openai_api_key,
		// bedrock_bearer, custom_endpoint — all stored under the one -key name
		// and graded by presence alone. "No key probe" (the design's own
		// words): dispatch is what finds out a stored key no longer works.
		s.gradeProviderKey(ctx, &row, p, owner)
	}
	return row
}

// gradeProviderKey grades a typed-key/token provider: stored -> live, absent
// -> not_configured. custom_endpoint says "token" throughout (5.4/5.6's own
// copy), every other typed kind says "key".
func (s *Server) gradeProviderKey(ctx context.Context, row *SetupProviderAccess, p types.ModelProvider, owner string) {
	raw, found, err := s.ownSecret(ctx, owner, providerSecretName(p.UID, providerKeyPart))
	// A store read failure grades exactly like "not configured": setup/status
	// degrades conservatively on a read failure throughout this file (see
	// onboardingComplete, hasRuns above) rather than turning one provider's
	// blip into a 500 for every row on the page.
	live := err == nil && found && len(raw) > 0
	addAction := providerAccessAddKeyAction
	if p.Kind == types.ModelProviderCustomEndpoint {
		addAction = providerAccessAddTokenAction
	}
	if live {
		row.State = modelAccessLive
		return
	}
	row.State = modelAccessNotConfigured
	row.Action = addAction
}

// gradeProviderSubscription grades a per-person Claude subscription: absent ->
// not_configured, present -> live unless its capture has crossed
// harnessTokenAging, the same conservative age heuristic SetupHarness.Aging
// already uses for the compose-mode managed token (no machine-readable expiry
// on a setup-token).
func (s *Server) gradeProviderSubscription(ctx context.Context, row *SetupProviderAccess, p types.ModelProvider, owner string) {
	raw, found, err := s.ownSecret(ctx, owner, providerSecretName(p.UID, providerOAuthPart))
	var blob managedCredBlob
	if err == nil && found && json.Unmarshal(raw, &blob) == nil && blob.Token != "" {
		if s.cfg.Now().UTC().Sub(blob.CapturedAt) > harnessTokenAging {
			row.State = modelAccessExpiring
			row.Action = providerAccessClaudeAgingAction
			return
		}
		row.State = modelAccessLive
		return
	}
	row.State = modelAccessNotConfigured
	row.Action = providerAccessSignInClaude
}

// gradeProviderBedrockSSO grades a captured AWS SSO session against a Bedrock
// SSO provider, reusing awsSSOCredentialState/modelAccessAction/
// modelAccessDeadline — SetupModelAccess's own vocabulary — over the
// provider-scoped read providerBedrockRefusal already reads from at dispatch.
// A session the provider's own account/role pin no longer allows grades
// expired_signin, mirroring setupModelAccess's roster-pin arm: a live,
// renewable session for the WRONG identity is not "live" from this person's
// seat, and dispatch would refuse it (mpBRPinned) the instant they tried.
func (s *Server) gradeProviderBedrockSSO(ctx context.Context, row *SetupProviderAccess, p types.ModelProvider, owner string) {
	scope := chosenProvider{provider: p, owner: owner}.awsScope()
	blob, found, err := s.readAWSSSOBlob(ctx, scope)
	if err != nil {
		// Conservative on a read failure, same discipline as gradeProviderKey.
		row.State = modelAccessNotConfigured
		row.Action = modelAccessSignInAction
		return
	}
	now := s.cfg.Now().UTC()
	spent := s.awsSSOTokenSpentFor(blob)
	row.State = awsSSOCredentialState(blob, found, true, spent, now)
	row.Deadline = modelAccessDeadline(blob, found, spent, now)
	row.Action = modelAccessAction(row.State, row.Deadline)
	if !found || p.Bedrock == nil {
		return
	}
	if stored, pinned, mismatch := providerPinContradiction(p, awsSSOPin{AccountID: blob.AccountID, RoleName: blob.RoleName}); mismatch {
		row.State = modelAccessExpiredSignin
		row.Deadline = ""
		row.Action = fmt.Sprintf(modelAccessPinContradictedAction, stored.AccountID, stored.RoleName, pinned.AccountID, pinned.RoleName)
	}
}

// providerPinContradiction is awsSSOPinContradiction's provider-scoped
// sibling: the pin a bedrock_sso provider names is ON THE PROVIDER RECORD
// (BedrockSettings.SSOAccountID/SSORoleName), never a roster row, so it
// cannot reuse that function's site-config lookup.
func providerPinContradiction(p types.ModelProvider, stored awsSSOPin) (awsSSOPin, awsSSOPin, bool) {
	if !stored.set() || p.Bedrock == nil {
		return awsSSOPin{}, awsSSOPin{}, false
	}
	pinned := awsSSOPin{AccountID: p.Bedrock.SSOAccountID, RoleName: p.Bedrock.SSORoleName}
	if !pinned.set() || stored == pinned {
		return awsSSOPin{}, awsSSOPin{}, false
	}
	return stored, pinned, true
}
