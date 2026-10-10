// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The three tier ids of docs/design/0.9/PLAN.md §10.3. A full daemon answers
// only two of them on its own; "runner" is reported by a laptop still
// enrolled into an org (WARDYN_ORG_URL, deprecated), and a client-mode runner
// (wardyn-runnerd) runs no control plane to ask.
const (
	TierLocalOnly = "local-only"
	TierRunner    = "runner"
	TierOrg       = "org"
)

// setupTier names who governs this install: an org-enrolled laptop is a
// runner, a daemon with SSO is the organisation's, anything else answers
// only to the person at the keyboard.
func (s *Server) setupTier() string {
	switch {
	case s.cfg.OrgFederation != nil:
		return TierRunner
	case s.cfg.OIDC != nil:
		return TierOrg
	default:
		return TierLocalOnly
	}
}
