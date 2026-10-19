// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// countReauthTimeout counts a credential hold that ran out on its OWN series,
// at the one moment the control plane learns of it (handlePostDecision): the
// expiry happens in the sidecar, and the approval row deliberately stays
// PENDING (the sign-in is still wanted), so this decision row is the only
// signal that reaches here. An Azure DevOps hold writes the same rule source;
// isADOEntraHost says why it is excluded.
func (s *Server) countReauthTimeout(ctx context.Context, dl egress.DecisionLog) {
	if dl.Decision != egress.Deny || dl.RuleSource != ruleSourceCredentialReauthTimeout || isADOEntraHost(dl.Request.Host) {
		return
	}
	// An Azure DevOps Server own-token hold stamps the Server host, which no name
	// rule can tell from another forge: the configured azure_devops rows can (#2801).
	if sc, ok := s.siteConfigSnapshot(ctx); ok {
		host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(dl.Request.Host), "."))
		if slices.Contains(adoServerHosts(sc), host) {
			return
		}
	}
	s.metrics.credentialReauthRecorded(credentialReauthOutcomeTimeout)
}
