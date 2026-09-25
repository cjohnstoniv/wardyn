// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// siteConfigStatusChecks bundles the /setup/status rows derived from one
// site-config read — siteConfigCheck, artifactRepoCheck and the conditional
// rows — into a single call, which keeps handleSetupStatus (setup.go) under
// the funlen gate as this list grows.
func siteConfigStatusChecks(checks []SetupCheck, sc types.SiteConfig, present map[string]bool) []SetupCheck {
	checks = append(checks, siteConfigCheck(sc, present), artifactRepoCheck(sc))
	for _, conditional := range []func(types.SiteConfig) (SetupCheck, bool){internalHostsCheck, signInHelpHTTPCheck, adoEntraRowsCheck} {
		if chk, ok := conditional(sc); ok {
			checks = append(checks, chk)
		}
	}
	return checks
}

// signInHelpHTTPCheck warns about a sign-in help link stored as http:// before
// the https-only rule (#489). The write refuses a new one; this is how an
// admin learns about the one already there. Only a link /healthz actually
// publishes (signInHelpPublic's check) is warned about: one it drops sends
// nobody anywhere. Frozen strings: docs/design/admin-access-canon.md.
func signInHelpHTTPCheck(sc types.SiteConfig) (SetupCheck, bool) {
	if !signInHelpIsHTTP(sc.SignInHelpURL) || validateSignInHelp("", sc.SignInHelpURL) != nil {
		return SetupCheck{}, false
	}
	return SetupCheck{
		ID: "sign_in_help_url", Label: "When someone can't sign in", Status: "warn",
		Detail: "The sign-in help link uses http://. Change it to an https:// address so people who can't sign in aren't sent to an unencrypted page.",
	}, true
}

// adoEntraRowsCheck warns when more than one ENABLED Azure DevOps row carries
// the entra lane (#603). The write refuses a second one (validateOneEntraRow),
// but a document stored before that rule can hold two, and only the first is
// ever served a sign-in. Not blocking: runs on the first row still work.
// Frozen string: docs/design/ado-card-canon.md.
func adoEntraRowsCheck(sc types.SiteConfig) (SetupCheck, bool) {
	enabled := 0
	for _, row := range gitProviderRows(sc) {
		if !row.Disabled && slices.Contains(row.Lanes, types.GitLaneEntra) {
			enabled++
		}
	}
	if enabled < 2 {
		return SetupCheck{}, false
	}
	return SetupCheck{
		ID: "ado_entra_rows", Label: "Azure DevOps", Status: "warn",
		Detail: "More than one Azure DevOps connection is enabled. Keep one enabled so runs sign in to a single organization.",
	}, true
}
