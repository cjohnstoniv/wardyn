// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// terminalUploadGrace is how long after a run goes terminal its own tail
// uploads are still accepted.
//
// It is a GRACE, not a retention window, and it is sized by what actually runs
// in that span: wardyn-rec PUTs the finished cast as the session closes,
// wardyn-scan PUTs its ScanFacts as the scan process exits, and wardyn-aws-sso
// PUTs the captured token cache as the login run finishes — all of them
// milliseconds-to-seconds after the state flips, all of them racing the
// completion watcher that flips it. Five minutes is generous enough that a slow
// upload over a saturated link still lands and short enough that a stolen run
// token cannot be replayed into the trail an hour later.
const terminalUploadGrace = 5 * time.Minute

// terminalGraceRoutes are the /internal/* doors a TERMINAL run may still use,
// inside terminalUploadGrace. Matched on the path segment rather than on a chi
// route pattern because this runs as middleware, before the sub-router has
// bound one.
//
// The exemption is explicit and short by design. Every one of these is
// an UPLOAD of something the run produced and nothing else can produce: the
// asciicast, the scan facts, the captured SSO cache. None of them mints,
// injects or decides anything, so a terminal run walking through one cannot
// obtain a credential — which is the whole reason the other doors are shut.
// Adding a route here is adding a door a dead run may open; the test table in
// internal_live_run_test.go exists so that stays a deliberate act.
var terminalGraceRoutes = []string{
	"/internal/recordings/",
	"/internal/scan-results/",
	"/internal/sso-token/",
}

// internalSelfGatedRoutes are the /internal/* doors that run their OWN, STRICTER
// liveness check and therefore skip this one.
//
// There is exactly one, and it is the door this gate was generalised FROM.
// handleInternalTokenRenew re-reads the run, refuses a terminal one and a
// missing one with 403, refuses a store error with 503, refuses a nil store with
// 503 — and audits each refusal as `identity.renew`/denied, naming the retiring
// jti, which is the record an auditor reads for "a token asked to outlive its
// run". Running this gate in front of it would replace that row with a generic
// one and buy nothing, because renew's own check is stricter on every axis.
// Pinned by TestRenewU070_TerminalRunRefusedFailClosed.
var internalSelfGatedRoutes = []string{"/internal/token/renew"}

// refuseTerminalRun is internalAuth's liveness half: the run whose token
// authenticated this call must still be non-terminal. It writes its own refusal
// and returns false once it has.
//
// Why it lives in the middleware and not in the handlers. Exactly one
// /internal/* door re-checked run state — handleInternalTokenRenew — and its own
// doc comment explains why that check is not redundant with token verification:
// revokeRunCascade is best-effort, Identity.RevokeRun's error is audited and
// swallowed and Broker.RevokeRun is audit-only, so a run that went terminal
// while its revocation write failed still presents a token that Verify accepts.
// Renew closed that window for itself and left it open for the mint door, the
// injection resolve door and the approval doors — i.e. for every door that hands
// out a credential, including the operator's live Anthropic OAuth token, for up
// to tokenTTL after the run was killed. One gate in the one place every door
// passes through.
//
// No store configured is admitted, deliberately and the same way
// handleInternalTokenRenew does NOT do it: renew answers 503 there, because a
// renewal is a fresh grant of authority that must never be issued on an
// unverifiable one. This gate is a different question — it is asked of every
// call, including the ones that only report — and a deployment with no run store
// has no run lifecycle at all, so there is no terminal state to be past. Refusing
// would turn "no store" into "no internal surface", which is a behaviour change
// for every test double and every store-less embedding, none of which has a run
// that can die.
//
// A transient read failure is 503, not 403: renew's own reasoning applies
// unchanged — a Postgres blip must cost a retry, never a run's credentials.
//
// CALLED FROM internalAuth, immediately after Verify and AFTER the claims are on
// the context — the refusal audits against the run the token names, so it needs
// them there first. The call site is four lines and carries only a pointer back
// here, deliberately: http.go is at the file-size gate and the reasoning belongs
// with the function that acts on it, not with the middleware that calls it.
//
// Liveness is not authenticity. Verify answers "was this token minted by us, for
// this audience, unexpired and unrevoked". It cannot answer "is the run behind
// it still running", and a run that went terminal while its revocation write
// failed is exactly the case where those two answers differ.
func (s *Server) refuseTerminalRun(w http.ResponseWriter, r *http.Request, claims *identity.Claims) bool {
	if s.cfg.Store == nil || pathHasAny(r.URL.Path, internalSelfGatedRoutes) {
		return true
	}
	run, err := s.cfg.Store.GetRun(r.Context(), claims.RunID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.auditInternalDenied(r, claims, authz.ReasonRunNotFound, "")
			writeError(w, http.StatusForbidden, "run not found")
			return false
		}
		// The error goes to the LOG, never to the caller. This runs on every
		// /internal/* door, so the caller is the in-sandbox proxy — and raw pgx
		// text carries the database host, port, user and database name from a
		// dial failure. Same rule and same shape as writeServerError
		// (writeservererror.go); the status differs because that helper is the
		// 500 twin and this refusal is RETRYABLE: the sidecar should come back,
		// which is exactly what handleInternalTokenRenew's own 503 says.
		slog.ErrorContext(r.Context(), "api: read run for the /internal liveness gate",
			slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.Any("err", err))
		writeError(w, http.StatusServiceUnavailable, "read run failed; retry")
		return false
	}
	if !isTerminalRunState(run.State) {
		return true
	}
	if internalUploadWithinGrace(r.URL.Path, run, s.cfg.Now().UTC()) {
		return true
	}
	s.auditInternalDenied(r, claims, authz.ReasonRunTerminal, string(run.State))
	writeError(w, http.StatusForbidden, "run is terminal")
	return false
}

// internalUploadWithinGrace reports whether this is one of the tail-upload doors
// AND the run's row was written recently enough for its own tail to still be
// arriving.
//
// UpdatedAt is not a terminal timestamp, and the distinction is the honest
// caveat here. types.AgentRun carries only CreatedAt and UpdatedAt, and
// every terminal transition does bump UpdatedAt — but so does any LATER
// non-state write to the row. The boot reconciler clearing a dead run's
// sandbox_ref is the real example: it re-opens this window hours after the run
// died, for a run whose revocation write failed, which is exactly the case this
// gate exists for.
//
// What is no longer an example is the activity keepalive. The UI relay,
// both attach pumps and the SSH channel keepalives all called Store.TouchRun
// before the door that refuses a non-RUNNING run, so an authenticated caller
// could re-open this window on a cadence for as long as they liked — the one
// re-opener that was driven by a request rather than by a rare internal write.
// TouchRun now refuses a terminal run in SQL, so the remaining re-openers are
// wardynd's own writes, on the paths named above.
//
// It is accepted rather than closed because the exposure is bounded on three
// sides and closing it needs a schema column this lane does not own:
//
//   - all three graced doors are UPLOAD-ONLY. None mints, injects or decides
//     anything, so a re-opened window cannot yield a credential.
//   - each one re-checks the run for itself. The SSO-token door additionally
//     requires the run to be an aws-sso container login AND binds the blob to
//     the launch-time scope stamp, so a dead run cannot write anything the live
//     run could not have written.
//   - the caller still has to hold that run's own unexpired, unrevoked token.
//
// The durable fix is a terminal_at column (or having the reconciler skip the
// sandbox_ref bump for already-terminal rows); until then the window is
// measured from the row's last write and says so.
func internalUploadWithinGrace(path string, run types.AgentRun, now time.Time) bool {
	if !pathHasAny(path, terminalGraceRoutes) {
		return false
	}
	// A clock that has not been set (a zero Now, or a run row with no
	// UpdatedAt) must not silently widen the window: Sub on a zero time is a
	// very large positive duration, so the comparison is written to fail closed
	// on one rather than open. A FUTURE timestamp fails closed for the same
	// reason (the age check below is >= 0).
	if run.UpdatedAt.IsZero() {
		return false
	}
	age := now.Sub(run.UpdatedAt)
	return age >= 0 && age <= terminalUploadGrace
}

// auditInternalDenied records a REFUSED /internal/* call. A token outliving its
// run's authority is security-relevant whether it is a dead sidecar or a
// replayed steal, so the refusal is never silent — the same shape and the same
// reasoning as auditRenewDenied, which this generalises.
// The reason is a CLOSED value and the run's state rides beside it as its own
// datum rather than being concatenated into it: a SIEM rule is written against
// the enum docs/OPERATIONS.md publishes, and `run_terminal:KILLED` is not a
// member of that enum — `run_terminal` is, with `run_state` saying which
// terminal state was found. runState is "" for the reasons that have none.
func (s *Server) auditInternalDenied(r *http.Request, claims *identity.Claims, reason authz.Reason, runState string) {
	d := authz.Deny(reason, r.URL.Path, "").OnRun(claims.RunID).With("actor", internalAuthActor)
	if runState != "" {
		d = d.With("run_state", runState)
	}
	ev := s.refusalEvent(r.Context(), types.ActorAgent, claims.SPIFFEID, r.Method, d)
	ev.SourceIP = r.RemoteAddr
	s.recordAudit(r.Context(), ev)
}

// pathHasAny reports whether path contains any of the segments. A substring
// match rather than a chi route pattern because this runs as middleware, before
// the sub-router has bound one; the segments all carry their surrounding
// slashes so "/internal/recordings/" cannot match a suffix of some other route.
func pathHasAny(path string, segments []string) bool {
	for _, seg := range segments {
		if strings.Contains(path, seg) {
			return true
		}
	}
	return false
}
