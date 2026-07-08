// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// export_test.go exposes internal functions for white-box testing.
// This file is only compiled in test builds.
package oidc

import "net/http"

// EncodeSessionForTest calls the unexported encodeSession method so that
// test files can build synthetic session cookies without going through the
// full OAuth2 flow.
func EncodeSessionForTest(a *Authenticator, sess Session) (*http.Cookie, error) {
	return a.encodeSession(sess)
}

// NewRewriteTransportForTest exposes the unexported split-horizon transport
// constructor for white-box testing.
func NewRewriteTransportForTest(publicURL, internalURL string, base *http.Client) (http.RoundTripper, error) {
	return newRewriteTransport(publicURL, internalURL, base)
}
