// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Issuer portals are off until a super admin switches them on, once. The operator's statement that every
// older wardynd is stopped is the fence; a rolling sample of the database's other client sessions is a veto
// of that statement, never proof of it. Until activation every replica samples every portalSampleEvery and
// keeps the last portalSampleWindow in memory; after it, every replica samples hourly as an alarm.
const (
	portalSampleEvery  = 10 * time.Second
	portalSampleWindow = 2 * time.Minute
	portalAlarmEvery   = time.Hour
)

const (
	portalActivateStatementMsg  = "State that every pre-0.10.2 wardynd is stopped (old_servers_stopped: true) to switch issuer portals on."
	portalSampleWarmingMsg      = "This replica started less than two minutes ago and has not seen enough database sessions to check for older servers. Try again shortly."
	portalOldServerSuspectedMsg = "Another client of this database does not carry a wardynd label of 0.10.2 or later. Stop it, or check each listed session and confirm with confirm_listed_sessions."
)

// portalSample is the other client sessions one read of the database saw.
type portalSample struct {
	at       time.Time
	sessions []types.ClientSession
}

// portalAlarmKey is what the hourly alarm dedupes on, per replica: the label, the address and the UTC day.
type portalAlarmKey struct {
	label, addr, day string
}

// portalActivationMonitor is one replica's rolling sample and its alarm's memory. The zero value is ready to
// use. started is the time of its first sample, which a replica that never samples never sets, so such a
// replica answers the activation as still warming.
type portalActivationMonitor struct {
	mu      sync.Mutex
	started time.Time
	samples []portalSample
	seen    map[portalAlarmKey]bool
}

// record adds a sample taken at now and drops what is older than the window.
func (m *portalActivationMonitor) record(now time.Time, sessions []types.ClientSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started.IsZero() {
		m.started = now
	}
	m.samples = append(m.samples, portalSample{at: now, sessions: sessions})
	m.samples = slices.DeleteFunc(m.samples, func(p portalSample) bool { return now.Sub(p.at) > portalSampleWindow })
}

// suspects is the distinct sessions of the samples in the window ending at now, and how long the replica
// still has to wait before the window is full (zero when it is).
func (m *portalActivationMonitor) suspects(now time.Time) ([]types.ClientSession, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started.IsZero() {
		return nil, portalSampleWindow
	}
	if wait := portalSampleWindow - now.Sub(m.started); wait > 0 {
		return nil, wait
	}
	var out []types.ClientSession
	for _, p := range m.samples {
		if now.Sub(p.at) > portalSampleWindow {
			continue
		}
		for _, c := range p.sessions {
			if !slices.Contains(out, c) {
				out = append(out, c)
			}
		}
	}
	return out, 0
}

// newSightings is the sessions not yet reported today, marked reported, and drops keys of earlier days.
func (m *portalActivationMonitor) newSightings(now time.Time, sessions []types.ClientSession) []types.ClientSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	day := now.UTC().Format("2006-01-02")
	for k := range m.seen {
		if k.day != day {
			delete(m.seen, k)
		}
	}
	if m.seen == nil {
		m.seen = map[portalAlarmKey]bool{}
	}
	var out []types.ClientSession
	for _, c := range sessions {
		k := portalAlarmKey{c.Label, c.ClientAddr, day}
		if !m.seen[k] {
			m.seen[k] = true
			out = append(out, c)
		}
	}
	return out
}

// RunPortalActivationMonitor samples the database's other clients until ctx ends: every portalSampleEvery
// until issuer portals are switched on, hourly after, when a session without a 0.10.2 label writes one
// portal_issuers.old_client_seen row a day per replica. Boot calls it once, beside the other sweepers.
func (s *Server) RunPortalActivationMonitor(ctx context.Context) {
	for {
		activated, _ := s.portalMonitorStep(ctx, s.cfg.Now().UTC())
		wait := portalSampleEvery
		if activated {
			wait = portalAlarmEvery
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// portalMonitorStep is one sample at now: recorded while portals are off, reported as an alarm once on. It
// returns whether portals are on (false when that could not be read).
func (s *Server) portalMonitorStep(ctx context.Context, now time.Time) (bool, error) {
	ps, ok := s.cfg.Store.(store.PortalActivationStore)
	if !ok {
		return false, nil
	}
	activated, err := ps.PortalIssuersActivated(ctx)
	if err != nil {
		slog.WarnContext(ctx, "portal activation: read the switch", slog.Any("err", err))
		return false, err
	}
	sessions, err := ps.UntaggedClientSessions(ctx)
	if err != nil {
		slog.WarnContext(ctx, "portal activation: sample the database's clients", slog.Any("err", err))
		return activated, err
	}
	if !activated {
		s.portalActivation.record(now, sessions)
		return false, nil
	}
	for _, c := range s.portalActivation.newSightings(now, sessions) {
		slog.WarnContext(ctx, "portal activation: a database client without a 0.10.2 label", slog.String("label", c.Label), slog.String("client_addr", c.ClientAddr))
		s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "wardynd", "portal_issuers.old_client_seen", c.Label, "success",
			mustJSON(map[string]any{"application_name": c.Label, "client_addr": c.ClientAddr})))
	}
	return true, nil
}

// mountPortalIssuerRoutes mounts the issuer-portal registry's admin routes (portal_issuer_*.go).
func (s *Server) mountPortalIssuerRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.humanOrAdminAuth)
		operatorOnly := r.With(s.requireOperator)
		operatorOnly.Post("/admin/portal-issuers/activate", s.handleActivatePortalIssuers)
		operatorOnly.Delete("/admin/portal-issuers/{k}", s.handleRetirePortalIssuer)
		operatorOnly.Put("/admin/delegates/{id}/admission", s.handlePutDelegateAdmission)
	})
}

type activatePortalIssuersRequest struct {
	OldServersStopped     bool `json:"old_servers_stopped"`
	ConfirmListedSessions bool `json:"confirm_listed_sessions"`
}

// portalSessionNames renders sessions as "label@address" for an audit row or a refusal.
func portalSessionNames(sessions []types.ClientSession) []string {
	out := make([]string, len(sessions))
	for i, c := range sessions {
		out[i] = c.Label + "@" + c.ClientAddr
	}
	return out
}

// handleActivatePortalIssuers is POST /api/v1/admin/portal-issuers/activate: the operator states that every
// pre-0.10.2 wardynd is stopped. The call reads this replica's rolling sample and answers at once. A session
// that is not a 0.10.2 wardynd vetoes it (409), unless confirm_listed_sessions overrides, which is audited
// with each session named. The mark is set once and never cleared.
func (s *Server) handleActivatePortalIssuers(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.cfg.Store.(store.PortalActivationStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonDelegationStoreUnavailable, "issuer portals require the Postgres store backend")
		return
	}
	var req activatePortalIssuersRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if !req.OldServersStopped {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonPortalIssuersStatementRequired, portalActivateStatementMsg)
		return
	}
	already, err := ps.PortalIssuersActivated(r.Context())
	if err != nil {
		writeServerError(w, r, "read the issuer portal switch", err)
		return
	}
	if already {
		writeJSON(w, http.StatusOK, map[string]any{"activated": true, "already_activated": true})
		return
	}
	suspects, wait := s.portalActivation.suspects(s.cfg.Now().UTC())
	if wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeErrorReason(w, http.StatusConflict, reasonPortalIssuersSampleWarming, portalSampleWarmingMsg)
		return
	}
	names := portalSessionNames(suspects)
	if len(suspects) > 0 && !req.ConfirmListedSessions {
		writeJSON(w, http.StatusConflict, map[string]any{"error": portalOldServerSuspectedMsg,
			"reason": reasonPortalIssuersOldServerSuspected, "sessions": suspects})
		return
	}
	actorType, actor := actorFromRequest(r)
	if err := ps.ActivatePortalIssuers(r.Context(), actor, names); err != nil {
		writeServerError(w, r, "switch issuer portals on", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor, "portal_issuers.activate", "portal_issuers", "success",
		mustJSON(map[string]any{"old_servers_stopped": true, "sessions_seen": len(suspects)})))
	if len(suspects) > 0 {
		s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor, "portal_issuers.activate_override", "portal_issuers", "success",
			mustJSON(map[string]any{"sessions": names})))
	}
	writeJSON(w, http.StatusOK, map[string]any{"activated": true, "overridden_sessions": len(suspects)})
}
