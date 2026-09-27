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

// BedrockWardynRunProvesPerUserSSO reports a descriptive error unless events
// (a completed run's own audit trail) proves the specific claim LL3w exists
// to check: the run's Bedrock call went out on the per-user AWS SSO lane
// (run.bedrock.configure's mode=="sso-inject-proxy" — the issue's own
// "sandbox SSO cache holds only the placeholder"; resolveBedrockAuth's other
// modes are "bearer", "sso-inject" without proxy injection, "aws-dir-mount"
// and "resident"), on a model this suite's own allow-list permits
// (bedrock.go's ModelAllowed), backed by an actual successful credential
// mint (credential.mint, outcome=success).
//
// It does NOT check that no credential.* row is attributed to a shared admin
// bearer: every credential.mint is written with Actor=the run's own SPIFFE
// identity (internal/api/internal.go) and every credential.revoke with
// Actor="wardyn-broker" (internal/broker/revoke.go) — neither can ever read
// as the admin-token sentinel, on ANY run of ANY kind, so that comparison
// could never fail and proved nothing. What DOES distinguish this run is the
// mode and model on its own run.bedrock.configure row, and that a mint for
// it actually succeeded — both checked here.
func BedrockWardynRunProvesPerUserSSO(events []types.AuditEvent) error {
	var configured *bedrockConfigureData
	var minted bool
	for _, e := range events {
		switch e.Action {
		case "run.bedrock.configure":
			var d bedrockConfigureData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return fmt.Errorf("run.bedrock.configure row %s: unreadable data: %w", e.ID, err)
			}
			configured = &d
		case "credential.mint":
			if e.Outcome == "success" {
				minted = true
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
	if !minted {
		return errors.New("no successful credential.mint row: the sandbox's kernel identity was never actually credentialed")
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
// sentence internal/api/bedrock_dataplane_fault.go's bedrockFaultHints writes
// to a run's failure_hint when AWS refuses the model call with the given
// error class ("AccessDeniedException" or "ThrottlingException") — Wardyn's
// own sentence, keyed by the AWS class the proxy's bedrockUpstreamFault
// classified, never AWS's raw response verbatim. class must be one of those
// two exact strings.
//
// This is observable ONLY on a lane the proxy actually MITMs bedrock-runtime
// on — the bearer (API-key) lane, or a plain-HTTP WARDYN_BEDROCK_BASE_URL
// test hatch. The per-user AWS SSO lane's bedrock-runtime traffic is an
// opaque, un-MITM'd tunnel (isMITMHost covers only the SSO portal host on
// that lane, not bedrock-runtime itself), so a denied Integration on THAT
// lane never produces this hint at all — see the live test's own mode check
// before it calls this.
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
