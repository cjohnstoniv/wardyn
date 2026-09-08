// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDenyAlwaysReject_BedrockLaneIsGuarded pins F019: the "which host is this
// run's model provider" question had TWO implementations, and denyAlwaysReject —
// the guard whose whole job is to refuse a deny·always that permanently breaks a
// workspace's model access — consulted the anthropic/openai-only one. The Bedrock
// lane is not hypothetical: resolveBedrockAuth's PREFERRED bearer mode TLS-MITMs
// bedrock-runtime and injects the Authorization header proxy-side, which is
// exactly the "proxy-side credential injection refuses a denied host" failure the
// guard's own message names.
//
// RED on the base tree: the bedrock rows return "" (deny accepted, workspace
// bricked). The anthropic row is here so a fix that simply refused everything
// could not pass, and the unrelated row so the guard still lets ordinary hosts
// through.
func TestDenyAlwaysReject_BedrockLaneIsGuarded(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     Config
		host    string
		refused bool
	}{
		{"anthropic (already guarded)", Config{BedrockRegion: "us-east-1"}, "api.anthropic.com", true},
		{"bedrock data plane", Config{BedrockRegion: "us-east-1"}, "bedrock-runtime.us-east-1.amazonaws.com", true},
		{"bedrock control plane", Config{BedrockRegion: "us-east-1"}, "bedrock.us-east-1.amazonaws.com", true},
		{"bedrock data plane, trailing dot + case", Config{BedrockRegion: "us-east-1"}, "Bedrock-Runtime.US-East-1.amazonaws.com.", true},
		{
			"WARDYN_BEDROCK_BASE_URL override host",
			Config{BedrockRegion: "us-east-1", BedrockBaseURL: "https://vpce-abc.bedrock-runtime.us-east-1.vpce.amazonaws.com"},
			"vpce-abc.bedrock-runtime.us-east-1.vpce.amazonaws.com", true,
		},
		{"an ordinary app host stays decidable", Config{BedrockRegion: "us-east-1"}, "api.example.com", false},
		{"bedrock host with Bedrock disabled stays decidable", Config{}, "bedrock-runtime.us-east-1.amazonaws.com", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{cfg: tc.cfg}
			why := s.denyAlwaysReject(context.Background(), types.Workspace{}, tc.host)
			if refused := why != ""; refused != tc.refused {
				t.Fatalf("denyAlwaysReject(%q) = %q (refused=%v), want refused=%v", tc.host, why, refused, tc.refused)
			}
			if tc.refused && !strings.Contains(why, "permanently break model access") {
				t.Errorf("refusal for %q did not come from the model-provider arm: %q", tc.host, why)
			}
		})
	}
}
