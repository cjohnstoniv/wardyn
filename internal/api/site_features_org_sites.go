// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Organisation sites (org_sites.go, run_placement_site.go) work end to end through the API, so the switch can
// be turned on. It is a plain on or off switch: there is no setting to check and nothing to do after a write.
func init() {
	registerSiteFeature(featureOrgSites, siteFeatureHook{available: func() bool { return true }})
}
