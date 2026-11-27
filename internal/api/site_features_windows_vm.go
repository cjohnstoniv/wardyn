// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Windows VM sandboxes can be turned on: the licence fence, the runner and the API are all in place. available stays
// true, since the database's take trigger reads the stored switch whatever this server says.
func init() {
	registerSiteFeature(featureWindowsVM, siteFeatureHook{available: func() bool { return true }})
}
