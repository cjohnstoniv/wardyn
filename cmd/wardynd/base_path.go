// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// basePathRe is one or more /segment parts of URL-unreserved characters: a
// leading slash, no trailing slash, no empty segment, no query or fragment,
// and nothing that would need escaping where internal/api writes the base
// into index.html.
var basePathRe = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)+$`)

// validateBasePath is WARDYN_BASE_PATH's boot refusal. Beyond the value's own
// shape it refuses the two URLs that would silently miss the prefix: the OIDC
// callback, which the IdP would send to a 404, and a plain-http control-plane
// URL, which runs dial on the console listener (an https one reaches the
// internal listener, which stays at the host root).
func validateBasePath(base, oidcIssuer, oidcRedirectURL, controlURL string) error {
	if base == "" {
		return nil
	}
	dots := func(seg string) bool { return seg == "." || seg == ".." }
	if !basePathRe.MatchString(base) || slices.ContainsFunc(strings.Split(base, "/"), dots) {
		return fmt.Errorf("refusing to start: WARDYN_BASE_PATH %q is not a sub-path like \"/wardyn\": it needs a leading slash, no trailing slash, no \".\" or \"..\" segment, no query or fragment, and only letters, digits and . _ ~ -", base)
	}
	if oidcIssuer != "" {
		u, err := url.Parse(strings.TrimSpace(oidcRedirectURL))
		if err != nil || !strings.HasPrefix(u.Path, base+"/") {
			return fmt.Errorf("refusing to start: WARDYN_OIDC_REDIRECT_URL %q is not under WARDYN_BASE_PATH %q — sign-in is served only under the base path, so the identity provider would return every login to a 404; register and set https://<host>%s/auth/callback", oidcRedirectURL, base, base)
		}
	}
	if u, err := url.Parse(strings.TrimSpace(controlURL)); err == nil && strings.EqualFold(u.Scheme, "http") && strings.TrimSuffix(u.Path, "/") != base {
		return fmt.Errorf("refusing to start: WARDYN_CONTROL_PLANE_URL %q is plain http://, so runs reach this daemon on the console listener, which serves only under WARDYN_BASE_PATH %q — append the base path (%s://%s%s)", controlURL, base, u.Scheme, u.Host, base)
	}
	return nil
}
