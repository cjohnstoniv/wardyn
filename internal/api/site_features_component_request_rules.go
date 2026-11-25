// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Request rules on custom components work end to end on this release, so the key is always available; the
// stored switch alone turns them on (componentRequestRulesOn).
func init() {
	registerSiteFeature(featureComponentRequestRules, siteFeatureHook{available: func() bool { return true }})
}
