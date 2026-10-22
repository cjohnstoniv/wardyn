// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"maps"
	"net/http"
	"slices"
)

// meServiceReach is GET /me's `service` block, present only for a service identity's token: the routes
// it may call and what it may already launch with. Read-only; it grants nothing.
type meServiceReach struct {
	Name       string             `json:"name"`
	Sponsor    string             `json:"sponsor"`
	Routes     []string           `json:"routes"`     // the allow-list keys, sorted
	Presets    []meReachPreset    `json:"presets"`    // what GET /presets lists for this caller
	Workspaces []meReachWorkspace `json:"workspaces"` // what GET /workspaces lists for this caller
}

type meReachPreset struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

type meReachWorkspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// meServiceReach builds the block from the same predicates the routes use: presetOpenTo (GET /presets)
// and serviceLaunchableWorkspaces (GET /workspaces). It writes the error those routes would and reports
// false when either read fails, so /me never shows a reach narrower or wider than the routes answer.
func (s *Server) meServiceReach(w http.ResponseWriter, r *http.Request) (meServiceReach, bool) {
	name, sponsor, _ := serviceIdentityFromContext(r.Context())
	reach := meServiceReach{Name: name, Sponsor: sponsor, Routes: slices.Sorted(maps.Keys(serviceAllowed)),
		Presets: []meReachPreset{}, Workspaces: []meReachWorkspace{}}
	presets, err := s.cfg.Store.ListLaunchPresets(r.Context())
	if err != nil {
		writeServerError(w, r, "list presets", err)
		return meServiceReach{}, false
	}
	for _, p := range presets {
		if s.presetOpenTo(r, p) {
			reach.Presets = append(reach.Presets, meReachPreset{Name: p.Name, Version: p.Version})
		}
	}
	rows, err := s.serviceLaunchableWorkspaces(r.WithContext(withCapBatch(r.Context())))
	if errors.Is(err, errServiceSiteConfigUnreadable) {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonRosterUnreadable, "the site configuration could not be read; try again")
		return meServiceReach{}, false
	}
	if err != nil {
		writeServerError(w, r, "list workspaces", err)
		return meServiceReach{}, false
	}
	for _, ws := range rows {
		reach.Workspaces = append(reach.Workspaces, meReachWorkspace{ID: ws.ID.String(), Name: ws.Name})
	}
	return reach, true
}
