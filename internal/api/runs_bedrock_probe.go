// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "os"

// SetupBedrock is the readiness snapshot of the boot-environment Bedrock lanes
// (WARDYN_BEDROCK_* and the operator's AWS secrets) the wizard renders. A run's
// Bedrock credential comes only from a Bedrock model provider; this reports
// what the boot environment still configures until those lanes retire.
type SetupBedrock struct {
	Region string `json:"region,omitempty"`
	Model  string `json:"model,omitempty"`
	// The three boot-environment credential SOURCES, in precedence order
	// (bearer > ~/.aws mount > resident SigV4). ANY one is sufficient — a
	// mount- or bearer-credentialed host has NO aws-access-key-id/-secret
	// secrets yet is fully ready.
	CredsPresent  bool `json:"creds_present"`  // resident aws-access-key-id + aws-secret-access-key secrets
	AWSMount      bool `json:"aws_mount"`      // host-mode read-only ~/.aws bind-mount (SSO auto-refreshes)
	BearerPresent bool `json:"bearer_present"` // bedrock-api-key bearer token secret (never resident)
	// Ready is the server-computed readiness (region+model+any credential source),
	// echoed so the UI doesn't re-derive — and drift from — this gate.
	Ready bool `json:"ready"`
}

// ready reports whether the boot environment names a region, a model and at
// least one credential source (presence, not value).
func (b SetupBedrock) ready() bool {
	return b.Region != "" && b.Model != "" && (b.CredsPresent || b.AWSMount || b.BearerPresent)
}

// configured reports whether the operator has touched ANY Bedrock knob (region,
// model, or a credential source that is Bedrock's alone) — used to decide
// whether the bedrock_provider check is worth showing at all.
func (b SetupBedrock) configured() bool {
	return b.Region != "" || b.Model != "" || b.CredsPresent || b.AWSMount || b.BearerPresent
}

// credSourceDesc names the winning credential source for honest UI copy —
// "resident keys" is wrong for a mount/bearer host.
func (b SetupBedrock) credSourceDesc() string {
	switch {
	case b.BearerPresent:
		return "a proxy-injected Bedrock API key (never resident in the sandbox)"
	case b.AWSMount:
		return "your host AWS credentials via a read-only ~/.aws mount (SSO auto-refreshes)"
	default:
		return "resident AWS SigV4 credentials"
	}
}

// setupBedrock reports boot-environment Bedrock readiness: region/model are
// boot-time config (non-secret, safe to echo to the UI) and each credential
// flag is presence, never the value. AWSMount is BedrockAWSConfigDir set AND
// the dir still existing (stat it, so a since-deleted ~/.aws doesn't read
// ready).
func (s *Server) setupBedrock(present map[string]bool) SetupBedrock {
	awsMount := false
	if s.cfg.BedrockAWSConfigDir != "" {
		st, err := os.Stat(s.cfg.BedrockAWSConfigDir)
		awsMount = err == nil && st.IsDir()
	}
	b := SetupBedrock{
		Region:        s.cfg.BedrockRegion,
		Model:         s.cfg.BedrockModel,
		CredsPresent:  present[bedrockAccessKeyIDSecret] && present[bedrockSecretAccessKeySecret],
		AWSMount:      awsMount,
		BearerPresent: present[bedrockAPIKeySecret],
	}
	b.Ready = b.ready()
	return b
}
