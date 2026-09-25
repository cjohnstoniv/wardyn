// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// statusCheck returns the row siteConfigStatusChecks emits for id, if any.
func statusCheck(sc types.SiteConfig, id string) (SetupCheck, bool) {
	for _, c := range siteConfigStatusChecks(nil, sc, nil) {
		if c.ID == id {
			return c, true
		}
	}
	return SetupCheck{}, false
}

// #489: a sign-in help link stored as http:// before the https-only rule is a
// setup warning, never a blocker; https:// or no link says nothing.
func TestSetupCheck_SignInHelpHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, link string
		want       bool
	}{
		{"no link", "", false},
		{"https link", "https://it.corp.example/request", false},
		{"http link", "http://helpdesk.corp.example/", true},
		{"upper-case HTTP scheme", "HTTP://helpdesk.corp.example/", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ok := statusCheck(types.SiteConfig{SignInHelpURL: tc.link}, "sign_in_help_url")
			if ok != tc.want {
				t.Fatalf("sign_in_help_url row present = %v, want %v (%+v)", ok, tc.want, c)
			}
			if !ok {
				return
			}
			const sentence = "The sign-in help link uses http://. Change it to an https:// address so people who can't sign in aren't sent to an unencrypted page."
			if c.Status != "warn" || c.Blocking || c.Detail != sentence || c.Label != "When someone can't sign in" {
				t.Errorf("row = %+v, want a non-blocking warn carrying the #489 sentence", c)
			}
		})
	}
}

// #603: more than one ENABLED Azure DevOps Entra row is a non-blocking warning
// on the Review step; a disabled row, or a row without the entra lane, does
// not count.
func TestSetupCheck_ADOEntraRows(t *testing.T) {
	entra := func(id string, disabled bool) types.GitProvider {
		return types.GitProvider{ID: id, Kind: types.GitProviderAzureDevOps, Disabled: disabled,
			BaseURLs: []string{"https://dev.azure.com/" + id}, Lanes: []types.GitLane{types.GitLaneEntra}}
	}
	pat := types.GitProvider{ID: "pat", Kind: types.GitProviderAzureDevOps,
		BaseURLs: []string{"https://dev.azure.com/pat"}, Lanes: []types.GitLane{types.GitLanePAT}}
	for _, tc := range []struct {
		name string
		rows []types.GitProvider
		want bool
	}{
		{"no rows", nil, false},
		{"one enabled", []types.GitProvider{entra("a", false)}, false},
		{"one enabled beside a PAT row", []types.GitProvider{entra("a", false), pat}, false},
		{"one enabled, one disabled", []types.GitProvider{entra("a", false), entra("b", true)}, false},
		{"two enabled", []types.GitProvider{entra("a", false), entra("b", false)}, true},
		{"three enabled", []types.GitProvider{entra("a", false), entra("b", false), entra("c", false)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := types.SiteConfig{}
			if tc.rows != nil {
				sc.WorkspaceProviders = &types.WorkspaceProviders{Git: tc.rows}
			}
			c, ok := statusCheck(sc, "ado_entra_rows")
			if ok != tc.want {
				t.Fatalf("ado_entra_rows row present = %v, want %v (%+v)", ok, tc.want, c)
			}
			if !ok {
				return
			}
			const sentence = "More than one Azure DevOps connection is enabled. Keep one enabled so runs sign in to a single organization."
			if c.Status != "warn" || c.Blocking || c.Detail != sentence {
				t.Errorf("row = %+v, want a non-blocking warn carrying the #603 sentence", c)
			}
		})
	}
}
