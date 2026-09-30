// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package testlive

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// bedrockConfigureData is the shape internal/api/runs_dispatch_llm.go's
// recordBedrockTransport marshals onto a run.bedrock.configure row's Data.
type bedrockConfigureData struct {
	Model string `json:"model"`
	Mode  string `json:"mode"`
}

// mintScopeData is the shape a credential.mint row's Data carries for the
// per-user AWS SSO grant: mintEvent puts the grant's Scope under "scope", and
// authorBedrockSSOInjection authors that scope with a "snapshot" naming who
// it was captured for and how (awsSSOScopeSnapshot).
type mintScopeData struct {
	Scope struct {
		SecretName string `json:"secret_name"`
		Snapshot   struct {
			OwnerSubject     string `json:"owner_subject"`
			CredentialSource string `json:"credential_source"`
		} `json:"snapshot"`
	} `json:"scope"`
}

// BedrockWardynRunProvesPerUserSSO reports a descriptive error unless events
// (a completed run's own audit trail) prove the run's Bedrock call went out
// on THIS MEMBER's own per-user AWS SSO capture, on an allow-listed model.
//
// "per-user" is proven on the credential.mint row itself, not on
// run.bedrock.configure's mode: mode=="sso-inject-proxy" is chosen from
// b.ssoInject && b.ssoProxyInject regardless of whether the roster row behind
// it is `per_user` or `shared` — an operator's shared captured session
// produces the exact same mode. What actually distinguishes them is the
// mint's own scope snapshot (authorBedrockSSOInjection): its secret_name is
// the AWS-SSO sentinel, its credential_source is CredentialSourcePerUser only
// for a per-user row, and its owner_subject is the subject the roster
// resolved the capture for. A mint whose snapshot says "shared", or whose
// owner is anyone but memberPrincipal, must be graded a failure here, or the
// proof is false.
//
// owner_subject is runIdentitySubject(ctx, run.CreatedBy) at dispatch,
// evaluated on the dispatcher's own detached context where
// localPrincipalFromContext is always empty outside local mode — so for the
// OIDC-backed deployment this suite targets, owner_subject is run.CreatedBy
// verbatim, the same "sub" GET /me's "principal" reports for the same token.
// Compared for exact equality here; an install where the two are not spelled
// the same is disclosed in docs/LIVE-TESTS.md rather than silently worked
// around.
func BedrockWardynRunProvesPerUserSSO(events []types.AuditEvent, memberPrincipal string) error {
	var configured *bedrockConfigureData
	var perUserMintForMember bool
	for _, e := range events {
		switch e.Action {
		case "run.bedrock.configure":
			var d bedrockConfigureData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return fmt.Errorf("run.bedrock.configure row %s: unreadable data: %w", e.ID, err)
			}
			configured = &d
		case "credential.mint":
			if e.Outcome != "success" {
				continue
			}
			var m mintScopeData
			if err := json.Unmarshal(e.Data, &m); err != nil {
				continue // not every credential.mint carries an SSO-shaped scope
			}
			if m.Scope.SecretName == string(types.AWSSSOAccessTokenSecret) &&
				m.Scope.Snapshot.CredentialSource == string(types.CredentialSourcePerUser) &&
				m.Scope.Snapshot.OwnerSubject == memberPrincipal {
				perUserMintForMember = true
			}
		}
	}
	if configured == nil {
		return errors.New("no run.bedrock.configure audit row: this run may never have used Bedrock at all")
	}
	if configured.Mode != "sso-inject-proxy" {
		return fmt.Errorf("run.bedrock.configure mode is not the per-user AWS SSO lane (sso-inject-proxy)")
	}
	if !ModelAllowed(configured.Model) {
		return fmt.Errorf("run.bedrock.configure model is not on the suite's allow-list (Claude Haiku 4.5, Amazon Nova Micro)")
	}
	if !perUserMintForMember {
		return errors.New("no successful credential.mint row proves a per-user AWS SSO capture owned by this member: " +
			"the run may have used the operator's SHARED captured session instead, a different grant, or never minted at all")
	}
	return nil
}

// RunCreatedByIsMember reports a descriptive error unless createdBy (the
// run's own AgentRun.CreatedBy — the human principal, token `sub`) is exactly
// memberPrincipal. An empty createdBy, or a match against anyone else
// (including an admin), fails it.
func RunCreatedByIsMember(createdBy, memberPrincipal string) error {
	if createdBy == "" {
		return errors.New("run has no created_by: not attributed to any human")
	}
	if createdBy != memberPrincipal {
		return errors.New("run created_by is not this member: attributed to someone else")
	}
	return nil
}

// TranscriptContainsReply reports a descriptive error unless transcript (a
// run's session recording, raw asciicast bytes — the JSON escaping of plain
// ASCII text leaves a literal substring intact) contains want, case
// insensitive.
func TranscriptContainsReply(transcript []byte, want string) error {
	if !strings.Contains(strings.ToLower(string(transcript)), strings.ToLower(want)) {
		return fmt.Errorf("transcript does not contain the expected reply %q", want)
	}
	return nil
}

// BedrockWardynForcedFaultOK reports a descriptive error unless hint is the
// sentence bedrockFaultHints writes to a run's failure_hint when AWS refuses
// the model call with the given error class ("AccessDeniedException" or
// "ThrottlingException") — Wardyn's own sentence, keyed by the AWS class the
// proxy's bedrockUpstreamFault classified, never AWS's raw response verbatim.
// class must be one of those two exact strings.
//
// Observable ONLY on a lane the proxy actually MITMs bedrock-runtime on — the
// bearer (API-key) lane, or a provider's plain-HTTP bedrock.base_url test hatch.
// The per-user AWS SSO lane's bedrock-runtime traffic is an opaque, un-MITM'd
// tunnel, so a denial on that lane never produces this hint at all — see the
// live test's own mode check before it calls this.
func BedrockWardynForcedFaultOK(hint, class string) error {
	switch class {
	case "AccessDeniedException", "ThrottlingException":
	default:
		return fmt.Errorf("BedrockWardynForcedFaultOK: unknown fault class %q", class)
	}
	if !strings.Contains(hint, class) {
		return fmt.Errorf("run failure_hint does not name %s: %q", class, hint)
	}
	return nil
}
