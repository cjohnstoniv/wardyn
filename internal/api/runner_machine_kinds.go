// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// machineKindDenialsOf is the stored runners.machine_kind_denials, [] when none, so the settings routes
// always state it.
func machineKindDenialsOf(cfg types.SiteConfig) []types.MachineKindDenial {
	if cfg.Runners == nil || cfg.Runners.MachineKindDenials == nil {
		return []types.MachineKindDenial{}
	}
	return slices.Clone(cfg.Runners.MachineKindDenials)
}

// withDenialList is rs with runners.machine_kind_denials a list, [] when none, so a site-config answer never says
// null. It is a copy: rs may be the store's own.
func withDenialList(rs *types.RunnerSettings) *types.RunnerSettings {
	if rs == nil || rs.MachineKindDenials != nil {
		return rs
	}
	c := *rs
	c.MachineKindDenials = []types.MachineKindDenial{}
	return &c
}

// machineKindDenialSiteRefusal says which denial is keyed to a site that is not a live organisation site, ""
// when each keyed one is live. A store that keeps no sites has none. A read that fails is returned.
func (s *Server) machineKindDenialSiteRefusal(ctx context.Context, ds []types.MachineKindDenial) (string, error) {
	for i, d := range ds {
		if d.SiteID == "" {
			continue
		}
		notLive := fmt.Sprintf("machine_kind_denials[%d]: site_id %s is not a live site", i, d.SiteID)
		ss, ok := s.cfg.Store.(store.OrgSiteStore)
		id, err := uuid.Parse(d.SiteID)
		if !ok || err != nil {
			return notLive, nil
		}
		site, err := ss.GetOrgSite(ctx, id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && site.State != types.OrgSiteActive) {
			return notLive, nil
		}
		if err != nil {
			return "", fmt.Errorf("read site %s: %w", id, err)
		}
	}
	return "", nil
}

// writeMachineKindDenialSiteRefusal answers machineKindDenialSiteRefusal: its read that failed, else its refusal.
func writeMachineKindDenialSiteRefusal(w http.ResponseWriter, r *http.Request, msg string, err error) {
	if err != nil {
		writeServerError(w, r, "read machine-kind denial sites", err)
		return
	}
	writeErrorReason(w, http.StatusBadRequest, reasonMachineKindDenialsInvalid, "Machine-kind denials: "+msg)
}

// denialSiteOf sets p's site at dispatch: the site run was placed at, read only while sites are on (sc is the
// caller's read), as at create. The site's name, which the refusal of a denial keyed to it says, is read only
// when such a denial is listed. A read that fails is returned, and the dispatch fails closed.
func (s *Server) denialSiteOf(ctx context.Context, sc types.SiteConfig, run types.AgentRun, p *placement.LocalPlan) error {
	if run.SiteID == nil || !siteFeatureOn(sc, featureOrgSites) {
		return nil
	}
	p.SiteID = run.SiteID
	keyed := slices.ContainsFunc(p.MachineKindDenials, func(d types.MachineKindDenial) bool { return d.SiteID == run.SiteID.String() })
	ss, ok := s.cfg.Store.(store.OrgSiteStore)
	if !keyed || !ok {
		return nil
	}
	site, err := ss.GetOrgSite(ctx, *run.SiteID)
	if err != nil {
		return fmt.Errorf("read the run's site: %w", err)
	}
	p.SiteName = site.Name
	return nil
}
