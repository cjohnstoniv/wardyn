// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runAttribution is the policy the run's proxy names in a refusal the policy
// decided: the leaf profile the run was launched under (its contact optional: without one it
// borrows the site's policy_help),
// else the site's policy_help, else nil. Frozen into the sidecar config for the
// run's life. A profile that cannot be read is logged and falls through, since
// the attribution is advice and must not fail a launch.
func (s *Server) runAttribution(ctx context.Context, run types.AgentRun, site types.SiteConfig) *policyref.Ref {
	p, err := s.runProfile(ctx, run)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: the run's governance profile could not be read for its refusal attribution",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
	}
	if p != nil {
		return profileRefOrSiteHelp(p, site.PolicyHelp)
	}
	if site.PolicyHelp != nil {
		return policyref.Project(policyref.SourceDeployment, "", site.PolicyHelp)
	}
	return nil
}
