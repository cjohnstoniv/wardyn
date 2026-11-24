// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/netip"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// serveKnobs are the values parseServeKnobs reads and checks for the Server.
type serveKnobs struct {
	defaultPolicy         types.RunPolicySpec
	siteConfigSeed        *api.SiteConfigSeed
	demoVideoBaseURL      string
	cliArtifactBase       string
	orgCAFingerprints     []string
	governAdminRunsExempt []string
	trustedProxies        []netip.Prefix
}

// parseServeKnobs reads, once at boot and failing closed on a malformed value, the two
// files the Server is given (WARDYN_DEFAULT_POLICY and WARDYN_SITE_CONFIG_SEED_FILE) and
// the free-text knobs api.Config carries parsed: WARDYN_DEMO_VIDEO_BASE_URL,
// WARDYN_CLI_ARTIFACT_BASE, WARDYN_ORG_CA_FINGERPRINTS, WARDYN_GOVERN_ADMIN_RUNS_EXEMPT
// and WARDYN_TRUSTED_PROXIES.
func parseServeKnobs(f *bootFlags) (k serveKnobs, err error) {
	if k.defaultPolicy, err = api.LoadPolicySpec(*f.policyPath); err != nil {
		return serveKnobs{}, err
	}
	if k.siteConfigSeed, err = api.LoadSiteConfigSeed(*f.siteConfigSeedFile); err != nil {
		return serveKnobs{}, err
	}
	if k.demoVideoBaseURL, err = api.ValidateDemoVideoBaseURL(*f.demoVideoBaseURL); err != nil {
		return serveKnobs{}, err
	}
	if k.cliArtifactBase, err = api.ValidateCLIArtifactBase(*f.cliArtifactBase); err != nil {
		return serveKnobs{}, err
	}
	if k.orgCAFingerprints, err = api.ParseOrgCAFingerprints(*f.orgCAFingerprints); err != nil {
		return serveKnobs{}, err
	}
	if k.trustedProxies, err = api.ParseTrustedProxies(*f.trustedProxies); err != nil {
		return serveKnobs{}, err
	}
	k.governAdminRunsExempt, err = parseGovernAdminRunsExempt(*f.governAdminRunsExempt, *f.governAdminRuns)
	return k, err
}
