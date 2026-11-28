// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runnerHoldReader is the store's runner-containment read, the one the runner lease refusal makes
// (store.PG.RunHeldContainedOnRunner): the recover door calls it, never a copy of it.
type runnerHoldReader interface {
	RunHeldContainedOnRunner(ctx context.Context, runID uuid.UUID) (bool, error)
}

var _ runnerHoldReader = store.PG{}

// recoveryStore is the store the recover door reads: a store without every part of it recovers nothing.
type recoveryStore interface {
	store.RunTokenRecoverer
	store.OutageWindows
	runnerHoldReader
}

// recoveryStoreOf is the server's store as a recoveryStore; ok is false when it lacks a part.
func (s *Server) recoveryStoreOf() (recoveryStore, bool) {
	st, ok := s.cfg.Store.(recoveryStore)
	return st, ok
}

// recoverySupported is whether this server can recover a run token at all: its identity provider verifies an
// expired token (identity.RecoveryVerifier) and its store has every read the door makes. A proxy's pause is only
// recorded where it holds (handleInternalProxyLoaded), so a server that cannot recover holds no run for recovery.
func (s *Server) recoverySupported() bool {
	if _, ok := s.cfg.Identity.(identity.RecoveryVerifier); !ok {
		return false
	}
	_, ok := s.recoveryStoreOf()
	return ok
}

// proxyReportsPause is whether a proxy's loaded report says it pauses and recovers: its switches decode to an object
// whose outage_pause is "on". A report from an older proxy has no such key.
func proxyReportsPause(switches json.RawMessage) bool {
	var sw map[string]string
	return len(switches) > 0 && json.Unmarshal(switches, &sw) == nil && sw["outage_pause"] == "on"
}

// mountTokenRecoverRoute registers POST /internal/token/recover with no auth middleware, so it is listed in
// internalRoutesWithoutAuth, the closed set of /api/v1/internal/ routes without one that
// TestEveryInternalRouteWithoutAuthIsAnException checks. It is the only route that accepts an expired run token, and
// it verifies that token itself, through identity.RecoveryVerifier. A runner's proxy reaches it through its relay.
func (s *Server) mountTokenRecoverRoute(r chi.Router) {
	r.Post("/internal/token/recover", s.handleInternalTokenRecover)
}

// handleInternalTokenRecover gives a run whose token expired while the control plane could not be reached a fresh
// token (#4082). It checks what renew checks, plus a recorded outage window that covers the token's expiry. A 403 or
// 501 is final, and the proxy stops asking; every other answer is retryable.
func (s *Server) handleInternalTokenRecover(w http.ResponseWriter, r *http.Request) {
	verifier, ok := s.cfg.Identity.(identity.RecoveryVerifier)
	if !ok {
		s.auditAuthFailedAs(r, internalAuthActor, reasonRecoveryUnsupported)
		writeErrorReason(w, http.StatusNotImplemented, reasonRecoveryUnsupported, "this server cannot recover a run token")
		return
	}
	tok, ok := bearerToken(r)
	if !ok {
		s.auditAuthFailedAs(r, internalAuthActor, "missing_run_token")
		writeErrorReason(w, http.StatusUnauthorized, reasonMissingRunToken, "missing run token")
		return
	}
	claims, err := verifier.VerifyExpired(r.Context(), tok, internalAudience, store.RecoveryLimit)
	var revoked *identity.RevokedTokenError
	switch {
	case errors.Is(err, identity.ErrNotExpired):
		writeErrorReason(w, http.StatusConflict, reasonTokenNotExpired, "the run token has not expired; renew it")
		return
	case errors.Is(err, identity.ErrTokenTooOld):
		s.auditAuthFailedAs(r, internalAuthActor, reasonTokenTooOld)
		writeErrorReason(w, http.StatusForbidden, reasonTokenTooOld, "the run token expired more than 24 hours ago; revive the run")
		return
	case errors.As(err, &revoked):
		if revoked.Claims != nil {
			s.auditRecoverDenied(r, revoked.Claims, reasonRunRevoked)
		}
		writeErrorReason(w, http.StatusForbidden, reasonRunRevoked, "the run's tokens were revoked")
		return
	case err != nil:
		s.refuseRunToken(w, r, err) // 401, retryable: a signature, audience or revocation-store failure
		return
	}
	if _, ok := s.recoveryStoreOf(); !ok {
		s.auditRecoverDenied(r, claims, reasonRecoveryUnsupported)
		writeErrorReason(w, http.StatusNotImplemented, reasonRecoveryUnsupported, "this server cannot recover a run token")
		return
	}
	r = r.WithContext(contextWithClaims(r.Context(), claims))
	run, err := s.cfg.Store.GetRun(r.Context(), claims.RunID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.auditRecoverDenied(r, claims, reasonRunNotFound)
		writeErrorReason(w, http.StatusForbidden, reasonRunNotFound, "run not found")
		return
	case err != nil:
		writeErrorReason(w, http.StatusServiceUnavailable, reasonRecoveryReadFailed, loggedMsg(r.Context(), "read run", err))
		return
	}
	if s.refuseRelayBinding(w, r, claims, &run) {
		return
	}
	s.recoverRunToken(r.Context(), w, r, claims, run)
}

// recoverRefusal is one answer of the door after the token is verified: its status and message. A 403 is final.
type recoverRefusal struct {
	status int
	msg    string
}

// recoverRefusals are the door's refusals by audit reason. The run-state ones the renew door shares answer with its
// wire reasons (recoverWireReason).
var recoverRefusals = map[string]recoverRefusal{
	reasonRunNotFound:           {http.StatusForbidden, "run not found"},
	reasonRunNotRunning:         {http.StatusForbidden, "the run is not running"},
	reasonRunPastLifetime:       {http.StatusForbidden, "the run is past its end or its lifetime cap"},
	reasonRunTokenCut:           {http.StatusForbidden, "the run's token lineage was cut when it was stopped; only a revive runs it again"},
	reasonRunContainmentPending: {http.StatusForbidden, "run is containment pending: its runner deleted it and is waiting for proof that it stopped"},
	reasonRunHeldContained:      {http.StatusForbidden, "the run's runner holds it contained"},
	reasonTokenTooOld:           {http.StatusForbidden, "the run token expired more than 24 hours ago; revive the run"},
	reasonStrictAllowanceSpent:  {http.StatusForbidden, "the run's strict outage allowance has run out"},
	reasonTokenLineageRetired:   {http.StatusServiceUnavailable, "the presented token is not the run's current token"},
	reasonOutageWindowPending:   {http.StatusServiceUnavailable, "no recorded outage covers the token's expiry yet; retry"},
	reasonRecoveryReadFailed:    {http.StatusServiceUnavailable, "a read the recovery needs failed; retry"},
}

// recoverRunToken is the one function that holds the recovery's checks, its mint, its stamp and its lease. The final
// checks come first, so a final fact is reported even when a retryable one also holds.
func (s *Server) recoverRunToken(ctx context.Context, w http.ResponseWriter, r *http.Request, claims *identity.Claims, run types.AgentRun) {
	st, _ := s.recoveryStoreOf() // handleInternalTokenRecover has checked it
	refuse := func(reason string) { s.refuseRecover(w, r, claims, reason) }
	readFailed := func(what string, err error) {
		slog.WarnContext(ctx, "wardynd: a run token recovery read failed", slog.String("run_id", run.ID.String()),
			slog.String("read", what), slog.Any("err", err))
		refuse(reasonRecoveryReadFailed)
	}
	now, err := s.dbNow(ctx)
	if err != nil {
		readFailed("the database's clock", err)
		return
	}
	if reason := recoverRunStateRefusal(run, now); reason != "" {
		refuse(reason)
		return
	}
	// The lineage is read after checks 1-4, and only a row that is still there passes: a run gone since GetRun is
	// refused, never read as an empty lineage, which check 9 would accept.
	lineage, err := st.RunRecoveryLineage(ctx, run.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		refuse(reasonRunNotFound)
		return
	case err != nil:
		readFailed("the run's token lineage", err)
		return
	case strings.HasPrefix(lineage.JTI, store.TokenCutPrefix):
		refuse(reasonRunTokenCut)
		return
	}
	if pending, perr := s.runContainmentPending(ctx, run); perr != nil {
		readFailed("run containment", perr)
		return
	} else if pending {
		refuse(reasonRunContainmentPending)
		return
	}
	if run.RunnerID != nil {
		held, herr := st.RunHeldContainedOnRunner(ctx, run.ID)
		if herr != nil {
			readFailed("runner containment", herr)
			return
		}
		if held {
			refuse(reasonRunHeldContained)
			return
		}
	}
	if now.Sub(claims.Expiry) > store.RecoveryLimit {
		refuse(reasonTokenTooOld)
		return
	}
	check, err := st.RunRecoveryCheck(ctx, run.ID, false)
	if err != nil {
		readFailed("the run's recovery facts", err)
		return
	}
	if check.StrictSpent {
		refuse(reasonStrictAllowanceSpent)
		return
	}
	if lineage.JTI != "" && claims.JTI != lineage.JTI {
		refuse(reasonTokenLineageRetired)
		return
	}
	win, covered, err := st.OutageWindowCovers(ctx, run.RunnerID, run.Placement, claims.Expiry)
	if err != nil {
		readFailed("outage windows", err)
		return
	}
	if !covered {
		w.Header().Set("Retry-After", "60")
		refuse(reasonOutageWindowPending)
		return
	}

	// Every check passed: what renew does, in its order.
	sponsor := claims.Sponsor
	if isServicePrincipal(claims.Sub) {
		var serr error
		sponsor, serr = s.runSponsor(ctx, claims.Sub)
		if errors.Is(serr, errServiceRetired) {
			s.auditRecoverDenied(r, claims, "service_identity_retired")
			writeErrorReason(w, http.StatusForbidden, reasonServiceIdentityInactive, "the service identity that started this run is retired")
			return
		}
		if serr != nil {
			writeServerError(w, r, "recover run identity", serr)
			return
		}
	}
	releaseLease, busy := s.renewalLeaseSlot(ctx, run)
	if busy {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonRunRenewLeaseBusy, "the run's runner lease is being sent; retry the recovery")
		return
	}
	defer releaseLease()
	id, err := s.mintRunIdentity(ctx, claims.RunID, runWorkloadOf(claims.SPIFFEID, claims.RunID), claims.Sub, sponsor, claims.OperatorOwned)
	if err != nil {
		writeServerError(w, r, "recover run identity", err)
		return
	}
	// The stamp hands the run to the new token and cuts the presented one from the lineage; the token is handed out
	// only once it lands. It races a loss mark: both are single-row updates, and exactly one wins.
	stamped, err := st.StampRunTokenRecovered(ctx, run.ID, id.JTI, claims.JTI)
	if err != nil {
		readFailed("the recovery stamp", err)
		return
	}
	if !stamped {
		s.refuseRecoverRaced(ctx, w, r, claims, st, now)
		return
	}
	switch err := s.sendRenewedLease(ctx, run); {
	case errors.Is(err, errRunnerLeaseBusy):
		s.deferRenewedLease(run.ID)
	case err != nil:
		slog.WarnContext(ctx, "wardynd: sending a recovered run's runner lease failed",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
	}

	ev := s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID, "run.token.recover", id.JTI, "success", mustJSON(map[string]any{
		"prev_jti":      claims.JTI,
		"expired_for_s": int64(now.Sub(claims.Expiry) / time.Second),
		"window_kind":   win.Kind,
		"window_id":     win.ID,
		"expires_at":    id.Expiry.UTC().Format(time.RFC3339),
	}))
	ev.SourceIP = r.RemoteAddr
	s.recordAudit(r.Context(), ev)
	writeJSON(w, http.StatusOK, tokenRenewResponse{Token: id.Token, ExpiresAt: id.Expiry.UTC().Format(time.RFC3339)})
}

// recoverRunStateRefusal is the first of checks 1, 2, 3 and 4 on the run row that fails for run, "" when none does.
// Check 4b, a lineage cut at a stopped pause, follows on the lineage read after it: only a revive's fresh token runs
// such a run again.
func recoverRunStateRefusal(run types.AgentRun, now time.Time) string {
	switch {
	case isTerminalRunState(run.State):
		return "run_terminal:" + string(run.State)
	case run.State != types.RunRunning:
		return reasonRunNotRunning
	case run.LifetimeCapAt != nil && pastLifetimeCap(run, now), run.EndsAt != nil && now.After(*run.EndsAt):
		return reasonRunPastLifetime
	case runIsKept(run):
		return "run_lost:" + string(run.LostReason)
	}
	return ""
}

// refuseRecoverRaced answers a stamp that did not land: the row changed after the checks. A fresh read answers
// with the first failing check 1, 2, 4, 4b or 9, or run_not_found when the run's row is gone; when none fails (the row
// changed back), the answer is retryable. No token is handed out on any of these.
func (s *Server) refuseRecoverRaced(ctx context.Context, w http.ResponseWriter, r *http.Request, claims *identity.Claims, st recoveryStore, now time.Time) {
	run, err := s.cfg.Store.GetRun(ctx, claims.RunID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.refuseRecover(w, r, claims, reasonRunNotFound)
		return
	case err != nil:
		s.refuseRecover(w, r, claims, reasonRecoveryReadFailed)
		return
	}
	lineage, err := st.RunRecoveryLineage(ctx, run.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.refuseRecover(w, r, claims, reasonRunNotFound)
		return
	case err != nil:
		s.refuseRecover(w, r, claims, reasonRecoveryReadFailed)
		return
	}
	reason := recoverRunStateRefusal(run, now)
	if reason == reasonRunPastLifetime { // check 3 is not re-asked here
		reason = ""
	}
	switch {
	case reason != "":
	case strings.HasPrefix(lineage.JTI, store.TokenCutPrefix):
		reason = reasonRunTokenCut
	case lineage.JTI != "" && claims.JTI != lineage.JTI:
		reason = reasonTokenLineageRetired
	default:
		reason = reasonRecoveryReadFailed
	}
	s.refuseRecover(w, r, claims, reason)
}

// refuseRecover writes one refusal of a verified token: the identity.recover denied row (auditRecoverDenied), then
// the answer. A terminal or kept run answers with the renew door's wire reasons.
func (s *Server) refuseRecover(w http.ResponseWriter, r *http.Request, claims *identity.Claims, reason string) {
	s.auditRecoverDenied(r, claims, reason)
	switch {
	case strings.HasPrefix(reason, "run_terminal:"):
		writeErrorReason(w, http.StatusForbidden, string(authz.ReasonRunTerminal), "run is terminal")
	case strings.HasPrefix(reason, "run_lost:"):
		writeErrorReason(w, http.StatusForbidden, string(authz.ReasonRunKept), "run is lost")
	default:
		ref, ok := recoverRefusals[reason]
		if !ok {
			ref = recoverRefusals[reasonRecoveryReadFailed]
		}
		writeErrorReason(w, ref.status, reason, ref.msg)
	}
}

// auditRecoverDenied records a refused recovery as identity.recover denied, as auditRenewDenied records a refused
// renew, but at most one row per run and reason per identity.RunTokenLapseAfter (claimDecisionRefusal): a proxy
// asking every minute through a long outage writes one row per reason, not one per ask. The answer is unchanged.
func (s *Server) auditRecoverDenied(r *http.Request, claims *identity.Claims, reason string) {
	if !s.claimDecisionRefusal(claims.RunID, "identity.recover:"+reason) {
		return
	}
	ev := s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"identity.recover", claims.JTI, "denied", mustJSON(map[string]any{"reason": reason}))
	ev.SourceIP = r.RemoteAddr
	s.recordAudit(r.Context(), ev)
}
