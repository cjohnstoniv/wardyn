// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "testing"

// TestSetupBedrock_ReadyAndConfigured exercises the pure SetupStatus.Bedrock
// helpers the wizard's bedrock_provider check relies on.
func TestSetupBedrock_ReadyAndConfigured(t *testing.T) {
	cases := []struct {
		name           string
		b              SetupBedrock
		wantConfigured bool
		wantReady      bool
	}{
		{"untouched", SetupBedrock{}, false, false},
		{"region only", SetupBedrock{Region: "us-east-1"}, true, false},
		{"creds only", SetupBedrock{CredsPresent: true}, true, false},
		{"fully configured", SetupBedrock{Region: "us-east-1", Model: "m", CredsPresent: true}, true, true},
		// A ~/.aws mount or a bearer token is a valid credential source on its own —
		// region+model+either must read ready (the "needs setup" bug was gating on
		// CredsPresent alone). The mount/bearer flag alone (no region/model) is
		// configured-but-not-ready.
		{"mount only", SetupBedrock{AWSMount: true}, true, false},
		{"bearer only", SetupBedrock{BearerPresent: true}, true, false},
		{"ready via mount", SetupBedrock{Region: "us-east-1", Model: "m", AWSMount: true}, true, true},
		{"ready via bearer", SetupBedrock{Region: "us-east-1", Model: "m", BearerPresent: true}, true, true},
		{"region+model no creds", SetupBedrock{Region: "us-east-1", Model: "m"}, true, false},
	}
	for _, c := range cases {
		if got := c.b.configured(); got != c.wantConfigured {
			t.Errorf("%s: configured() = %v, want %v", c.name, got, c.wantConfigured)
		}
		if got := c.b.ready(); got != c.wantReady {
			t.Errorf("%s: ready() = %v, want %v", c.name, got, c.wantReady)
		}
	}
}
