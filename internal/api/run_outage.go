// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// recordOutageTerms fixes run's outage terms (#4082) from the site's allowance and the resolved policy, and records them
// once, so a later site or policy edit never changes a running run's bound. It returns the recorded terms, which on a
// second dispatch of the same run are the first dispatch's. A failed site read takes the default allowance, which no
// site value is shorter than. Terms that cannot be recorded refuse the run, unless they are the default ones, which a
// missing record already reads as.
func (s *Server) recordOutageTerms(ctx context.Context, run types.AgentRun, siteCfg types.SiteConfig, siteCfgErr error, policy types.RunPolicySpec) (types.OutageTerms, bool) {
	site := 0
	if siteCfgErr == nil {
		site = siteCfg.OutageAllowance()
	}
	terms := types.EffectiveOutageTerms(site, policy.Outage)
	termer, ok := s.cfg.Store.(store.RunOutageTermer)
	var err error
	if ok {
		var stored types.OutageTerms
		if stored, err = termer.SetRunOutageTerms(ctx, run.ID, terms); err == nil {
			return stored, true
		}
	}
	switch {
	case terms.IsDefault():
		if err != nil {
			slog.WarnContext(ctx, "wardynd: recording a run's default outage terms failed",
				slog.String("run_id", run.ID.String()), slog.Any("err", err))
		}
		return terms, true
	case !ok:
		err = errors.New("the run store cannot record outage terms")
	}
	s.failAndRevoke(ctx, run.ID, types.RunStarting, "This run was not launched: its outage terms could not be recorded.")
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create", run.ID.String(), "failure",
		mustJSON(map[string]any{"error": "outage terms: " + loggedMsg(ctx, "record outage terms", err)})))
	return terms, false
}

// runOutageTerms is run id's recorded outage terms: the defaults when the store keeps none.
func (s *Server) runOutageTerms(ctx context.Context, id uuid.UUID) (types.OutageTerms, error) {
	termer, ok := s.cfg.Store.(store.RunOutageTermer)
	if !ok {
		return types.DefaultOutageTerms(), nil
	}
	return termer.RunOutageTerms(ctx, id)
}

// proxyOutage is the proxy's outage key for t: nil when the proxy would act as it does by default (the default
// allowance, not strict), because a previous release's proxy refuses the key. EndUnattended is the control plane's
// alone, so it never writes the key.
func proxyOutage(t types.OutageTerms) *proxy.OutageConfig {
	if t.AllowanceSec == types.OutageAllowanceDefaultMinutes*60 && !t.Strict {
		return nil
	}
	return &proxy.OutageConfig{AllowanceS: t.AllowanceSec, Strict: t.Strict}
}

// validateSiteOutageAllowance refuses a site allowance other than unset, 0 (the default) or 60 to 1440 minutes.
func validateSiteOutageAllowance(p *int) error {
	if p != nil && *p != 0 && (*p < types.OutageAllowanceDefaultMinutes || *p > types.OutageAllowanceMaxMinutes) {
		return fmt.Errorf("outage_allowance_minutes must be 0 or %d to %d", types.OutageAllowanceDefaultMinutes, types.OutageAllowanceMaxMinutes)
	}
	return nil
}
