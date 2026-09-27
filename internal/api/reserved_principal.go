// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "strings"

// isReservedPrincipal reports whether p names an identity that is not a
// person: the admin token, the local-mode operator (the configured seat, or any
// "local:" name — the default seat is "local:<os-user>", and a run it created
// outlives a later switch to SSO), or a device. Authorization compares the
// caller's principal to these strings, so a human whose identity-provider
// subject — or whose wdn_ token's replayed principal — equals one would be
// treated as that identity: owning its runs, skipping its re-checks (#1162).
// Every door a human principal enters by refuses one: the SSO callback, a
// session cookie, and a wdn_ token.
//
// Trimmed and case-folded (Unicode: a Kelvin sign in place of the k in
// "admin-token" folds too). No comparison matches a variant today; refusing
// one keeps that true if a comparison ever folds.
func (s *Server) isReservedPrincipal(p string) bool {
	p = strings.ToLower(strings.TrimSpace(p))
	op := strings.ToLower(strings.TrimSpace(s.cfg.LocalOperator))
	return p == adminTokenPrincipal || (op != "" && p == op) ||
		strings.HasPrefix(p, "local:") || strings.HasPrefix(p, "device:")
}
