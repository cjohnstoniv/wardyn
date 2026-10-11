// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"slices"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The runners management routes (0.9). Revoking a runner is the revoke door's, not this file's.
//
//	GET /runners, /runners/{id}, /runners/tokens, /runners/settings   security (admin or security_admin)
//	DELETE /runners/tokens/{id}                                        security
//	PUT /runners/settings                                              admin (SUPER)
//	GET /me/runners, /me/runners/{id}                                  the caller's own runners only
func (s *Server) mountRunnerInventoryRoutes(r, operatorOnly, securityOps chi.Router) {
	securityOps.Get("/runners", s.handleListRunners)
	securityOps.Get("/runners/tokens", s.handleListRunnerTokens)
	securityOps.Delete("/runners/tokens/{id}", s.handleRevokeRunnerToken)
	securityOps.Get("/runners/settings", s.handleGetRunnerSettings)
	operatorOnly.Put("/runners/settings", s.handlePutRunnerSettings)
	securityOps.Get("/runners/{id}", s.handleGetRunner)
	r.Get("/me/runners", s.handleListMyRunners)
	r.Get("/me/runners/{id}", s.handleGetMyRunner)
}

func (s *Server) runnerInventoryStore(w http.ResponseWriter) (store.RunnerInventoryStore, bool) {
	rs, ok := s.cfg.Store.(store.RunnerInventoryStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonRunnerRegistrationUnavailable, "runner inventory requires the Postgres store backend")
	}
	return rs, ok
}

// runnerOnline is the hub's live answer, false whenever it cannot be asked: a runner is never
// shown online on a guess.
func (s *Server) runnerOnline(id uuid.UUID) bool {
	return s.cfg.RunnerOnline != nil && s.cfg.RunnerOnline(id)
}

// runnerLapsed reports an unclaimed runner past its wait: it no longer exists for any reader,
// whether or not the sweeper has deleted its row yet.
func runnerLapsed(row types.Runner, now time.Time) bool {
	return row.State == types.RunnerUnclaimed && !row.CreatedAt.After(now.Add(-types.RunnerUnclaimedTTL))
}

// runnerView is the row as a reader sees it. admin is the administrators' view: the owner is named
// and the fingerprint is whole. On an owner's own route an unclaimed runner's fingerprint is
// abbreviated, so the console cannot be the place it is copied from into the claim.
func (s *Server) runnerView(row types.Runner, runs int, admin bool) types.RunnerView {
	v := types.RunnerView{
		ID: row.ID, Name: row.Name, State: row.State, MintedBy: row.MintedBy,
		Online:         row.State == types.RunnerClaimed && s.runnerOnline(row.ID),
		KeyFingerprint: row.KeyFingerprint, Version: row.Version, CreatedAt: row.CreatedAt,
		ClaimedAt: row.ClaimedAt, LastSeenAt: row.LastSeenAt, RevokedAt: row.RevokedAt,
		PostureSource: row.PostureSource, RunsActive: runs,
	}
	if admin {
		v.Owner = row.Owner
	} else if row.State == types.RunnerUnclaimed {
		v.KeyFingerprint, v.KeyFingerprintAbbreviated = abbreviateFingerprint(row.KeyFingerprint)
	}
	if row.State == types.RunnerUnclaimed {
		expires := row.CreatedAt.Add(types.RunnerUnclaimedTTL)
		v.ClaimExpiresAt = &expires
	}
	if row.PostureReportedAt != nil {
		posture := row.Posture
		v.Posture, v.PostureReportedAt = &posture, row.PostureReportedAt
	}
	return v
}

const (
	fingerprintHead = 8
	fingerprintTail = 4
)

func abbreviateFingerprint(fp string) (string, bool) {
	if len(fp) <= fingerprintHead+fingerprintTail {
		return fp, false
	}
	return fp[:fingerprintHead] + "…" + fp[len(fp)-fingerprintTail:], true
}

// runnerViews is rows as views, each with its count of runs.
func (s *Server) runnerViews(w http.ResponseWriter, r *http.Request, rs store.RunnerInventoryStore, rows []types.Runner, admin bool) ([]types.RunnerView, bool) {
	ids := make([]uuid.UUID, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	runs, err := rs.CountActiveRunsByRunner(r.Context(), ids)
	if err != nil {
		writeServerError(w, r, "count runs by runner", err)
		return nil, false
	}
	out := make([]types.RunnerView, len(rows))
	emails := map[string]string{}
	for i, row := range rows {
		out[i] = s.runnerView(row, runs[row.ID], admin)
		if admin || row.MintedBy == "" || row.MintedBy == row.Owner {
			continue
		}
		email, seen := emails[row.MintedBy]
		if !seen {
			// A name is a courtesy: a failed lookup leaves the opaque subject, never fails the read.
			email, _ = s.principalEmail(r.Context(), row.MintedBy)
			emails[row.MintedBy] = email
		}
		out[i].MintedByEmail = email
	}
	return out, true
}

// handleListRunners is GET /api/v1/runners?state=active|revoked|all&limit=&offset= (default active):
// every person's runners, the owner named, newest first. Active is unclaimed and claimed.
func (s *Server) handleListRunners(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.runnerInventoryStore(w)
	if !ok {
		return
	}
	filter := types.RunnerFilter(r.URL.Query().Get("state"))
	switch filter {
	case "":
		filter = types.RunnerFilterActive
	case types.RunnerFilterActive, types.RunnerFilterRevoked, types.RunnerFilterAll:
	default:
		writeErrorReason(w, http.StatusBadRequest, reasonRunnerFilterInvalid, "state must be active, revoked or all")
		return
	}
	page, ok := parseListPage(w, r, defaultListLimit)
	if !ok {
		return
	}
	rows, truncated, err := pagedItems(page, func(p store.Page) ([]types.Runner, error) {
		return rs.ListRunnersPage(r.Context(), filter, s.cfg.Now().UTC(), p)
	}, nil)
	if err != nil {
		writeServerError(w, r, "list runners", err)
		return
	}
	views, ok := s.runnerViews(w, r, rs, rows, true)
	if !ok {
		return
	}
	if truncated {
		w.Header().Set("X-Wardyn-Truncated", "true")
	}
	writeJSON(w, http.StatusOK, views)
}

// handleGetRunner is GET /api/v1/runners/{id}, revoked included.
func (s *Server) handleGetRunner(w http.ResponseWriter, r *http.Request) {
	s.getRunnerView(w, r, "", true)
}

// handleListMyRunners is GET /api/v1/me/runners: the caller's own unclaimed and claimed runners.
func (s *Server) handleListMyRunners(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnersEnabled(w, r) {
		return
	}
	owner, ok := s.runnerOwnerForRead(w, r)
	if !ok {
		return
	}
	rs, ok := s.runnerInventoryStore(w)
	if !ok {
		return
	}
	rows, err := rs.ListRunnersByOwner(r.Context(), owner)
	if err != nil {
		writeServerError(w, r, "list my runners", err)
		return
	}
	now := s.cfg.Now().UTC()
	rows = slices.DeleteFunc(rows, func(row types.Runner) bool {
		return row.Owner != owner || row.State == types.RunnerRevoked || runnerLapsed(row, now)
	})
	if views, ok := s.runnerViews(w, r, rs, rows, false); ok {
		writeJSON(w, http.StatusOK, views)
	}
}

// handleGetMyRunner is GET /api/v1/me/runners/{id}. Another person's runner, a revoked one and an
// absent id answer alike.
func (s *Server) handleGetMyRunner(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnersEnabled(w, r) {
		return
	}
	owner, ok := s.runnerOwnerForRead(w, r)
	if !ok {
		return
	}
	s.getRunnerView(w, r, owner, false)
}

// getRunnerView serves one runner. A non-empty owner confines it to that person's own live runners.
func (s *Server) getRunnerView(w http.ResponseWriter, r *http.Request, owner string, admin bool) {
	rs, ok := s.runnerInventoryStore(w)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "runner")
	if !ok {
		return
	}
	row, err := rs.GetRunner(r.Context(), id)
	if err == nil && (runnerLapsed(row, s.cfg.Now().UTC()) || (!admin && (row.Owner != owner || row.State == types.RunnerRevoked))) {
		err = store.ErrNotFound
	}
	if notFoundIf(w, err, "runner", reasonRunnerNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "read runner", err)
		return
	}
	views, ok := s.runnerViews(w, r, rs, []types.Runner{row}, admin)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, views[0])
}

// runnerOwnerForRead is the person whose own runners a /me read shows.
func (s *Server) runnerOwnerForRead(w http.ResponseWriter, r *http.Request) (string, bool) {
	return s.runnerSessionOwner(w, r, reasonRunnerSessionRequired, "sign in under your own account to see your runners")
}
