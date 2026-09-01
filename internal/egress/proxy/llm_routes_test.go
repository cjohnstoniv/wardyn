// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import "testing"

// TestIsLLMHost_BedrockPrivateEndpoint pins isLLMHost's Bedrock arm across the
// PrivateLink widening. The ACCEPT rows are the public regional endpoints plus
// the VPC-endpoint forms that CONTAIN but do not START WITH the service name
// (the old prefix matcher dropped them, so private-endpoint model traffic fell
// out of the audit trail's opaque-tunnel coverage).
//
// The REJECT rows are the ones that matter: isLLMHost picks the inspection path
// in serveMITMRequest, so a hostile or squattable lookalike that talks its way
// in opts a MITM'd body OUT of inspect_forward_egress scanning. Every accept is
// anchored on AWS-owned DNS — a plain substring match would take every row
// below the fold.
func TestIsLLMHost_BedrockPrivateEndpoint(t *testing.T) {
	p := newProxy(Options{})

	for _, tc := range []struct {
		host string
		want bool
		why  string
	}{
		// --- public regional (unchanged behaviour) ---
		{"bedrock-runtime.us-east-1.amazonaws.com", true, "public data plane"},
		{"bedrock.us-east-1.amazonaws.com", true, "public control plane"},
		{"BEDROCK-RUNTIME.US-EAST-1.AMAZONAWS.COM", true, "case-insensitive"},
		{"bedrock-runtime.us-east-1.amazonaws.com.", true, "trailing dot trimmed"},
		{"bedrock-runtime.us-gov-west-1.amazonaws.com", true, "four-part gov region"},
		{"bedrock-runtime.ap-southeast-4.amazonaws.com", true, "region label with a digit"},

		// --- PrivateLink / VPC endpoint (the gap this closes) ---
		{"vpce-0a1b2c3d4e5f6a7b8-9zyxwvut.bedrock-runtime.us-east-1.vpce.amazonaws.com", true, "interface endpoint"},
		{"vpce-0a1b2c3d-9zyx-us-east-1a.bedrock-runtime.us-east-1.vpce.amazonaws.com", true, "zonal endpoint"},
		{"vpce-0a1b2c3d4e5f6a7b8-bedrock-runtime.us-east-1.vpce.amazonaws.com", true, "hyphen-glued service name"},
		{"vpce-0a1b2c3d4e5f6a7b8-9zyxwvut.bedrock.us-east-1.vpce.amazonaws.com", true, "control plane via PrivateLink"},

		// --- hostile lookalikes: the rows that matter ---
		{"bedrock-runtime.evil.com", false, "attacker-owned zone wearing the service name"},
		{"x-bedrock.attacker.net", false, "service name as a hyphen suffix off-AWS"},
		{"bedrock-runtime.us-east-1.amazonaws.com.evil.com", false, "AWS host as a left-hand prefix"},
		{"bedrock-runtime.s3.amazonaws.com", false, "legacy S3 virtual host — bucket names are attacker-chosen"},
		{"bedrock-runtime.s3-website-us-east-1.amazonaws.com", false, "S3 website endpoint, same squat"},
		{"vpce-0a1b.bedrock-runtime.us-east-1.vpce.amazonaws.com.evil.com", false, "vpce host as a left-hand prefix"},
		{"bedrock-runtime.us-east-1.vpce.amazonaws.evil.com", false, "amazonaws as a bare label, not the zone"},
		{"notbedrock-runtime.us-east-1.amazonaws.com", false, "service name glued without a hyphen"},
		{"bedrockruntime.us-east-1.amazonaws.com", false, "not the service name"},
		{"my-bedrock-runtime.corp.internal", false, "corp artifact host that must keep the artifact scan path"},
		{"s3.us-east-1.amazonaws.com", false, "AWS, but not Bedrock"},
		{"", false, "empty host"},
	} {
		if got := p.isLLMHost(tc.host); got != tc.want {
			t.Errorf("isLLMHost(%q) = %v, want %v (%s)", tc.host, got, tc.want, tc.why)
		}
	}
}
