// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"regexp"

	"github.com/cjohnstoniv/wardyn/internal/version"
)

// releaseVersion is a release (X.Y.Z) or a release candidate (X.Y.Z-rc.N). A development build's
// version carries +<commit>, so it never matches.
var releaseVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$`)

// buildVersion is this binary's version, swapped in tests.
var buildVersion = version.String

// releaseProxyRepo is where release proxy images are published.
const releaseProxyRepo = "ghcr.io/cjohnstoniv/wardyn-proxy"

// releaseProxyImage is the published proxy image built from this binary's version, or "" on a
// development build, which has no matching image.
func releaseProxyImage() string {
	if v := buildVersion(); releaseVersion.MatchString(v) {
		return releaseProxyRepo + ":" + v
	}
	return ""
}
