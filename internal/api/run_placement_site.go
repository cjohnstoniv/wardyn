// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// siteChoice is what the site step found for a run: what it hands runnerpool.ResolveSite (the default pool
// of the one site the run's homed drives share, and whether the run is pinned) and a preference it decided
// itself, for drives homed at different sites or a site with no default pool to weigh.
type siteChoice struct {
	choice runnerpool.Choice
	pref   runnerpool.SitePreference
}

// siteStep reads where the run's drives are homed. It reads nothing, and answers no site, while sites are off
// (sc is the caller's own read of the site config), when the request names its pool, and when something
// already holds the run to one runner: a drive or workspace folder bound to it, a kept machine
// (keptPlacementPin), or the request's runner_id. Such a pin keeps that runner alone (kindTargets), so a
// site pool that does not hold it would refuse a run that places today. A read that fails is returned, and
// the create refuses with it: a run is never placed as if its drives had no site.
func (s *Server) siteStep(ctx context.Context, in placementInput, sc types.SiteConfig) (siteChoice, error) {
	if !siteFeatureOn(sc, featureOrgSites) {
		return siteChoice{}, nil
	}
	if in.driveRunner != nil || in.mountRunner != nil || in.req.RunnerID != "" {
		return siteChoice{choice: runnerpool.Choice{Pinned: true}}, nil
	}
	ss, ok := s.cfg.Store.(store.OrgSiteStore)
	if !ok || in.req.RunnerPoolID != "" || len(in.driveIDs) == 0 {
		return siteChoice{}, nil
	}
	homes, err := ss.DriveHomeSites(ctx, in.driveIDs)
	if err != nil {
		return siteChoice{}, fmt.Errorf("read the drives' sites: %w", err)
	}
	siteID, pref := drivesHomeSite(in.driveIDs, homes)
	if siteID == nil {
		return siteChoice{pref: pref}, nil
	}
	site, err := ss.GetOrgSite(ctx, *siteID)
	if err != nil {
		return siteChoice{}, fmt.Errorf("read the drives' site: %w", err)
	}
	if site.State != types.OrgSiteActive || site.DefaultPoolID == nil {
		return siteChoice{pref: runnerpool.SiteUnusable}, nil
	}
	return siteChoice{choice: runnerpool.Choice{SiteDefault: site.DefaultPoolID}}, nil
}

// drivesHomeSite is the one site every homed drive of a run is at, as drivesBoundRunner is the one runner
// every drive is bound to. A drive with no home is passed over. No homed drive gives no site and no
// preference; drives homed at two sites give no site and runnerpool.SiteAmbiguous.
func drivesHomeSite(driveIDs []uuid.UUID, homes map[uuid.UUID]uuid.UUID) (*uuid.UUID, runnerpool.SitePreference) {
	var site *uuid.UUID
	for _, id := range driveIDs {
		home, ok := homes[id]
		switch {
		case !ok:
		case site == nil:
			site = &home
		case *site != home:
			return nil, runnerpool.SiteAmbiguous
		}
	}
	return site, ""
}

// driveIDsOf is the ids of a run's drives, in the run's order.
func driveIDsOf(drives []runDrive) []uuid.UUID {
	if len(drives) == 0 {
		return nil
	}
	out := make([]uuid.UUID, len(drives))
	for i, d := range drives {
		out[i] = d.resolved.Drive.ID
	}
	return out
}

// poolSiteOf sets pool's site: the live site the pool is at, read only while sites are on (sc is the
// caller's read). With sites off, or a pool at no live site, pool is returned as it is. A read that fails
// is returned, and the create refuses with it.
func (s *Server) poolSiteOf(ctx context.Context, sc types.SiteConfig, pool types.ResolvedRunnerPool) (types.ResolvedRunnerPool, error) {
	ss, ok := s.cfg.Store.(store.OrgSiteStore)
	if !ok || !siteFeatureOn(sc, featureOrgSites) {
		return pool, nil
	}
	site, at, err := ss.PoolSite(ctx, pool.ID)
	if err != nil {
		return pool, fmt.Errorf("read the pool's site: %w", err)
	}
	if at {
		id := site.ID
		pool.SiteID, pool.SiteName = &id, site.Name
	}
	return pool, nil
}

// siteDefaultRefusal is a refusal of a run whose pool was chosen as its drives' site's default: the same
// status and reason as for that pool chosen as the person's own default, with a sentence that says why this
// pool was chosen and how to run elsewhere. A server error, or a refusal of a pool chosen any other way, is
// returned as it is.
func siteDefaultRefusal(pool types.ResolvedRunnerPool, r *runRefusal) *runRefusal {
	if r == nil || pool.Selection != types.RunnerPoolSelectedSite || r.err != nil || r.status >= http.StatusInternalServerError {
		return r
	}
	out := *r
	out.body.Error = r.body.Error + " " + siteDefaultSentence(pool)
	return &out
}

// siteDefaultSentence says why a run went to pool: it is the default pool of the site its drives are at.
func siteDefaultSentence(pool types.ResolvedRunnerPool) string {
	if pool.SiteName == "" {
		return pool.Name + " was chosen because it is the default pool of the site this run's drives are at. Name a pool to run elsewhere."
	}
	return pool.Name + " was chosen because it is the default pool of " + pool.SiteName + ", the site this run's drives are at. Name a pool to run elsewhere."
}
