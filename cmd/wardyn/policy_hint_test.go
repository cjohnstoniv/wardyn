// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"
	"testing"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestPolicyHint(t *testing.T) {
	ref := &sdk.PolicyRef{Source: "profile", Name: "walled", Owner: "Platform team",
		Email: "platform@example.com", RequestURL: "https://help.example.com/request", RequestText: "File a ticket."}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"profile with a link", &sdk.APIError{Status: 403, Policy: ref},
			`governed by profile "walled" (owner: Platform team), to request a change: File a ticket. https://help.example.com/request`},
		{"the email route when there is no link", fmt.Errorf("create: %w", &sdk.APIError{Status: 403,
			Policy: &sdk.PolicyRef{Source: "profile", Name: "walled", Email: "platform@example.com"}}),
			`governed by profile "walled", to request a change: mailto:platform@example.com`},
		{"the deployment", &sdk.APIError{Status: 403, Policy: &sdk.PolicyRef{Source: "deployment", RequestText: "Ask IT."}},
			`governed by this deployment's policy, to request a change: Ask IT.`},
		{"a policy with no route names itself alone", &sdk.APIError{Status: 403, Policy: &sdk.PolicyRef{Source: "profile", Name: "walled"}},
			`governed by profile "walled"`},
		{"no policy, no line", &sdk.APIError{Status: 403}, ""},
		{"not an API error", fmt.Errorf("boom"), ""},
		{"control characters never reach the terminal", &sdk.APIError{Status: 403, Policy: &sdk.PolicyRef{Source: "profile",
			Name: "wal\x1b[2Jled", RequestText: "line\nbreak\u202eevil"}},
			`governed by profile "wal[2Jled", to request a change: linebreakevil`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := policyHint(tc.err); got != tc.want {
				t.Errorf("policyHint = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPolicyHintDoesNotChangeTheExitCode: the remedy line is printed after the
// error and never moves the code CI branches on.
func TestPolicyHintDoesNotChangeTheExitCode(t *testing.T) {
	for _, status := range []int{403, 422} {
		plain := &sdk.APIError{Status: status}
		governed := &sdk.APIError{Status: status, Policy: &sdk.PolicyRef{Source: "profile", Name: "walled"}}
		if a, b := exitCodeFor(plain), exitCodeFor(governed); a != b {
			t.Errorf("status %d: exit code %d without a policy, %d with one", status, a, b)
		}
	}
	if got := strings.Count(policyHint(&sdk.APIError{Status: 403, Policy: &sdk.PolicyRef{Source: "profile", Name: "a"}}), "\n"); got != 0 {
		t.Errorf("the hint spans %d line breaks, want one line", got+1)
	}
}
