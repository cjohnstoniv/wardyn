// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runReportLostAction is the row the decision door writes on a run's trail when it answers one of the run's
// posts with neither a kept row nor a refusal row: a 503 (the run, or the clock the liveness gate decides by,
// could not be read) or a 401 for the run's expired token. The egress proxy does not post a report again, and
// its summary of what it dropped goes through the same door, so without this row a confined replay of the run
// could be judged clean without a decision it never saw (runDecisionPostRefused).
const runReportLostAction = "run.report.drop"

// The reasons a run.report.drop row carries.
const (
	reportLostUnreadable = reasonInternalLivenessReadFailed // a 503: the run or the gate's clock could not be read
	reportLostExpired    = "token_expired"                  // a 401: the run's token has expired
	reportLostInvalid    = "decision_log_invalid"           // a 400: the post's body did not decode
)

// expiredRunRead is the claim (claimDecisionRefusal) under which an expired token that lapsed more than
// identity.RunTokenLapseAfter ago buys one read of its run, so such a token costs at most one store read per
// run in that span.
const expiredRunRead = "run.report.drop:read"

// runUnreadableError is verifyRunToken's refusal of a revoked token at the decision door whose run could not
// be read, so it is not known whether the run is over. The run id is the token's verified one.
type runUnreadableError struct {
	RunID uuid.UUID
	Err   error
}

func (e *runUnreadableError) Error() string {
	return "read the run behind a revoked token: " + e.Err.Error()
}

func (e *runUnreadableError) Unwrap() error { return e.Err }

// refuseRunToken answers internalAuth's refusal of a run token. A token whose run could not be read is a
// retryable 503, as refuseTerminalRun's is; every other refusal is a 401.
//
// Do not leak the verification reason (revoked vs expired vs forged) to the caller. The auth.fail row is
// equally coarse, one reason for the whole verify failure, so the trail records THAT a run token was refused
// without telling a brute-forcer which half it got wrong. The 503 tells only a holder of a genuine revoked
// token that its run could not be read, and only while the store fails.
//
// At the decision door, a post whose token names its run and is refused here leaves one run.report.drop row
// on that run (noteDecisionReportLost): a run that cannot be read, or an expired token within the bound
// noteExpiredDecisionPost keeps.
func (s *Server) refuseRunToken(w http.ResponseWriter, r *http.Request, err error) {
	var unreadable *runUnreadableError
	if errors.As(err, &unreadable) {
		slog.ErrorContext(r.Context(), "api: read the run behind a revoked token at the decision door",
			slog.String("run_id", unreadable.RunID.String()), slog.Any("err", unreadable.Err))
		s.noteDecisionReportLost(r, unreadable.RunID, reportLostUnreadable)
		writeErrorReason(w, http.StatusServiceUnavailable, reasonInternalLivenessReadFailed, "read run failed; retry")
		return
	}
	var expired *identity.ExpiredTokenError
	if r.URL.Path == decisionsDoor && errors.As(err, &expired) {
		s.noteExpiredDecisionPost(r, expired)
	}
	s.auditAuthFailedAs(r, internalAuthActor, "invalid_run_token")
	s.auditRunIdentityExpired(r, err)
	writeErrorReason(w, http.StatusUnauthorized, reasonInvalidRunToken, "invalid run token")
}

// refuseDecisionLog answers a decision-door post whose body did not decode with a 400, and notes it on the
// run's trail (noteDecisionReportLost): the proxy counts a refused post among those it dropped, so it could
// have been a decision.
func (s *Server) refuseDecisionLog(w http.ResponseWriter, r *http.Request, runID uuid.UUID) {
	s.noteDecisionReportLost(r, runID, reportLostInvalid)
	writeErrorReason(w, http.StatusBadRequest, reasonInternalDecisionLogInvalid, "invalid decision log")
}

// answerDecisionPost answers a decision-door post: a 202 once its egress row was kept, written or held by the
// audit spool, and otherwise a retryable 503. The proxy counts that post among those it dropped, and its
// summary of them goes through this door.
func answerDecisionPost(w http.ResponseWriter, kept bool) {
	if !kept {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonInternalDecisionUnrecorded, "decision log not recorded; retry")
		return
	}
	writeJSON(w, http.StatusAccepted, nil)
}

// decisionGate is the writer internalAuth hands the liveness gate (refuseTerminalRun): at the decision door, a
// 503 the gate answers is noted on the run's trail (noteDecisionReportLost); every other door, and every
// other answer, is written as it is.
func (s *Server) decisionGate(w http.ResponseWriter, r *http.Request, claims *identity.Claims) http.ResponseWriter {
	if r.URL.Path != decisionsDoor {
		return w
	}
	return decisionGateWriter{ResponseWriter: w, s: s, r: r, runID: claims.RunID}
}

// decisionGateWriter is decisionGate's writer at the decision door.
type decisionGateWriter struct {
	http.ResponseWriter
	s     *Server
	r     *http.Request
	runID uuid.UUID
}

func (w decisionGateWriter) WriteHeader(code int) {
	if code == http.StatusServiceUnavailable {
		w.s.noteDecisionReportLost(w.r, w.runID, reportLostUnreadable)
	}
	w.ResponseWriter.WriteHeader(code)
}

// noteExpiredDecisionPost notes a decision-door post on an expired token, so that a holder of a dead token
// cannot fail a confined replay of a run that ended long ago. No token is minted for a run that is over (the
// renew handler refuses it), so every token of such a run expires at most RunTokenTTL after it ended:
//   - a token that expired within identity.RunTokenLapseAfter is noted at once;
//   - an older one is noted only while its run is not over, read at most once per run in that span
//     (expiredRunRead). A run that is over, or gone, writes no row; a read that fails writes none and is
//     tried again at the next post.
//
// So no such row lands on a run more than RunTokenTTL + RunTokenLapseAfter after it ended. The older arm keeps
// its own claim so that an old token's read cannot hold back the row a recent one writes.
func (s *Server) noteExpiredDecisionPost(r *http.Request, exp *identity.ExpiredTokenError) {
	if s.cfg.Now().Sub(exp.Expiry) < identity.RunTokenLapseAfter {
		s.noteDecisionReportLost(r, exp.RunID, reportLostExpired)
		return
	}
	if s.cfg.Store == nil || !s.claimDecisionRefusal(exp.RunID, expiredRunRead) {
		return
	}
	run, err := s.cfg.Store.GetRun(r.Context(), exp.RunID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.releaseDecisionRefusal(exp.RunID, expiredRunRead)
		}
		return
	}
	if !runIsOver(run, s.cfg.Now().UTC()) && !s.noteDecisionReportLost(r, exp.RunID, reportLostExpired) {
		s.releaseDecisionRefusal(exp.RunID, expiredRunRead)
	}
}

// noteDecisionReportLost writes the run.report.drop row on runID's trail, at most once per run in each
// identity.RunTokenLapseAfter in each server process (claimDecisionRefusal), and gives the claim back when the
// row was neither written nor spooled. One row fails a confined replay of the run; the rest would only repeat
// it. It reports false only when the row was claimed here and neither written nor spooled.
func (s *Server) noteDecisionReportLost(r *http.Request, runID uuid.UUID, reason string) bool {
	if !s.claimDecisionRefusal(runID, runReportLostAction) {
		return true
	}
	ev := s.auditEvent(&runID, types.ActorSystem, "wardynd", runReportLostAction, decisionsDoor, "failure",
		mustJSON(map[string]any{"reason": reason}))
	ev.SourceIP = r.RemoteAddr
	if !s.recordAuditDurably(r.Context(), ev) {
		s.releaseDecisionRefusal(runID, runReportLostAction)
		return false
	}
	return true
}
