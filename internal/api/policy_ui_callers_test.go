// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestValidateUIAppsCallers pins the inbound declaration's shape: callers belong to an inbound app, at most 32,
// each in the canonical form the resolver matches, named once, and never `all`.
func TestValidateUIAppsCallers(t *testing.T) {
	many := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("service:svc-%02d", i)
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		app     types.UIApp
		wantErr string // "" = valid
	}{
		{"inbound, no callers: the owner only", types.UIApp{Name: "api", Port: 8080, Inbound: true}, ""},
		{"all three forms", types.UIApp{Name: "api", Port: 8080, Inbound: true,
			Callers: []string{"user:ana@example.com", "user:00u1a2b3c4", "group:payments", "service:billing"}}, ""},
		{"a group name may hold a space", types.UIApp{Name: "api", Port: 8080, Inbound: true, Callers: []string{"group:domain admins"}}, ""},
		{"32 callers", types.UIApp{Name: "api", Port: 8080, Inbound: true, Callers: many(32)}, ""},
		{"33 callers", types.UIApp{Name: "api", Port: 8080, Inbound: true, Callers: many(33)}, "exceeds the maximum of 32"},
		{"callers without inbound", types.UIApp{Name: "api", Port: 8080, Callers: []string{"user:ana@example.com"}}, "only meaningful with inbound"},
		{"all is not a caller", types.UIApp{Name: "api", Port: 8080, Inbound: true, Callers: []string{"all"}}, `"all" is not a caller`},
		{"all: is not a caller", types.UIApp{Name: "api", Port: 8080, Inbound: true, Callers: []string{"all:"}}, "invalid caller"},
		{"a bare name", types.UIApp{Name: "api", Port: 8080, Inbound: true, Callers: []string{"ana"}}, "invalid caller"},
		{"an upper-case kind", types.UIApp{Name: "api", Port: 8080, Inbound: true, Callers: []string{"User:ana@example.com"}}, "invalid caller"},
		{"an upper-case user", types.UIApp{Name: "api", Port: 8080, Inbound: true, Callers: []string{"user:Ana@example.com"}}, "invalid user caller"},
		{"an empty user", types.UIApp{Name: "api", Port: 8080, Inbound: true, Callers: []string{"user:"}}, "invalid user caller"},
		{"a user with a space", types.UIApp{Name: "api", Port: 8080, Inbound: true, Callers: []string{"user:ana smith"}}, "invalid user caller"},
		{"an upper-case group", types.UIApp{Name: "api", Port: 8080, Inbound: true, Callers: []string{"group:Payments"}}, "invalid group caller"},
		{"a non-ASCII group", types.UIApp{Name: "api", Port: 8080, Inbound: true, Callers: []string{"group:payKments"}}, "invalid group caller"},
		{"an empty group", types.UIApp{Name: "api", Port: 8080, Inbound: true, Callers: []string{"group:"}}, "invalid group caller"},
		{"a service name too short", types.UIApp{Name: "api", Port: 8080, Inbound: true, Callers: []string{"service:ab"}}, "invalid service caller"},
		{"an upper-case service", types.UIApp{Name: "api", Port: 8080, Inbound: true, Callers: []string{"service:Billing"}}, "invalid service caller"},
		{"a service starting with a digit", types.UIApp{Name: "api", Port: 8080, Inbound: true, Callers: []string{"service:1billing"}}, "invalid service caller"},
		{"a duplicate", types.UIApp{Name: "api", Port: 8080, Inbound: true, Callers: []string{"group:payments", "service:billing", "group:payments"}}, "duplicate caller"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateUIApps([]types.UIApp{tc.app})
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("validateUIApps = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("validateUIApps = %v, want an error naming %q", err, tc.wantErr)
			}
			if err != nil && !strings.HasPrefix(err.Error(), "ui_apps[0]: ") {
				t.Errorf("error %q does not name the app it is about", err)
			}
		})
	}
}

// The check runs in validatePolicySpec's funnel, so a stored policy, an inline policy and a profile ceiling all meet it.
func TestValidatePolicySpecRefusesABadCaller(t *testing.T) {
	spec := types.RunPolicySpec{MinConfinementClass: types.CC2, UIApps: []types.UIApp{{Name: "api", Port: 8080, Inbound: true, Callers: []string{"everyone"}}}}
	err := validatePolicySpec(spec)
	if err == nil || !strings.Contains(err.Error(), "ui_apps[0]: callers[0]") {
		t.Fatalf("validatePolicySpec = %v, want a refusal naming ui_apps[0] callers[0]", err)
	}
}
