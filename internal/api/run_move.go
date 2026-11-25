// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/hoptls"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Moving a paused run to another machine: POST /runs/{id}/move and GET /runs/{id}/placements, for the run's owner
// or a super admin. Both answer 409 move_unavailable while moving a run is off (moveAvailable), which it is on
// every server until the release that can move runs, and a refused move writes one run.move denied row.

// errMoveUnavailable is startMove's answer while no move can be made.
var errMoveUnavailable = errors.New("api: moving a run is not available")

// registerRunMoveRoutes mounts the move's routes on the authenticated run router.
func (s *Server) registerRunMoveRoutes(r chi.Router) {
	r.Post("/runs/{id}/move", s.handleMoveRun)
	r.Get("/runs/{id}/placements", s.handleRunPlacements)
}

// handleMoveRun serves POST /runs/{id}/move {to}: the owner, or a super admin, moves a run. Another caller gets
// the 404 a missing run gets, so the route is no existence oracle; then a move that is off answers
// move_unavailable before anything else is read. An admitted move answers 202 with its row; a refusal answers its
// reason, with the layer a destination refusal names.
func (s *Server) handleMoveRun(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorizedBy(w, r, id, s.ownsRunOrSuperAdmin)
	if !ok {
		return
	}
	if !s.moveOn(r.Context()) {
		s.refuseMoveUnavailable(w, r, run)
		return
	}
	var body struct {
		To string `json:"to"`
	}
	if msg := decodeStrictMsg(w, r, &body); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	actorType, principal := actorFromRequest(r)
	to, err := types.ParseMoveDestination(body.To)
	if err != nil {
		to = types.MoveDestination{}
	}
	mv, err := s.startMove(r.Context(), moveRequest{Run: run, To: to, ActorType: actorType, Principal: principal})
	var ref *moveRefusal
	switch {
	case errors.Is(err, errMoveUnavailable):
		s.refuseMoveUnavailable(w, r, run)
	case errors.As(err, &ref):
		writeErrorReasonField(w, ref.status, ref.reason, ref.msg, ref.layer)
	case err != nil:
		writeErrorReason(w, http.StatusServiceUnavailable, reasonMovePolicyUnreadable, "The move could not be judged; nothing was stopped.")
	default:
		writeJSON(w, http.StatusAccepted, mv)
	}
}

// runPlacementsAnswer is GET /runs/{id}/placements: every move of the run, refused and failed ones with their
// reason, and the run's placement history.
type runPlacementsAnswer struct {
	Moves      []types.RunMove      `json:"moves"`
	Placements []types.RunPlacement `json:"placements"`
}

// handleRunPlacements serves GET /runs/{id}/placements: a run's moves and placement history, for the owner or a
// super admin. It answers move_unavailable while moving a run is off.
func (s *Server) handleRunPlacements(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	if _, ok := s.getRunAuthorizedBy(w, r, id, s.ownsRunOrSuperAdmin); !ok {
		return
	}
	ms, isMoveStore := s.cfg.Store.(store.RunMoveStore)
	if !s.moveOn(r.Context()) || !moveGateOpen() || !isMoveStore {
		writeErrorReason(w, http.StatusConflict, reasonMoveUnavailable, moveUnavailableMessage)
		return
	}
	moves, err := ms.ListRunMoves(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "The run's moves could not be read.")
		return
	}
	placements, err := ms.ListRunPlacements(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "The run's placements could not be read.")
		return
	}
	writeJSON(w, http.StatusOK, runPlacementsAnswer{Moves: moves, Placements: placements})
}

// moveOn reads site config for moveAvailable; a site config that cannot be read reads off.
func (s *Server) moveOn(ctx context.Context) bool {
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	return err == nil && moveAvailable(sc)
}

// moveRequest is one asked-for move: the run as the request read it, the destination, and who asked.
type moveRequest struct {
	Run       types.AgentRun
	To        types.MoveDestination
	ActorType types.ActorType
	Principal string
}

// startMove is the move's only entry: handleMoveRun and the move sweep call it (TestMoveGateCallers). It answers
// errMoveUnavailable while the gate is closed. Otherwise it admits the move (step 1), judges the destination
// (step 2), and either runs the locked sequence in the request (a run already in a stopped pause) or hands a
// running run to the executor; the answer is the move's row, a *moveRefusal, or a read the judgement needed that
// failed. Every refusal writes one run.move denied row, and a refusal after the row is admitted ends it refused.
func (s *Server) startMove(ctx context.Context, req moveRequest) (types.RunMove, error) {
	if !moveGateOpen() {
		return types.RunMove{}, errMoveUnavailable
	}
	ms, ok := s.cfg.Store.(store.RunMoveStore)
	if !ok {
		return types.RunMove{}, errMoveUnavailable
	}
	x, ref, err := s.admitMove(ctx, ms, req)
	if err != nil {
		ref = refuseMove(http.StatusServiceUnavailable, reasonMovePolicyUnreadable, "A read the move needs failed; nothing was stopped.")
	}
	if ref != nil {
		if x == nil {
			s.recordAudit(ctx, s.auditEvent(&req.Run.ID, req.ActorType, req.Principal, "run.move", req.Run.ID.String(), "denied",
				mustJSON(map[string]any{"reason": ref.reason, "layer": ref.layer})))
		} else {
			x.end(ctx, refuseMoveEnd(ref.reason, ref.msg))
		}
		return types.RunMove{}, ref
	}
	if x.stopped {
		if end := x.stopLocked(ctx); end != nil {
			x.end(ctx, end)
			status := moveReasonStatus(end.reason)
			if end.state == types.MoveFailed {
				status = http.StatusServiceUnavailable
			}
			if end == moveLapsed {
				end = failMove(reasonMoveOrphaned, "The move's hold lapsed; the move sweep ends it.")
			}
			return types.RunMove{}, refuseMove(status, end.reason, end.msg)
		}
	}
	mv := x.mv
	s.goBackground(func() { x.execute(s.cfg.BaseCtx) })
	return mv, nil
}

// admitMove is step 1 in its order, the admitted row, and step 2. It answers the executor for an accepted move, a
// refusal (with the executor when the row was admitted, so the caller ends it), or a read that failed.
func (s *Server) admitMove(ctx context.Context, ms store.RunMoveStore, req moveRequest) (*moveExec, *moveRefusal, error) {
	run := req.Run
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return nil, nil, err
	}
	if ref, err := s.moveRunRefusal(ctx, run); ref != nil || err != nil {
		return nil, ref, err
	}
	rec, err := s.loadRunSandboxSpec(ctx, run.ID)
	switch {
	case errors.Is(err, store.ErrNotFound) || errors.Is(err, errRunSandboxSpecNotKept):
		return nil, refuseMove(http.StatusConflict, reasonMoveSpecUnrecorded, "The run's sandbox was not recorded when it started, so it cannot be moved."), nil
	case err != nil:
		return nil, nil, err
	}
	if reason, layer := rec.moveRefusal(); reason != "" {
		return nil, refuseMoveAt(reason, layer, "The run holds a value in its sandbox that a move cannot carry ("+layer+")."), nil
	}
	if why, err := movePortStopRefusal.call(ctx, run); err != nil || why != "" {
		if err != nil {
			why = "This server cannot record a person's pause."
		}
		return nil, refuseMove(http.StatusConflict, reasonMovePauseRefused, why), nil
	}
	if contained, err := movePortLocallyContained.call(ctx, run.ID); err != nil {
		return nil, refuseMove(http.StatusServiceUnavailable, reasonMoveContainmentUnreadable, "Whether the run is held contained could not be read, so it was not moved."), nil
	} else if contained {
		return nil, refuseMove(http.StatusConflict, reasonMoveLocallyContained, "The run is held contained, so it stays where it is."), nil
	}
	if ref, err := moveHeldRefusal(ctx, ms, run.ID); ref != nil || err != nil {
		return nil, ref, err
	}
	dest, ref, err := s.resolveMoveDestination(ctx, run, req.To, sc)
	if ref != nil || err != nil {
		return nil, ref, err
	}
	if moveSamePlace(run, dest) {
		return nil, refuseMoveAt(reasonMoveDestinationRefused, "same_place", "The run already runs there."), nil
	}
	if (run.RunnerID != nil || dest.local()) && hoptls.CheckURL(s.cfg.RunnerOrgURL) != nil {
		return nil, refuseMove(http.StatusConflict, reasonMovePlainLink, "A runner's link to this server is not TLS, so the run's files cannot be carried over it."), nil
	}
	caps, ref, err := s.moveCapsRefusal(ctx, run, rec, dest)
	if ref != nil || err != nil {
		return nil, ref, err
	}
	x, ref, err := s.admitMoveRow(ctx, ms, req, rec, dest, sc)
	if x == nil || ref != nil || err != nil {
		return x, ref, err
	}
	x.caps = caps
	plan, ref, err := s.judgeMoveDestination(ctx, run, rec, dest, caps, sc)
	if err != nil {
		return x, refuseMove(http.StatusServiceUnavailable, reasonMovePolicyUnreadable, "A read the destination's judgement needs failed; nothing was stopped."), nil
	}
	if ref != nil {
		return x, ref, nil
	}
	x.plan = plan
	x.mv.ToTrustClass, x.mv.Narrowed = plan.Trust, mustJSON(plan.Narrowed)
	if ok, err := ms.SetRunMoveDestination(ctx, x.mv); err != nil || !ok {
		return x, refuseMove(http.StatusServiceUnavailable, reasonMovePolicyUnreadable, "The destination's judgement could not be recorded; nothing was stopped."), nil
	}
	return x, nil, nil
}

// moveSamePlace reports whether d is where the run runs now: the same runner for a run on a runner, else the same
// executor of the organisation's (the default one when neither names an executor).
func moveSamePlace(run types.AgentRun, d moveDest) bool {
	if run.RunnerID != nil || d.local() {
		return run.RunnerID != nil && d.RunnerID != nil && *run.RunnerID == *d.RunnerID
	}
	return run.RunnerPoolExecutor == d.Executor
}

// moveHeldRefusal refuses a run a move holds, and a run an earlier move of which still has a sandbox to remove: the
// sweep's removal of that sandbox must never meet one this move makes.
func moveHeldRefusal(ctx context.Context, ms store.RunMoveStore, runID uuid.UUID) (*moveRefusal, error) {
	if held, err := ms.RunMoveInFlight(ctx, runID); err != nil {
		return nil, err
	} else if held {
		return refuseMove(http.StatusConflict, reasonMoveBusy, "A move already holds this run."), nil
	}
	moves, err := ms.ListRunMoves(ctx, runID)
	if err != nil {
		return nil, err
	}
	for _, m := range moves {
		if m.Cleanup == types.MoveCleanupPending {
			return refuseMove(http.StatusConflict, reasonMoveBusy, "An earlier move of this run is still removing a sandbox it left; try again shortly."), nil
		}
	}
	return nil, nil
}

// moveRunRefusal is step 1's terms on the run itself: RUNNING and not claimed by a Resume, interactive, keeping no
// machine, not a provider sign-in, and a container.
func (s *Server) moveRunRefusal(ctx context.Context, run types.AgentRun) (*moveRefusal, error) {
	switch {
	case run.State != types.RunRunning || run.PausedReason == movePauseResuming:
		return refuseMove(http.StatusConflict, reasonMoveNotRunning, "Only a running run can be moved."), nil
	case !run.Interactive:
		return refuseMove(http.StatusConflict, reasonMoveNotInteractive, "Only an interactive run can be moved."), nil
	case run.Task == harnessLoginTask:
		return refuseMove(http.StatusConflict, reasonMoveHarnessLogin, "A provider sign-in run is never moved."), nil
	case run.MachineKind != "" && run.MachineKind != types.MachineContainer:
		return refuseMove(http.StatusConflict, reasonMoveMachineKindUnsupported, "Only a container run can be moved; a VM run cannot."), nil
	}
	ks, ok := s.cfg.Store.(interface {
		GetKeptMachineByRun(context.Context, uuid.UUID) (types.KeptMachine, error)
	})
	if !ok {
		return nil, nil
	}
	_, err := ks.GetKeptMachineByRun(ctx, run.ID)
	switch {
	case err == nil:
		return refuseMove(http.StatusConflict, reasonMoveKeptMachine, "The run keeps its machine, so it stays where it is."), nil
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	}
	return nil, err
}

// admitMoveRow writes the admitted row under the install's and a personal destination's caps. No token stamp is
// written and nothing is stopped.
func (s *Server) admitMoveRow(ctx context.Context, ms store.RunMoveStore, req moveRequest, rec runSandboxSpecRecord, dest moveDest,
	sc types.SiteConfig,
) (*moveExec, *moveRefusal, error) {
	run := req.Run
	gen, err := moveGen(ctx, run)
	if err != nil {
		return nil, nil, err
	}
	var from types.RunnerTrustClass
	if ts, ok := s.cfg.Store.(store.RunTrustStore); ok {
		if from, err = ts.RunRunnerTrustClass(ctx, run.ID); err != nil {
			return nil, nil, err
		}
	}
	limits := store.RunMoveLimits{Install: moveInstallCap}
	if dest.personal() {
		limits.PerRunner = movePerRunnerCap
	}
	toEvidence := types.RunEvidenceSubstrate
	if dest.local() {
		toEvidence = types.RunEvidenceRunnerAsserted
	}
	mv, err := ms.AdmitRunMove(ctx, types.RunMove{RunID: run.ID, PauseGen: gen,
		FromPlacement: run.Placement, FromRunnerID: run.RunnerID, FromPoolID: run.RunnerPoolID, FromExecutor: run.RunnerPoolExecutor,
		FromSandboxRef: run.SandboxRef, FromEvidenceSource: run.EvidenceSource, FromTrustClass: from,
		ToPlacement: dest.Placement, ToRunnerID: dest.RunnerID, ToPoolID: dest.PoolID, ToExecutor: dest.Executor,
		ToEvidenceSource: toEvidence, RecordingLevel: run.RecordingLevel, MovedBy: req.Principal}, limits)
	if errors.Is(err, store.ErrRunMoveBusy) {
		return nil, refuseMove(http.StatusConflict, reasonMoveBusy, "A move already holds this run, or as many moves as this server runs at once are in flight."), nil
	}
	if err != nil {
		return nil, nil, err
	}
	return &moveExec{s: s, ms: ms, mv: mv, run: run, rec: rec, dest: dest, sc: sc, actorType: req.ActorType, principal: req.Principal,
		stopped: run.PausedAt != nil && run.PausedReason == movePauseStopped, maxBytes: moveMaxCarriedBytes(sc)}, nil, nil
}

// The move caps: moves in flight on the install, and onto one person's runner.
const (
	moveInstallCap   = 4
	movePerRunnerCap = 1
)

const moveUnavailableMessage = "Moving a run is not available on this server: an admin turns it on with features.run_move.enabled in site config, once a release that can move runs is installed."

// refuseMoveUnavailable answers 409 move_unavailable and writes the refused move's run.move denied row.
func (s *Server) refuseMoveUnavailable(w http.ResponseWriter, r *http.Request, run types.AgentRun) {
	actorType, principal := actorFromRequest(r)
	s.recordAudit(r.Context(), s.auditEvent(&run.ID, actorType, principal, "run.move", run.ID.String(), "denied",
		mustJSON(map[string]any{"reason": reasonMoveUnavailable})))
	writeErrorReason(w, http.StatusConflict, reasonMoveUnavailable, moveUnavailableMessage)
}
