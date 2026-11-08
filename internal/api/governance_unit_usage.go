// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The usage window: a read covers the runs created in [since, until). It ends now (to the next second) and spans 30 days unless
// asked, and no read spans more than 92 days, so one request sums a bounded slice of the usage table.
const (
	unitModelUsageDefaultWindow = 30 * 24 * time.Hour
	unitModelUsageMaxWindow     = 92 * 24 * time.Hour
)

// UnitModelUsageView is one unit's line of the response: the single-run usage view (totals, by model, the
// opaque tunnels and the newest row's time) over the runs frozen to that unit alone.
type UnitModelUsageView struct {
	UnitID   uuid.UUID `json:"unit_id"`
	UnitName string    `json:"unit_name"`
	ModelUsageView
}

// UnitModelUsageResponse is GET /api/v1/governance/units/{id}/model-usage: the unit asked about, first, and
// each unit below it that has usage in the window, by name. Tokens are shown, not cost.
type UnitModelUsageResponse struct {
	Since time.Time            `json:"since"`
	Until time.Time            `json:"until"`
	Units []UnitModelUsageView `json:"units"`
}

// handleUnitModelUsage serves GET /api/v1/governance/units/{id}/model-usage to a unit admin of the unit or
// of a unit above it, or a security admin. The window is checked first, then the scope: an unknown unit and
// a unit outside the caller's subtrees answer a unit admin the same refusal, so existence does not leak.
func (s *Server) handleUnitModelUsage(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "unit")
	if !ok {
		return
	}
	since, until, ok := parseUnitUsageWindow(w, r)
	if !ok || !s.requireUnitsInScope(w, r, "governance.units.model_usage", false, id) {
		return
	}
	us, ok := s.cfg.Store.(store.UnitModelUsageReader)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonModelUsageUnsupported, "this store does not keep model usage")
		return
	}
	rows, err := s.unitTreeRows(r.Context())
	if err != nil {
		writeServerError(w, r, "read the unit tree", err)
		return
	}
	names := map[uuid.UUID]string{}
	for _, row := range rows {
		names[row.ID] = row.Name
	}
	if _, found := names[id]; !found {
		writeErrorReason(w, http.StatusNotFound, reasonGovernanceUnitNotFoundByID, "governance unit not found")
		return
	}
	limit, err := s.unitDepthLimit(r.Context())
	if err != nil {
		writeServerError(w, r, "read the unit depth limit", err)
		return
	}
	subtree, _ := subtreeUnits(rows, []types.GovernanceUnitAdmin{{UnitID: id}}, limit)
	units := make([]uuid.UUID, 0, len(subtree))
	for u := range subtree {
		units = append(units, u)
	}
	sums, tunnels, err := readUnitModelUsage(r, us, units, since, until)
	if err != nil {
		writeErrorReason(w, http.StatusInternalServerError, reasonModelUsageReadFailed, "could not read model usage")
		return
	}
	var providers []types.ModelProvider
	if site, ok := s.siteConfigSnapshot(r.Context()); ok && site.ModelProviders != nil {
		providers = site.ModelProviders.Providers
	}
	writeJSON(w, http.StatusOK, UnitModelUsageResponse{Since: since, Until: until,
		Units: unitModelUsageViews(id, units, names, sums, tunnels, providers)})
}

// parseUnitUsageWindow is the since and until of the request, UTC: until defaults to the next whole second,
// so a run created this second is inside the window, and since to 30 days before it. It writes the 400 and reports false for a time that does not parse, a window
// that is not forward, or one longer than 92 days.
func parseUnitUsageWindow(w http.ResponseWriter, r *http.Request) (since, until time.Time, ok bool) {
	q := r.URL.Query()
	until = time.Now().UTC().Truncate(time.Second).Add(time.Second)
	var err error
	if v := q.Get("until"); v != "" {
		if until, err = time.Parse(time.RFC3339, v); err != nil {
			return since, until, refuseUsageWindow(w)
		}
		until = until.UTC()
	}
	since = until.Add(-unitModelUsageDefaultWindow)
	if v := q.Get("since"); v != "" {
		if since, err = time.Parse(time.RFC3339, v); err != nil {
			return since, until, refuseUsageWindow(w)
		}
		since = since.UTC()
	}
	if !since.Before(until) || until.Sub(since) > unitModelUsageMaxWindow {
		return since, until, refuseUsageWindow(w)
	}
	return since, until, true
}

func refuseUsageWindow(w http.ResponseWriter) bool {
	writeErrorReason(w, http.StatusBadRequest, reasonModelUsageWindowInvalid,
		"since and until are RFC3339 times, since before until, at most 92 days apart")
	return false
}

// readUnitModelUsage is the usage sums and the opaque tunnel counts of units over the window.
func readUnitModelUsage(r *http.Request, us store.UnitModelUsageReader, units []uuid.UUID, since, until time.Time) (map[uuid.UUID]store.ModelUsageSummary, map[uuid.UUID]int64, error) {
	sums, err := us.UnitModelUsage(r.Context(), units, since, until)
	if err != nil {
		return nil, nil, err
	}
	tunnels, err := us.UnitOpaqueTunnels(r.Context(), units, since, until)
	return sums, tunnels, err
}

// unitModelUsageViews is the response's units: asked first, always (with zero totals when it has no usage),
// then each other unit that has usage or opaque tunnels, by name.
func unitModelUsageViews(asked uuid.UUID, units []uuid.UUID, names map[uuid.UUID]string, sums map[uuid.UUID]store.ModelUsageSummary, tunnels map[uuid.UUID]int64, providers []types.ModelProvider) []UnitModelUsageView {
	var out []UnitModelUsageView
	for _, u := range units {
		_, hasSum := sums[u]
		if u != asked && !hasSum && tunnels[u] == 0 {
			continue
		}
		v := buildModelUsageView(sums[u], tunnels[u], nil)
		nameUsageProviders(&v, providers)
		out = append(out, UnitModelUsageView{UnitID: u, UnitName: names[u], ModelUsageView: v})
	}
	slices.SortFunc(out, func(a, b UnitModelUsageView) int {
		if (a.UnitID == asked) != (b.UnitID == asked) {
			if a.UnitID == asked {
				return -1
			}
			return 1
		}
		if c := strings.Compare(a.UnitName, b.UnitName); c != 0 {
			return c
		}
		return strings.Compare(a.UnitID.String(), b.UnitID.String()) // two units may share a name
	})
	return out
}

// nameUsageProviders gives each line the first provider, in the site's order, whose address its host is;
// a line no provider serves keeps the host as its name.
func nameUsageProviders(v *ModelUsageView, providers []types.ModelProvider) {
	for i := range v.ByModel {
		for _, p := range providers {
			if providerServesHost(p, v.ByModel[i].Host) {
				v.ByModel[i].Provider = p.ID
				break
			}
		}
	}
}
