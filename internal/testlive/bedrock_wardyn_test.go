// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package testlive

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// bedrockConfigureEvent builds a run.bedrock.configure row with the given
// data, mirroring recordBedrockTransport's own marshalling.
func bedrockConfigureEvent(t *testing.T, mode, model string) types.AuditEvent {
	t.Helper()
	data, err := json.Marshal(bedrockConfigureData{Mode: mode, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	return types.AuditEvent{ID: uuid.New(), Action: "run.bedrock.configure", Data: data}
}

// ssoMintEvent builds a successful (or not) credential.mint row for the
// per-user AWS SSO grant, mirroring authorBedrockSSOInjection's own scope
// shape: {"scope":{"secret_name":"aws-sso-access-token","snapshot":
// {"owner_subject":...,"credential_source":"per_user"|"shared"}}}.
func ssoMintEvent(t *testing.T, outcome, credentialSource, ownerSubject string) types.AuditEvent {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"scope": map[string]any{
			"secret_name": string(types.AWSSSOAccessTokenSecret),
			"snapshot": map[string]any{
				"owner_subject":     ownerSubject,
				"credential_source": credentialSource,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return types.AuditEvent{ID: uuid.New(), ActorType: types.ActorAgent, Action: "credential.mint", Outcome: outcome, Data: data}
}

// adoEntraMintEvent builds a successful credential.mint row for a DIFFERENT
// grant entirely: the per-user Azure DevOps Entra token
// (adoEntraScopeSnapshot, internal/api/runs_dispatch_ado_inject.go), which
// carries the SAME two snapshot fields (owner_subject, credential_source)
// the AWS SSO grant does, under secret_name
// types.ADOEntraAccessTokenSecret — a REALISTIC collision, not a
// github-token stub with no snapshot at all (round 3's review: a snapshot-
// less fixture left the secret_name clause unpinned, since the
// credential_source clause alone was enough to reject it either way).
func adoEntraMintEvent(t *testing.T, ownerSubject string) types.AuditEvent {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"scope": map[string]any{
			"secret_name": string(types.ADOEntraAccessTokenSecret),
			"snapshot": map[string]any{
				"owner_subject":     ownerSubject,
				"credential_source": string(types.CredentialSourcePerUser),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return types.AuditEvent{ID: uuid.New(), ActorType: types.ActorAgent, Action: "credential.mint", Outcome: "success", Data: data}
}

const bedrockWardynTestMember = "member-sub"

// TestBedrockWardynRunProvesPerUserSSO pins the LL3w audit check
// hermetically: the per-user SSO lane, an allow-listed model and a
// successful PER-USER mint owned by the member together pass; any one of
// them missing must fail — including the case round 2's review named (a
// mint whose scope snapshot says "shared") and the case round 3's review
// named (a REALISTIC other grant — a per-user Azure DevOps Entra mint for
// the SAME member — that must not be mistaken for the AWS SSO one just
// because it carries the same owner_subject/credential_source shape; a
// snapshot-less "different grant" fixture would leave the secret_name clause
// unpinned).
func TestBedrockWardynRunProvesPerUserSSO(t *testing.T) {
	good := []types.AuditEvent{
		bedrockConfigureEvent(t, "sso-inject-proxy", "us.anthropic.claude-haiku-4-5-20251001-v1:0"),
		ssoMintEvent(t, "success", "per_user", bedrockWardynTestMember),
	}
	if err := BedrockWardynRunProvesPerUserSSO(good, bedrockWardynTestMember); err != nil {
		t.Fatalf("a genuine per-user SSO run was rejected: %v", err)
	}

	for _, tc := range []struct {
		name   string
		events []types.AuditEvent
	}{
		{"no bedrock.configure row at all", []types.AuditEvent{ssoMintEvent(t, "success", "per_user", bedrockWardynTestMember)}},
		{"wrong lane (bearer, not sso-inject-proxy)", []types.AuditEvent{
			bedrockConfigureEvent(t, "bearer", "us.anthropic.claude-haiku-4-5-20251001-v1:0"),
			ssoMintEvent(t, "success", "per_user", bedrockWardynTestMember),
		}},
		{"model off the allow-list", []types.AuditEvent{
			bedrockConfigureEvent(t, "sso-inject-proxy", "anthropic.claude-opus-4-1-20250805-v1:0"),
			ssoMintEvent(t, "success", "per_user", bedrockWardynTestMember),
		}},
		{"SHARED scope (the operator's own captured session, not this member's)", []types.AuditEvent{
			bedrockConfigureEvent(t, "sso-inject-proxy", "us.anthropic.claude-haiku-4-5-20251001-v1:0"),
			ssoMintEvent(t, "success", "shared", bedrockWardynTestMember),
		}},
		{"per_user but owned by someone else", []types.AuditEvent{
			bedrockConfigureEvent(t, "sso-inject-proxy", "us.anthropic.claude-haiku-4-5-20251001-v1:0"),
			ssoMintEvent(t, "success", "per_user", "someone-else-sub"),
		}},
		{"a credential row exists but it's a different grant entirely (per-user Azure DevOps Entra token, same owner)", []types.AuditEvent{
			bedrockConfigureEvent(t, "sso-inject-proxy", "us.anthropic.claude-haiku-4-5-20251001-v1:0"),
			adoEntraMintEvent(t, bedrockWardynTestMember),
		}},
		{"SHARED SSO mint alongside a per-user Azure DevOps mint for the same member: still not per-user SSO", []types.AuditEvent{
			bedrockConfigureEvent(t, "sso-inject-proxy", "us.anthropic.claude-haiku-4-5-20251001-v1:0"),
			ssoMintEvent(t, "success", "shared", bedrockWardynTestMember),
			adoEntraMintEvent(t, bedrockWardynTestMember),
		}},
		{"the per-user SSO mint exists but never succeeded", []types.AuditEvent{
			bedrockConfigureEvent(t, "sso-inject-proxy", "us.anthropic.claude-haiku-4-5-20251001-v1:0"),
			ssoMintEvent(t, "denied", "per_user", bedrockWardynTestMember),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := BedrockWardynRunProvesPerUserSSO(tc.events, bedrockWardynTestMember); err == nil {
				t.Fatalf("events %+v were accepted", tc.events)
			}
		})
	}
}

func TestRunCreatedByIsMember(t *testing.T) {
	if err := RunCreatedByIsMember("member-sub", "member-sub"); err != nil {
		t.Fatalf("a matching created_by was rejected: %v", err)
	}
	for _, tc := range []struct{ createdBy, want string }{
		{"", "member-sub"},
		{"admin-token", "member-sub"},
		{"someone-else-sub", "member-sub"},
	} {
		if err := RunCreatedByIsMember(tc.createdBy, tc.want); err == nil {
			t.Fatalf("created_by %q against member %q was accepted", tc.createdBy, tc.want)
		}
	}
}

func TestTranscriptContainsReply(t *testing.T) {
	transcript := []byte(`[1.0, "o", "the model said: PONG\r\n"]`)
	if err := TranscriptContainsReply(transcript, "pong"); err != nil {
		t.Fatalf("a transcript carrying the reply was rejected: %v", err)
	}
	if err := TranscriptContainsReply(transcript, "banana"); err == nil {
		t.Fatalf("a transcript NOT carrying the reply was accepted")
	}
	if err := TranscriptContainsReply(nil, "pong"); err == nil {
		t.Fatalf("an empty transcript was accepted")
	}
}

// TestBedrockWardynForcedFaultOK pins the forced-fault check: the run's
// failure_hint must name the exact AWS error class asked for.
func TestBedrockWardynForcedFaultOK(t *testing.T) {
	const denyHint = "Amazon Bedrock refused the model call (AccessDeniedException): a policy denies it — " +
		"an AWS Organizations service control policy or an IAM policy on the role"
	if err := BedrockWardynForcedFaultOK(denyHint, "AccessDeniedException"); err != nil {
		t.Fatalf("a genuine AccessDeniedException hint was rejected: %v", err)
	}

	for _, tc := range []struct{ name, hint, class string }{
		{"empty hint", "", "AccessDeniedException"},
		{"wrong class in hint", "Amazon Bedrock is throttling this model (ThrottlingException)", "AccessDeniedException"},
		{"unknown class asked for", denyHint, "SomeOtherException"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := BedrockWardynForcedFaultOK(tc.hint, tc.class); err == nil {
				t.Fatalf("hint %q against class %q was accepted", tc.hint, tc.class)
			}
		})
	}
}
