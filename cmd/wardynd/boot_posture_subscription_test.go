// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// The whole point of subscriptionInjectPosture is the NEGATIVE cases, so the
// table leads with them. A shared subscription credential is one operator's live
// Anthropic OAuth token; injecting it into another human's run is what the
// harness vendor's terms prohibit, and the operator running Wardyn is the one who
// ends up in breach.
func TestSubscriptionInjectPosture(t *testing.T) {
	for _, tc := range []struct {
		name        string
		runner      string
		oidc        bool
		localMode   bool
		allowShared bool
		want        bool
	}{
		// The permitted shape: a single-user desktop.
		{"local mode, docker runner, no sso", "docker", false, true, false, true},

		// The demo waiver: compose runs a shared admin token, not local mode.
		{"override on a non-local docker daemon", "docker", false, false, true, true},

		// k8s is multi-user by definition and the waiver must NOT reach it. The
		// chart has no WARDYN_LOCAL_MODE key, but .Values.env renders verbatim, so
		// a cluster CAN be booted into local mode — this clause is what stops it.
		{"k8s runner", "k8s", false, false, false, false},
		{"k8s runner even in local mode", "k8s", false, true, false, false},
		{"k8s runner even with the override", "k8s", false, true, true, false},

		// A configured issuer declares that more than one human exists.
		// WARDYN_ALLOW_LOCAL_MODE_WITH_OIDC can produce localMode+oidc together;
		// the waiver must not reach that either.
		{"oidc configured", "docker", true, false, false, false},
		{"oidc with local mode (the ALLOW_LOCAL_MODE_WITH_OIDC shape)", "docker", true, true, false, false},
		{"oidc with the override", "docker", true, true, true, false},

		// Neither local mode nor the waiver: an authenticated multi-user daemon.
		{"admin-token daemon, no waiver", "docker", false, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := subscriptionInjectPosture(tc.runner, tc.oidc, tc.localMode, tc.allowShared)
			if got != tc.want {
				t.Fatalf("subscriptionInjectPosture(%q, oidc=%v, local=%v, allow=%v) = %v, want %v",
					tc.runner, tc.oidc, tc.localMode, tc.allowShared, got, tc.want)
			}
			// A refusal the operator cannot act on is a support ticket. Every denial
			// must name a way forward.
			if !got {
				if reason == "" {
					t.Fatal("refused with an empty reason")
				}
				if !strings.Contains(reason, "API key") && !strings.Contains(reason, "Bedrock") {
					t.Fatalf("refusal reason names no alternative credential path: %q", reason)
				}
			}
			if got && reason != "" {
				t.Fatalf("allowed but carried a reason: %q", reason)
			}
		})
	}
}
