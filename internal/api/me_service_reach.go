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
// and serviceLaunchableWorkspaces (GET /workspaces). A failed read is returned, never a shorter list;
// writeServiceReachError answers it as those routes would.
func (s *Server) meServiceReach(r *http.Request) (meServiceReach, error) {
	name, sponsor, _ := serviceIdentityFromContext(r.Context())
	reach := meServiceReach{Name: name, Sponsor: sponsor, Routes: slices.Sorted(maps.Keys(serviceAllowed)),
		Presets: []meReachPreset{}, Workspaces: []meReachWorkspace{}}
	presets, err := s.cfg.Store.ListLaunchPresets(r.Context())
	if err != nil {
		return meServiceReach{}, reachReadError{"list presets", err}
	}
	for _, p := range presets {
		if s.presetOpenTo(r, p) {
			reach.Presets = append(reach.Presets, meReachPreset{Name: p.Name, Version: p.Version})
		}
	}
	rows, err := s.serviceLaunchableWorkspaces(r.WithContext(withCapBatch(r.Context())))
	if err != nil {
		return meServiceReach{}, reachReadError{"list workspaces", err}
	}
	for _, ws := range rows {
		reach.Workspaces = append(reach.Workspaces, meReachWorkspace{ID: ws.ID.String(), Name: ws.Name})
	}
	return reach, nil
}

// writeServiceReachError answers a failed meServiceReach: 503 roster_unreadable when the site config
// cannot be read, as GET /workspaces does, otherwise the routes' 500 under the read that failed.
func writeServiceReachError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errServiceSiteConfigUnreadable) {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonRosterUnreadable, "the site configuration could not be read; try again")
		return
	}
	var re reachReadError
	if errors.As(err, &re) {
		writeServerError(w, r, re.read, re.err)
		return
	}
	writeServerError(w, r, "service reach", err)
}

// reachReadError is a failed read inside meServiceReach, named as the route that makes it names it.
type reachReadError struct {
	read string
	err  error
}

func (e reachReadError) Error() string { return e.read + ": " + e.err.Error() }
func (e reachReadError) Unwrap() error { return e.err }
