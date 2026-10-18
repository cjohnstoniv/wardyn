// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
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

// proxyImageGiven is whether the proxy image was named for serve outside the environment: on the command
// line, or in a managed profile, which an administrator pushed on purpose.
func proxyImageGiven(cmd *cobra.Command) bool {
	return cmd.Flags().Changed("proxy-image") || managedPref("ProxyImage") != ""
}

// proxyImagePinned says whether the image serve was given (--proxy-image or WARDYN_PROXY_IMAGE, "" when
// neither) was named on purpose, so the organisation's answer at connect must not replace it: it was
// passed as a flag, setup marked it WARDYN_PROXY_IMAGE_PINNED=1, or it is not this project's release
// image (a mirror) and not the image the organisation named at registration (identityImage, which a
// setup writes unmarked). An unmarked release image is what a setup before 0.9.3 derived, not a pin.
func proxyImagePinned(image, identityImage string, flagGiven bool, getenv func(string) string) bool {
	if image == "" {
		return false
	}
	if flagGiven || getenv("WARDYN_PROXY_IMAGE_PINNED") == "1" {
		return true
	}
	if image == identityImage {
		return false
	}
	tag, ok := strings.CutPrefix(image, releaseProxyRepo+":")
	return !(ok && releaseVersion.MatchString(tag))
}

// followOrgProxyImage is the hook that takes the proxy image the organisation answers at each connect,
// so an upgraded organisation's image reaches this runner without a reinstall. A pinned image wins,
// and an unusable answer is ignored. current is the image already in use; set applies a new one to
// the runs started from then on.
func followOrgProxyImage(current string, pinned bool, set func(string)) func(string) {
	if pinned {
		return nil
	}
	return func(answered string) {
		if answered == current {
			return
		}
		if !types.SafeImageRef(answered) {
			slog.WarnContext(context.Background(), "the organisation named a proxy image this runner cannot use; keeping the current one", "current", current)
			return
		}
		slog.InfoContext(context.Background(), "proxy image follows the organisation", "from", current, "to", answered)
		set(answered)
		current = answered
	}
}
