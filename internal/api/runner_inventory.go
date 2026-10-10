// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
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

// runnerViews lists rows through keep, never null, with each runner's run count.
func (s *Server) runnerViews(w http.ResponseWriter, r *http.Request, rs store.RunnerInventoryStore, rows []types.Runner, admin bool, keep func(types.Runner) bool) ([]types.RunnerView, bool) {
	runs, err := rs.CountActiveRunsByRunner(r.Context())
	if err != nil {
		writeServerError(w, r, "count runs by runner", err)
		return nil, false
	}
	now := s.cfg.Now().UTC()
	out := make([]types.RunnerView, 0, len(rows))
	for _, row := range rows {
		if !runnerLapsed(row, now) && keep(row) {
			out = append(out, s.runnerView(row, runs[row.ID], admin))
		}
	}
	return out, true
}

// handleListRunners is GET /api/v1/runners?state=active|revoked|all (default active): every
// person's runners, the owner named. Active is unclaimed and claimed.
func (s *Server) handleListRunners(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.runnerInventoryStore(w)
	if !ok {
		return
	}
	var keep func(types.Runner) bool
	switch r.URL.Query().Get("state") {
	case "", "active":
		keep = func(row types.Runner) bool { return row.State != types.RunnerRevoked }
	case "revoked":
		keep = func(row types.Runner) bool { return row.State == types.RunnerRevoked }
	case "all":
		keep = func(types.Runner) bool { return true }
	default:
		writeErrorReason(w, http.StatusBadRequest, reasonRunnerFilterInvalid, "state must be active, revoked or all")
		return
	}
	rows, err := rs.ListRunners(r.Context())
	if err != nil {
		writeServerError(w, r, "list runners", err)
		return
	}
	if views, ok := s.runnerViews(w, r, rs, rows, true, keep); ok {
		writeJSON(w, http.StatusOK, views)
	}
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
	owner, ok := s.runnerSessionOwner(w, r)
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
	keep := func(row types.Runner) bool { return row.Owner == owner && row.State != types.RunnerRevoked }
	if views, ok := s.runnerViews(w, r, rs, rows, false, keep); ok {
		writeJSON(w, http.StatusOK, views)
	}
}

// handleGetMyRunner is GET /api/v1/me/runners/{id}. Another person's runner, a revoked one and an
// absent id answer alike.
func (s *Server) handleGetMyRunner(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnersEnabled(w, r) {
		return
	}
	owner, ok := s.runnerSessionOwner(w, r)
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
	runs, err := rs.CountActiveRunsByRunner(r.Context())
	if err != nil {
		writeServerError(w, r, "count runs by runner", err)
		return
	}
	writeJSON(w, http.StatusOK, s.runnerView(row, runs[row.ID], admin))
}

// runnerSessionOwner is the person whose own runners a /me route shows: the signed-in human, never an
// administrative or delegated credential, which owns no runner.
func (s *Server) runnerSessionOwner(w http.ResponseWriter, r *http.Request) (string, bool) {
	owner := oidcHumanFromContext(r.Context())
	if owner == "" || neverOperator(r.Context()) {
		writeErrorReason(w, http.StatusForbidden, reasonRunnerSessionRequired, "sign in under your own account to see your runners")
		return "", false
	}
	return owner, true
}
