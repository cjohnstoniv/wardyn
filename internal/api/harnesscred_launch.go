// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// THE LOGIN LAUNCH, SPLIT AT THE RUN ID (P5, 0.7.3 field report).
//
// POST /setup/harness-login used to run the WHOLE launch inside the request:
// CreateRun, the audit stamp, then dispatchRun — which blocks on CreateSandbox
// for as long as the substrate needs. On k8s that is canaryWaitTimeout (3 min,
// canary.go) ON TOP of a cold image pull; the reporting estate measured 131 s.
// The console's own deadline is 60 s (WFETCH_TIMEOUT_MS, lib/api/core.ts), so
// the pane showed the unreachable-daemon sentence, `setRunId` never ran, and
// Cancel had nothing to kill: the sandbox was orphaned to harnessLoginIdleCap.
//
// The split point is the run id. Everything the CALLER needs — the id it polls,
// attaches and cancels with, and the launch-time scope/pin stamp the capture
// upload binds to — is written by launchHarnessLoginRun before this file
// answers. Everything that can block is finishHarnessLoginLaunch's, on a
// context.WithoutCancel so the client's own disconnect cannot strand a
// half-provisioned sandbox.
//
// WHY A KILL LANDING DURING THE PULL IS SAFE: dispatchRun claims
// PENDING->STARTING with a CAS (runs_dispatch.go), so a POST /runs/{id}/kill
// that wins the race makes dispatch ABORT rather than resurrect the run, and
// stampRunWatcherLease runs before CreateSandbox, so no reconcile sweep adopts
// a run that is merely pulling slowly.
//
// (This file exists rather than more of harnesscred.go because that file is one
// line under the 1000-line cap: see scripts/check-file-size.sh.)

// harnessLoginDispatch is what launchHarnessLoginRun composed and the detached
// tail below still needs. Named fields, not a second dispatchParams: these four
// are the login lane's own answers, and dispatchParams has a dozen more that a
// login box deliberately never sets (no injections, no repo, no verify plan).
type harnessLoginDispatch struct {
	RunToken string
	Image    string
	Policy   types.RunPolicySpec
	ExtraEnv map[string]string
}

// DRAFT (M2 canon pending) — the run's failure_hint when the dispatch ceiling
// cannot be resolved AFTER the run row exists. Before P5 this was a 500 with no
// run to speak of; now the caller already holds a 200 and a run id, so the run
// itself has to carry the reason or it sits PENDING forever with no hint and no
// watcher lease. Mirrors dispatchRun's own unresolved-ceiling sentence
// (runs_dispatch.go), in this lane's words.
const harnessLoginCeilingUnresolved = "this sign-in sandbox was not launched: Wardyn could not resolve the governance ceiling that bounds it — try again, and tell your admin if it keeps failing"

// DRAFT (M2 canon pending) — the run's failure_hint when the detached launch
// PANICS. It needs its own sentence: the panic window is almost entirely inside
// dispatchRun, so the ceiling sentence above would tell an operator Wardyn could
// not resolve a ceiling it in fact resolved, which is both untrue and
// un-actionable. Names the class (an internal error, not their configuration)
// and where the detail is, since a panic's own text is a stack trace nobody
// should read off a console banner.
const harnessLoginInternalError = "This sign-in sandbox hit an internal error while starting — try again; if it repeats, check the daemon log."

// handleHarnessLogin launches a container-login sandbox for a provider:
//
//	POST /api/v1/setup/harness-login  {provider}
//
// RBAC: the signed-in-human group, with the operator-or-per_user-row predicate
// INSIDE the handler (authorizeHarnessLogin). It is not operatorOnly any more
// because under a per_user roster row the whole point is that each person signs
// in themselves — a member who cannot reach this route has no route to model
// access at all.
func (s *Server) handleHarnessLogin(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Secrets == nil {
		writeError(w, http.StatusServiceUnavailable, "no secret store configured; managed harness login unavailable")
		return
	}
	var req harnessLoginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	provider := strings.TrimSpace(req.Provider)
	if provider == "" {
		provider = "anthropic"
	}
	hl, ok := harnessLoginByProvider(provider)
	if !ok {
		writeError(w, http.StatusBadRequest, "provider does not support container login in this version: "+provider)
		return
	}
	row, scope, allowed := s.authorizeHarnessLogin(w, r, provider)
	if !allowed {
		return
	}
	// AWS: `aws sso login` cannot run at all without an sso_start_url + sso_region
	// in the sandbox's ~/.aws/config. The region is boot config; the start URL is
	// the request's ONLY in legacy mode, where the operator is the sole caller and
	// there is nowhere else to keep it. Refuse up front rather than launching a
	// sandbox whose auto-typed command is guaranteed to fail.
	startURL := strings.TrimSpace(req.SSOStartURL)
	if row.SSOStartURL != "" {
		// ADMIN-OWNED, and it OVERRIDES the request rather than merely defaulting
		// it. A per_user row means many people sign in, and the capture is bound to
		// whatever portal the launch was seeded with (ssotoken.go's F006 check
		// compares the blob to THIS run's own audit record) — so honouring a
		// caller-supplied start URL would let anyone bind their capture to an
		// IdP/account of their choosing and have Wardyn bake it into every later
		// Bedrock run's ~/.aws/config. It also takes an org URL off the member's
		// typing surface entirely.
		startURL = row.SSOStartURL
	}
	if hl.regionalSSOEgress {
		if startURL == "" {
			writeError(w, http.StatusBadRequest,
				"aws sso login needs your organization's AWS access portal URL (e.g. https://my-org.awsapps.com/start); Wardyn has no stored copy of it")
			return
		}
		if verr := validateSSOStartURL(startURL); verr != nil {
			writeError(w, http.StatusBadRequest, verr.Error())
			return
		}
		if cmp.Or(s.cfg.BedrockAWSSSORegion, s.cfg.BedrockRegion) == "" {
			writeError(w, http.StatusBadRequest,
				"no AWS SSO region is configured; set -bedrock-aws-sso-region (WARDYN_BEDROCK_AWS_SSO_REGION) or -bedrock-region and restart wardynd")
			return
		}
	}
	_, actor := actorFromRequest(r)
	// Everything up to the run row and its audit stamp is still synchronous, so
	// every genuinely PRE-CreateRun refusal keeps the status code it had: no
	// runner / no capabilities / no confinement class, the governance limit
	// below, the roster fail-closed arm and the admin-token-under-per_user arm
	// all answer here, with no run to show for it either way.
	run, dispatch, err := s.launchHarnessLoginRun(r.Context(), actor, hl, startURL,
		awsSSOPin{AccountID: row.SSOAccountID, RoleName: row.SSORoleName}, scope)
	if err != nil {
		// A governance limit is the acting principal's own profile refusing, not a
		// daemon fault — answered the way launchRecordRun's caller answers it
		// (record.go), with the profile's own sentence and no 500.
		if errors.Is(err, errRecordCeilingLimit) {
			writeError(w, http.StatusForbidden, strings.TrimPrefix(err.Error(), errRecordCeilingLimit.Error()+": "))
			return
		}
		writeServerError(w, r, "launch login sandbox", err)
		return
	}
	// ANSWER FIRST, then finish the launch. WithoutCancel keeps the request's
	// values — the ceiling memo above all, so the dispatch axis resolves exactly
	// the ceiling the Limits axis already bound — while dropping the deadline
	// that dies with this response.
	writeJSON(w, http.StatusOK, harnessLoginResponse{RunID: run.ID.String(), State: string(run.State)})
	go s.finishHarnessLoginLaunch(context.WithoutCancel(r.Context()), run, dispatch)
}

// finishHarnessLoginLaunch is the part of the launch that can block: resolve the
// dispatch ceiling, then dispatchRun (CreateSandbox, the STARTING->RUNNING CAS,
// run.interactive). It runs detached, after the caller already holds a 200 and
// a run id.
//
// THE CEILING ERROR IS THE MISSED DOOR. resolveDispatchCeiling errors AFTER the
// run row and its audit stamp exist. Synchronously that was a 500 and the
// caller knew the launch had failed; detached, returning would leave a 200
// already answered, a run PENDING forever, no failure_hint for the pane to show
// and no watcher lease for a sweep to adopt. So it fails the run the way
// dispatchRun fails its own unresolved-ceiling arm — failAndRevoke from
// PENDING, which revokes the run identity and writes the terminal state.
//
// Unreachable through the HTTP route as things stand (effectiveCeiling memoizes
// per request and launchHarnessLoginRun already resolved it, so a second error
// cannot appear out of a successful first), and kept anyway for the same reason
// dispatchRun keeps its zero-ceiling arm: the cost of the guard is four lines
// and the cost of its absence is a silently stranded run.
func (s *Server) finishHarnessLoginLaunch(ctx context.Context, run types.AgentRun, d harnessLoginDispatch) {
	// Contain a panic in a detached goroutine so a launch bug cannot take the
	// daemon down with it (same idiom as startCompletionWatcher).
	//
	// FAIL FROM THE RUN'S CURRENT STATE, re-read. `from` is a CAS precondition,
	// and by the time anything here can realistically panic dispatchRun has
	// already CASed PENDING->STARTING (runs_dispatch.go) — so a hard-coded
	// RunPending would not apply, and failAndRevoke's non-applied path is SILENT
	// (no log, no audit, no revoke). The run would sit STARTING with a live
	// identity until a reconcile sweep. One GetRun turns that into the terminal
	// state plus the identity revocation this arm exists for; if even that read
	// fails there is nothing better to do than fall back to PENDING.
	defer func() {
		if rec := recover(); rec != nil {
			slog.ErrorContext(ctx, "wardynd: harness login launch panicked",
				slog.String("run_id", run.ID.String()), slog.Any("panic", rec))
			from := types.RunPending
			if s.cfg.Store != nil {
				if got, gerr := s.cfg.Store.GetRun(ctx, run.ID); gerr == nil && !isTerminalRunState(got.State) {
					from = got.State
				}
			}
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.dispatch",
				run.ID.String(), "failure", mustJSON(map[string]any{
					"note": "the detached harness-login launch panicked; the run is failed rather than left non-terminal",
				})))
			s.failAndRevoke(ctx, run.ID, from, harnessLoginInternalError)
		}
	}()
	// The acting principal's ceiling, for the dispatch DENY axis. It was resolved
	// rather than exempted precisely so that re-tiering this route would simply
	// start binding instead of leaving a door open — which is what happened: the
	// login lane is member-reachable under a per_user row now, and this reads the
	// member's own ceiling with no change. The memo makes it the same resolution
	// the Limits axis already made.
	dc, _, dcErr := s.resolveDispatchCeiling(ctx)
	if dcErr != nil {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.dispatch",
			run.ID.String(), "failure", mustJSON(map[string]any{"error": dcErr.Error()})))
		s.failAndRevoke(ctx, run.ID, types.RunPending, harnessLoginCeilingUnresolved)
		return
	}
	s.dispatchRun(ctx, run, dc, dispatchParams{
		RunToken: d.RunToken, Image: d.Image, Policy: d.Policy,
		Interactive: true, ExtraEnv: d.ExtraEnv,
	})
}
