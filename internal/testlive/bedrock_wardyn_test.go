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

func mintEvent(outcome string) types.AuditEvent {
	return types.AuditEvent{ID: uuid.New(), ActorType: types.ActorAgent, Action: "credential.mint", Outcome: outcome}
}

// TestBedrockWardynRunProvesPerUserSSO pins the LL3w audit check
// hermetically: the per-user SSO lane, an allow-listed model and a
// successful mint together pass; any one of them missing must fail —
// including a case that DOES carry a credential.* row (proving the check no
// longer reduces to "some row exists").
func TestBedrockWardynRunProvesPerUserSSO(t *testing.T) {
	good := []types.AuditEvent{
		bedrockConfigureEvent(t, "sso-inject-proxy", "us.anthropic.claude-haiku-4-5-20251001-v1:0"),
		mintEvent("success"),
	}
	if err := BedrockWardynRunProvesPerUserSSO(good); err != nil {
		t.Fatalf("a genuine per-user SSO run was rejected: %v", err)
	}

	for _, tc := range []struct {
		name   string
		events []types.AuditEvent
	}{
		{"no bedrock.configure row at all", []types.AuditEvent{mintEvent("success")}},
		{"wrong lane (bearer, not sso-inject-proxy)", []types.AuditEvent{
			bedrockConfigureEvent(t, "bearer", "us.anthropic.claude-haiku-4-5-20251001-v1:0"),
			mintEvent("success"),
		}},
		{"model off the allow-list", []types.AuditEvent{
			bedrockConfigureEvent(t, "sso-inject-proxy", "anthropic.claude-opus-4-1-20250805-v1:0"),
			mintEvent("success"),
		}},
		{"a credential row exists but no mint ever succeeded", []types.AuditEvent{
			bedrockConfigureEvent(t, "sso-inject-proxy", "us.anthropic.claude-haiku-4-5-20251001-v1:0"),
			mintEvent("denied"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := BedrockWardynRunProvesPerUserSSO(tc.events); err == nil {
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
