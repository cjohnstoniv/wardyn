// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The features block's reason: the same closed set and convention as reasons.go, in a file of its own beside
// site_features.go. TestReasonDocsMatchReasonsGo reads this file too.
const (
	reasonSiteFeatureUnavailable = "feature_unavailable" // PUT /site-config turns on or sets a features key this release cannot turn on
)
