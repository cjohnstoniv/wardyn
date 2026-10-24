// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"strings"

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
// line, in a managed profile (which an administrator pushed on purpose) or in runnerd.json.
func proxyImageGiven(cfg runnerConfig) bool {
	src := cfg.field("proxy_image").Source
	return strings.HasPrefix(src, sourceFlag) || strings.HasPrefix(src, sourceManaged) || strings.HasPrefix(src, sourceFile)
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

// imageReleaseTag is the release version an image reference is tagged with, or "" for a digest-only
// reference, a development tag or anything else that is not X.Y.Z[-rc.N].
func imageReleaseTag(image string) string {
	ref, _, _ := strings.Cut(image, "@")
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i:], "/") {
		return ""
	}
	if tag := ref[i+1:]; releaseVersion.MatchString(tag) {
		return tag
	}
	return ""
}

// warnPinnedProxyImageSkew says, at startup, when the pinned proxy image is a release other than this
// binary's own. Nothing else compares them: an older proxy refuses configuration fields a newer
// runner sends, so runs fail to start.
func warnPinnedProxyImageSkew(pinned, runnerVersion string) {
	tag := imageReleaseTag(pinned)
	if tag == "" || !releaseVersion.MatchString(runnerVersion) || tag == runnerVersion {
		return
	}
	slog.WarnContext(context.Background(), "the pinned proxy image is a different release than this runner; runs may fail to start",
		"pinned", pinned, "runner", runnerVersion, "fix", "run setup with --proxy-image set to the image the organisation names, or remove the pin (--proxy-image and WARDYN_PROXY_IMAGE_PINNED) to follow the organisation")
}

// warnOlderProxyImage says, at startup, when the proxy image a k8s runner follows unpinned (the
// organisation's, named at registration) is an older release than this binary. The k8s driver waits
// for its proxy pod to answer proxy.ReadyPath, which an older proxy answers 404: the pod never reads
// Ready and every run fails at the start deadline. warnPinnedProxyImageSkew covers a pinned image.
func warnOlderProxyImage(image, runnerVersion string) {
	tag := imageReleaseTag(image)
	if tag == "" || !releaseVersion.MatchString(runnerVersion) || !releaseOlder(tag, runnerVersion) {
		return
	}
	slog.WarnContext(context.Background(), "the proxy image is an older release than this k8s runner; its pods never read ready, so every run fails to start",
		"image", image, "runner", runnerVersion, "fix", "run setup with --proxy-image "+releaseProxyRepo+":"+runnerVersion+" (or its mirror), or have the organisation name this release's proxy image")
}

// releaseOlder reports whether release a is older than release b, both matching releaseVersion: a
// release candidate is older than its release.
func releaseOlder(a, b string) bool {
	pa, pb := releaseParts(a), releaseParts(b)
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

// releaseParts is X, Y, Z and the candidate number of a release, a final release's candidate number
// being above every candidate's.
func releaseParts(v string) [4]int {
	var p [4]int
	core, rc, isRC := strings.Cut(v, "-rc.")
	for i, n := range strings.SplitN(core, ".", 3) {
		p[i], _ = strconv.Atoi(n)
	}
	p[3] = math.MaxInt
	if isRC {
		p[3], _ = strconv.Atoi(rc)
	}
	return p
}

// followOrgProxyImage is the hook that takes the proxy image the organisation answers at each connect,
// so an upgraded organisation's image reaches this runner without a reinstall. A pinned image wins,
// and an unusable answer is ignored. current is the image already in use; set applies a new one to
// the runs started from then on. A pinned runner's hook never calls set: it warns, once for each
// distinct answer, when the organisation names a different release than the pinned image's.
func followOrgProxyImage(current string, pinned bool, set func(string)) func(string) {
	if pinned {
		warned := map[string]bool{}
		return func(answered string) {
			want, have := imageReleaseTag(answered), imageReleaseTag(current)
			if want == "" || have == "" || want == have || warned[answered] {
				return
			}
			warned[answered] = true
			slog.WarnContext(context.Background(), "the organisation names a different proxy release than the one this runner is pinned to; runs may fail to start",
				"pinned", current, "organisation", answered, "fix", "run setup with --proxy-image "+answered+", or remove the pin (--proxy-image and WARDYN_PROXY_IMAGE_PINNED) to follow the organisation")
		}
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
