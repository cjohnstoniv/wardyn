// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A service grant's audit rows and its status: one row per (provider, replica) in provider_service_grant_status,
// which GET /model-providers and a service identity's GET /setup/status read, and which the create door reads
// to refuse a run the grant cannot serve. Nothing here holds a token, a secret or the identity provider's words.

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	serviceGrantStateOK     = "ok"
	serviceGrantStateFailed = "failing"
	serviceGrantStateNotYet = "not_yet"

	auditServiceGrantMintAction = "model_provider.service_grant.mint"

	// serviceGrantStatusWriteBound bounds the detached status write of one attempt.
	serviceGrantStatusWriteBound = 5 * time.Second

	// mpServiceGrantRefusal is the create and dispatch refusal for a grant that cannot be minted.
	mpServiceGrantRefusal = "Wardyn could not get a token for this provider's service grant (%s); an administrator must check its client credentials"
	// mpServiceGrantNotListed is the sentence of a service identity the grant does not list.
	mpServiceGrantNotListed = "This provider's service grant is not available to this service identity; an operator lists the service identities that may use it"
)

// serviceGrantDefinitive reports whether a recorded reason is one a retry would repeat.
func serviceGrantDefinitive(reason string) bool {
	return reason != svcReasonUnavailable && reason != ""
}

// serviceGrantHost is this process's host name, for the status row's display column.
func serviceGrantHost() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

// writeServiceGrantStatus records this replica's outcome for the row: tok non-nil is a success, otherwise e is
// the failure. A discarded mint writes nothing (the caller never gets here).
func (s *Server) writeServiceGrantStatus(ctx context.Context, p types.ModelProvider, digest string, at time.Time, tok *serviceGrantMinted, e *serviceGrantError) {
	gs, ok := s.serviceGrantStore()
	if !ok {
		return
	}
	row := store.ServiceGrantStatusRow{
		ProviderUID: p.UID, ReplicaID: s.svcGrant.replica(), ReplicaHost: serviceGrantHost(), GrantDigest: digest,
		ClientID: p.OIDC.ServiceGrant.ClientID, AttemptedAt: at.UTC(),
	}
	if tok != nil {
		row.Outcome, row.Egress = "ok", tok.egress
		okAt, until := at.UTC(), tok.expiresAt.UTC()
		row.LastOKAt, row.ValidUntil = &okAt, &until
	} else if e != nil {
		row.Outcome, row.Reason, row.OAuthError, row.HTTPStatus, row.Egress = "failed", e.reason, e.oauthError, e.httpStatus, e.egress
	}
	// Under the attempt's context (detached from the caller, and bounded by the attempt), with its own bound inside
	// it: a status write that hangs must not hold the caller's lock or pass.
	wctx, cancel := context.WithTimeout(ctx, serviceGrantStatusWriteBound)
	defer cancel()
	if err := gs.UpsertServiceGrantStatus(wctx, row); err != nil {
		slog.WarnContext(ctx, "wardynd: could not record a service grant's status", slog.String("provider", p.ID), slog.Any("err", err))
	}
}

// serviceGrantMintEvent is the request's one mint row: a success (cause ""), a success marked superseded with
// superseded_by=cause, or a failure (e != nil), with a fixed id and time, prepared as the serving recorder chain
// prepares a row (cfg.RunnerAuditPrepare: target cap, then data and target masked against the global corpus).
// It never carries the token, the secret, error_description or the response body, and no request stamp: the
// mint is a shared system action.
func (s *Server) serviceGrantMintEvent(id uuid.UUID, at time.Time, p types.ModelProvider, trigger string, sent time.Time,
	tok *serviceGrantMinted, e *serviceGrantError, cause string,
) (types.AuditEvent, error) {
	if s.cfg.RunnerAuditPrepare == nil {
		return types.AuditEvent{}, errors.New("no audit preparation is configured")
	}
	g := p.OIDC.ServiceGrant
	host := ""
	if u, err := url.Parse(g.TokenURL); err == nil {
		host = u.Hostname()
	}
	data := map[string]any{
		"provider": p.ID, "provider_uid": p.UID, "client_id": g.ClientID, "token_host": host, "scope": g.Scope,
		"audience": g.Audience, "trigger": trigger, "replica": s.svcGrant.replica(),
	}
	outcome := "success"
	switch {
	case tok != nil:
		data["expires_at"] = tok.expiresAt.UTC().Format(time.RFC3339)
		data["egress"] = tok.egress
		if tok.scope != "" {
			data["scope"] = tok.scope
		}
	case e != nil:
		outcome = "failure"
		data["reason"], data["oauth_error"], data["http_status"], data["egress"] = e.reason, e.oauthError, e.httpStatus, e.egress
		func() {
			s.svcGrant.mu.Lock()
			defer s.svcGrant.mu.Unlock()
			if prev, ok := s.svcGrant.cache[p.UID]; ok && prev.expiresAt.After(sent) {
				data["previous_valid_until"] = prev.expiresAt.UTC().Format(time.RFC3339)
			}
		}()
	}
	if cause != "" {
		data["superseded"], data["superseded_by"] = true, cause
	}
	ev := types.AuditEvent{ID: id, Time: at, ActorType: types.ActorSystem, Actor: serviceGrantActor,
		Action: auditServiceGrantMintAction, Target: p.ID, Outcome: outcome, Data: mustJSON(data)}
	return s.cfg.RunnerAuditPrepare(context.Background(), ev)
}

// serviceGrantMintCandidates is the success row and the row for each cause of superseded_by, all with id and at.
func (s *Server) serviceGrantMintCandidates(id uuid.UUID, at time.Time, p types.ModelProvider, trigger string,
	sent time.Time, tok *serviceGrantMinted,
) (map[string]types.AuditEvent, error) {
	out := make(map[string]types.AuditEvent, len(serviceGrantSupersededCauses)+1)
	for _, cause := range append([]string{""}, serviceGrantSupersededCauses...) {
		ev, err := s.serviceGrantMintEvent(id, at, p, trigger, sent, tok, nil, cause)
		if err != nil {
			return nil, err
		}
		out[cause] = ev
	}
	return out, nil
}

// settleServiceGrantRow records ev off the attempt's path and closes the returned channel when done: the database
// row, or the spool when the database cannot take it. ambiguous marks a fallback for a publication whose COMMIT
// answer was lost: its sink copy is sent only as the row the database holds. It owns the request's permit and
// releases it once the row is recorded, spooled, or logged as lost or unconfirmed.
func (s *Server) settleServiceGrantRow(ev types.AuditEvent, ambiguous bool, release func()) <-chan struct{} {
	done := make(chan struct{})
	s.goBackground(func() {
		defer close(done)
		defer release()
		defer s.recoverServiceGrantSettle(ev.ID)
		s.svcGrant.stop("settle.begin")
		err := errors.New("no service grant store")
		if gs, ok := s.serviceGrantStore(); ok {
			sctx, c := context.WithTimeout(context.Background(), serviceGrantPublishBound)
			canonical, serr := gs.SettleServiceGrantMint(sctx, ev)
			c()
			if serr == nil {
				s.emitServiceGrantRow(canonical)
				return
			}
			err = serr
		}
		ambiguous = ambiguous || errors.Is(err, store.ErrServiceGrantCommitUnknown)
		var aerr error
		if s.cfg.AuditSpool != nil {
			spool := s.cfg.AuditSpool.Append
			if s.svcGrant.spoolAppend != nil {
				spool = s.svcGrant.spoolAppend
			}
			if aerr = spool(ev); aerr == nil {
				slog.Error("wardynd: AUDIT WRITE FAILED: the mint row is spooled", slog.String("action", ev.Action),
					slog.String("event_id", ev.ID.String()), slog.Any("err", err))
				if !ambiguous {
					s.emitServiceGrantRow(ev) // no hashes, as the recorder chain sends a row its store write lost
				} else {
					slog.Error("wardynd: service grant mint row's sink copy withheld: whether the publication committed is unknown; the database row is canonical",
						slog.String("event_id", ev.ID.String()))
				}
				return
			}
		}
		msg, disposition := "wardynd: AUDIT WRITE FAILED: the mint row is in neither the database nor a spool", "lost"
		switch {
		case aerr != nil:
			msg, disposition = "wardynd: AUDIT WRITE FAILED: AUDIT FALLBACK SPOOL FAILED (EVENT LOST): the mint row's line may still be in the spool", "unconfirmed"
		case ambiguous:
			msg, disposition = "wardynd: AUDIT WRITE FAILED: the mint row may be in the database; a commit's answer was lost", "unconfirmed"
		}
		attrs := []any{slog.String("event_id", ev.ID.String()), slog.String("disposition", disposition),
			slog.String("action", ev.Action), slog.String("outcome", ev.Outcome), slog.Any("err", err),
			slog.Bool("commit_unknown", ambiguous), slog.Bool("spool_append_failed", aerr != nil)}
		if aerr != nil {
			attrs = append(attrs, slog.Any("spool_err", aerr))
		}
		slog.Error(msg, attrs...)
	})
	return done
}

// recoverServiceGrantSettle ends a settle that panicked, in the store, the spool or a sink, with one ERROR line.
func (s *Server) recoverServiceGrantSettle(id uuid.UUID) {
	if r := recover(); r != nil {
		slog.Error("wardynd: a service grant mint row's settle panicked; the row may be lost",
			slog.String("event_id", id.String()), slog.Any("panic", r), slog.String("stack", string(debug.Stack())))
	}
}

var serviceGrantSIEMQueue = 64 // var so tests can shorten it

// serviceGrantSIEM is this replica's queue of mint-row sink copies. mu owns all of it, so each submission ends
// exactly once: taken by the worker for the sink, or dropped (queue full, replica stopping, or still queued at the stop).
type serviceGrantSIEM struct {
	mu      sync.Mutex
	q       []types.AuditEvent
	running bool      // a worker is draining q
	sending uuid.UUID // the copy the worker has taken for the sink; uuid.Nil when none
	stopped bool      // terminal: set once by StopServiceGrantSIEM, never cleared
	drops   int64
}

// serviceGrantSIEMStopping reports whether the queue takes no more copies. The caller holds the queue's mu.
func (s *Server) serviceGrantSIEMStopping() bool {
	return s.svcGrant.siem.stopped || (s.cfg.BaseCtx != nil && s.cfg.BaseCtx.Err() != nil)
}

// emitServiceGrantRow hands a recorded mint row to this replica's sink worker and never waits.
func (s *Server) emitServiceGrantRow(ev types.AuditEvent) {
	q := &s.svcGrant.siem
	q.mu.Lock()
	defer q.mu.Unlock()
	switch {
	case s.serviceGrantSIEMStopping():
		q.drops++
		slog.Warn("wardynd: a service grant mint row's sink copy was dropped: the replica is stopping",
			slog.String("event_id", ev.ID.String()), slog.String("copy", "dropped"))
		return
	case len(q.q) >= serviceGrantSIEMQueue:
		q.drops++
		slog.Warn("wardynd: a service grant mint row's sink copy was dropped: the queue is full",
			slog.String("event_id", ev.ID.String()), slog.String("copy", "dropped"))
		return
	}
	q.q = append(q.q, ev)
	if !q.running {
		q.running = true
		s.goBackground(s.serviceGrantSIEMWorker)
	}
}

// serviceGrantSIEMNext takes the queue's head for the sink, or ends the worker when the queue is empty or stopping.
func (s *Server) serviceGrantSIEMNext() (types.AuditEvent, bool) {
	q := &s.svcGrant.siem
	q.mu.Lock()
	defer q.mu.Unlock()
	if s.serviceGrantSIEMStopping() || len(q.q) == 0 {
		q.running, q.sending = false, uuid.Nil
		return types.AuditEvent{}, false
	}
	ev := q.q[0]
	q.q = q.q[1:]
	q.sending = ev.ID
	return ev, true
}

// serviceGrantSIEMWorker sends queued copies to the sink in queue order, and runs only while copies are queued.
func (s *Server) serviceGrantSIEMWorker() {
	q := &s.svcGrant.siem
	held := uuid.Nil
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		slog.Error("wardynd: a service grant mint row's sink copy panicked in the sink",
			slog.String("event_id", held.String()), slog.String("copy", "unconfirmed"),
			slog.Any("panic", r), slog.String("stack", string(debug.Stack())))
		q.mu.Lock()
		defer q.mu.Unlock()
		q.running, q.sending = false, uuid.Nil
		if len(q.q) > 0 && !s.serviceGrantSIEMStopping() {
			q.running = true
			s.goBackground(s.serviceGrantSIEMWorker)
		}
	}()
	for {
		ev, ok := s.serviceGrantSIEMNext()
		if !ok {
			return
		}
		held = ev.ID
		s.svcGrant.stop("siem.emit")
		if gs, ok := s.serviceGrantStore(); ok {
			if sink := gs.ServiceGrantSIEM(); sink != nil {
				_ = sink.Emit(context.WithoutCancel(s.cfg.BaseCtx), ev)
			}
		}
	}
}

// StopServiceGrantSIEM ends the queue for good: copies still queued are dropped, counted and logged, and every
// later copy is dropped on arrival. cmd/wardynd calls it before the audit sinks close. It reports whether this
// call did the stopping.
func (s *Server) StopServiceGrantSIEM() bool {
	q := &s.svcGrant.siem
	ids, sending, stopped := func() ([]string, uuid.UUID, bool) {
		q.mu.Lock()
		defer q.mu.Unlock()
		if q.stopped {
			return nil, uuid.Nil, false
		}
		q.stopped = true
		q.drops += int64(len(q.q))
		ids := make([]string, 0, len(q.q))
		for _, ev := range q.q {
			ids = append(ids, ev.ID.String())
		}
		q.q = nil
		return ids, q.sending, true
	}()
	if !stopped {
		return false
	}
	if len(ids) > 0 {
		slog.Warn("wardynd: service grant mint row sink copies were dropped at the stop",
			slog.Int("dropped", len(ids)), slog.Any("event_ids", ids), slog.String("copy", "dropped"))
	}
	if sending != uuid.Nil {
		slog.Warn("wardynd: a service grant mint row's sink copy may still reach the sink after the stop",
			slog.String("event_id", sending.String()), slog.String("copy", "unconfirmed"))
	}
	return true
}

// thisReplicaServiceGrantFailure is this replica's latest recorded outcome for the row under its live digest:
// the failed row's reason, or "" when there is none or it succeeded. It reads the status table, never mints.
func (s *Server) thisReplicaServiceGrantFailure(ctx context.Context, p types.ModelProvider) (string, error) {
	gs, ok := s.serviceGrantStore()
	if !ok {
		return "", nil
	}
	rows, err := gs.ServiceGrantStatuses(ctx, []string{p.UID})
	if err != nil {
		return "", err
	}
	digest := serviceGrantDigest(p)
	for _, r := range rows {
		if r.ReplicaID == s.svcGrant.replica() && r.GrantDigest == digest && r.Outcome == "failed" && serviceGrantDefinitive(r.Reason) {
			return r.Reason, nil
		}
	}
	return "", nil
}

// serviceGrantStatuses builds GET /model-providers' service_grants: for each provider holding a grant, the
// client, the derived secret name and what every replica last saw. State reads the live replicas' rows under
// the live digest only.
func (s *Server) serviceGrantStatuses(ctx context.Context, block *types.ModelProviders) (map[string]types.ServiceGrantStatus, error) {
	if block == nil {
		return nil, nil
	}
	var uids []string
	for _, p := range block.Providers {
		if p.OIDC != nil && p.OIDC.ServiceGrant != nil {
			uids = append(uids, p.UID)
		}
	}
	if len(uids) == 0 {
		return nil, nil
	}
	var rows []store.ServiceGrantStatusRow
	if gs, ok := s.serviceGrantStore(); ok {
		var err error
		if rows, err = gs.ServiceGrantStatuses(ctx, uids); err != nil {
			return nil, err
		}
	}
	out := map[string]types.ServiceGrantStatus{}
	for _, p := range block.Providers {
		if p.OIDC == nil || p.OIDC.ServiceGrant == nil {
			continue
		}
		out[p.ID] = serviceGrantStatusOf(p, rows)
	}
	return out, nil
}

// serviceGrantStatusOf aggregates one provider's rows: failing if any live replica's latest attempt failed, ok
// if every live replica's succeeded however long ago, not_yet if none has attempted.
func serviceGrantStatusOf(p types.ModelProvider, rows []store.ServiceGrantStatusRow) types.ServiceGrantStatus {
	digest := serviceGrantDigest(p)
	st := types.ServiceGrantStatus{
		ClientID: p.OIDC.ServiceGrant.ClientID, ClientSecretRef: serviceGrantSecretName(p.UID),
		State: serviceGrantStateNotYet, Replicas: []types.ServiceGrantReplicaStatus{},
	}
	live, failing := 0, false
	for _, r := range rows {
		if r.ProviderUID != p.UID || r.GrantDigest != digest {
			continue
		}
		rs := types.ServiceGrantReplicaStatus{
			Host: r.ReplicaHost, Live: r.Live, State: serviceGrantStateOK, AttemptedAt: r.AttemptedAt,
			LastOKAt: r.LastOKAt, ValidUntil: r.ValidUntil, Reason: r.Reason, OAuthError: r.OAuthError,
			HTTPStatus: r.HTTPStatus, Egress: r.Egress,
		}
		if r.Outcome == "failed" {
			rs.State = serviceGrantStateFailed
		}
		st.Replicas = append(st.Replicas, rs)
		if r.Live {
			live++
			failing = failing || rs.State == serviceGrantStateFailed
		}
	}
	switch {
	case failing:
		st.State = serviceGrantStateFailed
	case live > 0:
		st.State = serviceGrantStateOK
	}
	return st
}

// gradeProviderServiceGrant is a service identity's /setup/status row on a service-grant row: live, or
// not_configured with cause service_grant_failing while the aggregate is failing.
func (s *Server) gradeProviderServiceGrant(ctx context.Context, row *SetupProviderAccess, p types.ModelProvider) {
	row.State = modelAccessLive
	gs, ok := s.serviceGrantStore()
	if !ok {
		return
	}
	rows, err := gs.ServiceGrantStatuses(ctx, []string{p.UID})
	if err != nil {
		unreadableProvider(row)
		return
	}
	if serviceGrantStatusOf(p, rows).State == serviceGrantStateFailed {
		row.State, row.Cause = modelAccessNotConfigured, reasonServiceGrantFailing
		row.Action = "An administrator must check this provider's client credentials"
	}
}

// serviceGrantAppliesTo reports whether owner is a service identity the row's grant lists.
func serviceGrantAppliesTo(p types.ModelProvider, owner string) bool {
	return p.OIDC != nil && p.OIDC.ServiceGrant != nil && isServicePrincipal(owner)
}

// sortedCleanupUIDs orders claim uids for a stable listing.
func sortedCleanupUIDs(m map[string]store.ServiceGrantCleanup) []string {
	uids := make([]string, 0, len(m))
	for uid := range m {
		uids = append(uids, uid)
	}
	slices.Sort(uids)
	return uids
}
