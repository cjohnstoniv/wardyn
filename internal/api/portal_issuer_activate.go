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
	"github.com/google/uuid"

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
	// portalSampleMaxGap is the longest silence between two successful samples that still counts as one
	// continuous window. A longer one (reads failing, a stalled replica) restarts the window.
	portalSampleMaxGap = 3 * portalSampleEvery
	portalAlarmEvery   = time.Hour
)

const (
	portalActivateStatementMsg  = "State that every pre-0.10.2 wardynd is stopped (old_servers_stopped: true) to switch issuer portals on."
	portalSampleWarmingMsg      = "This replica has not watched the database's sessions for a full two minutes (it just started, or its checks failed lately), so it cannot check for older servers yet. Try again shortly."
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

// portalMonitors holds each Server's monitor, so the Server struct (at the file-size gate) gains no field.
var portalMonitors sync.Map

// portalMonitor is this server's monitor, created on first use.
func (s *Server) portalMonitor() *portalActivationMonitor {
	v, _ := portalMonitors.LoadOrStore(s, &portalActivationMonitor{})
	return v.(*portalActivationMonitor)
}

// portalActivationMonitor is one replica's rolling sample and its alarm's memory. The zero value is ready to
// use. started is when the current continuous run of successful samples began and last is its newest sample;
// a replica that never samples has neither, and one whose samples stopped (reads failing) has a stale last,
// so both answer the activation as still warming. A window that is expired or has a gap is never clean.
type portalActivationMonitor struct {
	mu      sync.Mutex
	started time.Time
	last    time.Time
	samples []portalSample
	seen    map[portalAlarmKey]bool
	unsent  bool      // the activation's audit rows were owed and not yet settled at the last attempt
	retryAt time.Time // no new send of the owed rows before this
}

// mayRetry reports whether the owed rows may be sent at now.
func (m *portalActivationMonitor) mayRetry(now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !now.Before(m.retryAt)
}

func (m *portalActivationMonitor) retryAfter(at time.Time) {
	m.mu.Lock()
	m.retryAt = at
	m.mu.Unlock()
}

// owed reports whether the activation's audit rows are still unsettled, so the monitor retries soon.
func (m *portalActivationMonitor) owed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.unsent
}

func (m *portalActivationMonitor) setOwed(v bool) {
	m.mu.Lock()
	m.unsent = v
	m.mu.Unlock()
}

// record adds a sample taken at now and drops what is older than the window.
func (m *portalActivationMonitor) record(now time.Time, sessions []types.ClientSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started.IsZero() || now.Sub(m.last) > portalSampleMaxGap {
		// The first sample, or the first one after a gap: what was seen before the gap no longer counts and
		// the window starts over.
		m.started = now
		m.samples = nil
	}
	m.last = now
	m.samples = append(m.samples, portalSample{at: now, sessions: sessions})
	m.samples = slices.DeleteFunc(m.samples, func(p portalSample) bool { return now.Sub(p.at) > portalSampleWindow })
}

// suspects is the distinct sessions of the samples in the window ending at now, and how long the replica
// still has to wait before the window is full (zero when it is). A window whose newest sample is older than
// portalSampleMaxGap is not full: the replica has not observed the database lately, so it waits a whole window.
func (m *portalActivationMonitor) suspects(now time.Time) ([]types.ClientSession, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started.IsZero() || now.Sub(m.last) > portalSampleMaxGap {
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
		if activated && !s.portalMonitor().owed() {
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
	if activated {
		s.deliverPortalActivation(ctx, ps)
	}
	sessions, err := ps.UntaggedClientSessions(ctx)
	if err != nil {
		slog.WarnContext(ctx, "portal activation: sample the database's clients", slog.Any("err", err))
		return activated, err
	}
	if !activated {
		s.portalMonitor().record(now, sessions)
		return false, nil
	}
	for _, c := range s.portalMonitor().newSightings(now, sessions) {
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
	suspects, wait := s.portalMonitor().suspects(s.cfg.Now().UTC())
	if wait > 0 { // warming, or the sample has gone stale: either way retryable, never clean
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
	won, err := ps.ActivatePortalIssuersAs(r.Context(), store.PortalActivation{ActorType: string(actorType), Actor: actor, Sessions: names})
	if err != nil {
		writeServerError(w, r, "switch issuer portals on", err)
		return
	}
	if !won {
		// Another call set the mark between the read above and this write: it is the one the audit names.
		writeJSON(w, http.StatusOK, map[string]any{"activated": true, "already_activated": true})
		return
	}
	// The record is committed with the mark; a delivery that fails here is retried by every replica's monitor.
	s.deliverPortalActivation(r.Context(), ps)
	writeJSON(w, http.StatusOK, map[string]any{"activated": true, "overridden_sessions": len(suspects)})
}

// portalActivationRetryEvery is how long a replica leaves an activation whose rows it sent and could not see
// in the shared audit table before it sends them again, so a replica whose rows wait in its local spool does
// not append them again every tick.
const portalActivationRetryEvery = time.Minute

// deliverPortalActivation delivers the activation's audit rows while they are still owed, from the record the
// winning write committed: the actor, the sessions the operator overrode and each event's fixed id and time.
// Every delivery, by the activating request or any replica's monitor, is a replay of the same events
// (store.WithAuditReplay), so the audit table keeps one row each however often it runs.
//
// Delivery is not settlement. A recorder that returns nil or audit.ErrSpooled may hold the row only in this
// pod's local spool, which a replaced pod loses, so the record is settled only when the store reads both
// events, with their fixed times, in the shared audit table (SettlePortalActivation). Until then every
// replica sends what it does not see there.
func (s *Server) deliverPortalActivation(ctx context.Context, ps store.PortalActivationStore) {
	m := s.portalMonitor()
	rec, owed, err := ps.PendingPortalActivation(ctx)
	if err != nil {
		slog.WarnContext(ctx, "portal activation: read the owed audit rows", slog.Any("err", err))
		m.setOwed(true)
		return
	}
	if !owed {
		m.setOwed(false)
		return
	}
	m.setOwed(true)
	now := s.cfg.Now()
	if !m.mayRetry(now) {
		return
	}
	want := []store.AuditEventKey{{ID: rec.ActivateEventID, At: rec.EventAt}}
	if len(rec.Sessions) > 0 {
		want = append(want, store.AuditEventKey{ID: rec.OverrideEventID, At: rec.EventAt})
	}
	// An event counts as in the audit table only with the id and the event time the record fixed.
	stored := func() (map[uuid.UUID]bool, bool) {
		held, err := ps.AuditEventsStored(ctx, want)
		if err != nil {
			slog.WarnContext(ctx, "portal activation: read the audit table", slog.Any("err", err))
			return nil, false
		}
		in := map[uuid.UUID]bool{}
		for _, k := range want {
			if held[store.AuditEventKey{ID: k.ID, At: k.At.UTC().Truncate(time.Microsecond)}] {
				in[k.ID] = true
			}
		}
		return in, true
	}
	in, ok := stored()
	if !ok {
		return
	}
	ctx = store.WithAuditReplay(ctx)
	actorType := types.ActorType(rec.ActorType)
	if !in[rec.ActivateEventID] {
		activate := s.auditEvent(nil, actorType, rec.Actor, "portal_issuers.activate", "portal_issuers", "success",
			mustJSON(map[string]any{"old_servers_stopped": true, "sessions_seen": len(rec.Sessions)}))
		activate.ID, activate.Time = rec.ActivateEventID, rec.EventAt
		s.recordAuditDurably(ctx, activate)
	}
	if len(rec.Sessions) > 0 && !in[rec.OverrideEventID] {
		override := s.auditEvent(nil, actorType, rec.Actor, "portal_issuers.activate_override", "portal_issuers", "success",
			mustJSON(map[string]any{"sessions": rec.Sessions}))
		override.ID, override.Time = rec.OverrideEventID, rec.EventAt
		s.recordAuditDurably(ctx, override)
	}
	if in, ok = stored(); !ok || len(in) != len(want) {
		slog.WarnContext(ctx, "portal activation: the audit rows are not in the shared audit table yet, they stay owed")
		m.retryAfter(now.Add(portalActivationRetryEvery))
		return
	}
	settled, err := ps.SettlePortalActivation(ctx)
	if err != nil {
		slog.WarnContext(ctx, "portal activation: settle the audit rows", slog.Any("err", err))
		m.retryAfter(now.Add(portalActivationRetryEvery))
		return
	}
	if !settled {
		slog.WarnContext(ctx, "portal activation: the record was not settled, it stays owed")
		m.retryAfter(now.Add(portalActivationRetryEvery))
		return
	}
	m.setOwed(false)
}
