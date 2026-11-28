// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "github.com/cjohnstoniv/wardyn/internal/types"

// localWindowServed says this release can turn features.local_window on: the browser desktop yields its keyboard to
// an open local window, so a window and a browser desktop never both hold the run's keyboard.
const localWindowServed = true

func init() {
	registerSiteFeature(featureLocalWindow, siteFeatureHook{available: func() bool { return localWindowServed }})
}

// localWindowOn is whether sc turns on a window, on the person's own computer, onto a Linux VM run.
func localWindowOn(sc types.SiteConfig) bool { return siteFeatureOn(sc, featureLocalWindow) }
