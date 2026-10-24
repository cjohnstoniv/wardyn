// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
	"github.com/go-chi/chi/v5"
)

// Sign-ins in a Linux VM's desktop (#2972). The guest keeps its keychain and browser profiles on a
// tmpfs (guestagent.MountSignInStore) in a guest with no swap, so a sign-in made in the desktop lives
// in the machine's memory and is gone when it shuts down. It does enter the sandbox: once the agent
// runs again it can use every session the person opened and read any file they saved. The admin sets
// "Sign-ins in a desktop" per agent (types.AgentProvider.DesktopSignInsOff, Allowed by default); "Not
// allowed" makes the store read-only, so that desktop keeps none, and refuses the pause.
//
//	GET  /runs/{id}/desktop/sign-ins          the run's store and whether its agent is paused
//	POST /runs/{id}/desktop/sign-ins/pause    stop the agent inside the guest (the desktop stays live)
//	POST /runs/{id}/desktop/sign-ins/resume   continue it; each pause-to-resume is one desktop.sign_in.pause row
//
// The pause is voluntary: a sign-in made without it is neither paused nor audited.
//
// A pause and a resume each hold the run's operation lock (lockRunOp, a Postgres advisory lock on a
// connection of its own, so it serializes replicas) from their read of the row through the runner's
// answer and the row's write. The pause marks the row only once the runner has stopped the agent, so
// the GET and a second pause say paused only then, and a resume from another tab waits for an
// in-flight pause and then continues the agent that pause stopped.

// auditDesktopSignIn is the Audit tab's "paused the agent to sign in inside the desktop".
const auditDesktopSignIn = "desktop.sign_in.pause"

const (
	desktopSignInsOffMsg         = "Your admin doesn't allow sign-ins in this desktop while this agent runs"
	desktopSignInsUnavailableMsg = "This run's desktop has no sign-in store: its runner or its guest image is older than sign-ins"
	desktopResumeUnrecordedMsg   = "the agent was resumed, but that could not be recorded; try again"
)

// desktopSignInsView is GET /runs/{id}/desktop/sign-ins and the pause and resume answers. Store is
// "memory", "none", or "" for a run with no store (a container, or a VM that predates it).
type desktopSignInsView struct {
	Store    string     `json:"store"`
	Paused   bool       `json:"paused"`
	PausedAt *time.Time `json:"paused_at,omitempty"`
	// CanPause is false when the store is not "memory" or the admin has since set Not allowed.
	CanPause bool `json:"can_pause"`
	// Agent is the run's agent by its display name, for "while {agent} runs".
	Agent string `json:"agent"`
}

// desktopSignInsOff is the run's spec value: true when the agent's row says Not allowed, and when the
// roster could not be read (a desktop then keeps none rather than one nobody decided).
func desktopSignInsOff(sc types.SiteConfig, scErr error, agent string) bool {
	if scErr != nil {
		return true
	}
	row, ok := agentProviderFor(sc, agent)
	return ok && row.DesktopSignInsOff
}

// recordDesktopVMSignIns keeps what a VM's guest made of its sign-in store. A guest that reported
// none gets no row, so the console claims nothing for it; a failed write is logged and the run goes
// on with no row, the same.
func (s *Server) recordDesktopVMSignIns(ctx context.Context, run types.AgentRun, sb runner.Sandbox) {
	if !run.MachineKind.IsVM() || (sb.SignIns != guestagent.SignInsMemory && sb.SignIns != guestagent.SignInsNone) {
		return
	}
	st, ok := s.cfg.Store.(store.DesktopVMSignInStore)
	if !ok {
		return
	}
	if err := st.RecordDesktopVMSignIns(ctx, run.ID, sb.SignIns); err != nil {
		slog.WarnContext(ctx, "wardynd: the VM's sign-in store was not recorded; its run page says nothing about sign-ins", "run_id", run.ID, "err", err)
	}
}

func agentDisplay(agent string) string {
	if d, ok := harnessByID(agent); ok {
		return d.Display
	}
	return agent
}

func (s *Server) desktopSignInsView(ctx context.Context, run types.AgentRun, row store.DesktopVMSignIns) desktopSignInsView {
	v := desktopSignInsView{Store: row.Store, Paused: row.PausedAt != nil, PausedAt: row.PausedAt, Agent: agentDisplay(run.Agent)}
	if row.Store == guestagent.SignInsMemory {
		sc, err := s.cfg.Store.GetSiteConfig(ctx)
		v.CanPause = !desktopSignInsOff(sc, err, run.Agent)
	}
	return v
}

// desktopSignInsRun loads the run and its row for the three routes. A pause or resume is for the
// person who may enter the run's desktop (mayEnterRun), while it runs, and reads the row under the
// run's operation lock: r is rebound to the lock's context, and unlock (a no-op for the GET) is the
// caller's to defer.
func (s *Server) desktopSignInsRun(w http.ResponseWriter, r *http.Request, enter bool) (*http.Request, types.AgentRun, store.DesktopVMSignInStore, store.DesktopVMSignIns, func(), bool) {
	var none store.DesktopVMSignIns
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return r, types.AgentRun{}, nil, none, nil, false
	}
	run, ok := s.getRunAuthorizedBy(w, r, id, s.ownsRunOrAdmin)
	if !ok {
		return r, run, nil, none, nil, false
	}
	if enter && !s.mayPauseDesktopAgent(w, r, run) {
		return r, run, nil, none, nil, false
	}
	st, ok := s.cfg.Store.(store.DesktopVMSignInStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonDesktopSignInsUnavailable, desktopSignInsUnavailableMsg)
		return r, run, nil, none, nil, false
	}
	unlock := func() {}
	if enter {
		if r, unlock, ok = s.lockDoor(w, r, db.RunOpLockClass, run.ID.String()); !ok {
			return r, run, nil, none, nil, false
		}
	}
	row, found, err := st.DesktopVMSignIns(r.Context(), run.ID)
	if err != nil {
		unlock()
		slog.WarnContext(r.Context(), "wardynd: desktop sign-ins unreadable", "run_id", run.ID, "err", err)
		writeErrorReason(w, http.StatusServiceUnavailable, reasonDesktopSignInsUnavailable, "the desktop's sign-ins could not be read; try again")
		return r, run, nil, none, nil, false
	}
	if enter && !found {
		unlock()
		writeErrorReason(w, http.StatusConflict, reasonDesktopSignInsUnavailable, desktopSignInsUnavailableMsg)
		return r, run, nil, none, nil, false
	}
	return r, run, st, row, unlock, true
}

// mayPauseDesktopAgent refuses, before any store read, a pause or resume by someone who may not enter
// the run's desktop or of a run that is not running.
func (s *Server) mayPauseDesktopAgent(w http.ResponseWriter, r *http.Request, run types.AgentRun) bool {
	if s.refuseBackgroundOnly(w, r, run, "desktop") {
		return false
	}
	if superAdmin := s.isOperator(r.Context()); !mayEnterRun(run, principalFromRequest(r), superAdmin) {
		if superAdmin {
			s.refuseRunOwnerOnly(w, r, run)
		} else {
			s.refuse(w, r, authz.Deny(authz.ReasonAttachTicketForeignRun, run.ID.String(), "run not found").OnRun(run.ID).AsIf(authz.Reason(reasonRunNotFound)))
		}
		return false
	}
	if run.State != types.RunRunning || run.SandboxRef == "" {
		writeErrorReason(w, http.StatusConflict, reasonAttachNotRunning, "run is not RUNNING (state="+string(run.State)+")")
		return false
	}
	return true
}

// mountDesktopSignInRoutes: the read is the owner's or an admin's; pause and resume are the person's
// who may enter the run (mayPauseDesktopAgent).
func (s *Server) mountDesktopSignInRoutes(r chi.Router) {
	r.Get("/runs/{id}/desktop/sign-ins", s.handleGetDesktopSignIns)
	r.Post("/runs/{id}/desktop/sign-ins/pause", s.handlePauseDesktopAgent)
	r.Post("/runs/{id}/desktop/sign-ins/resume", s.handleResumeDesktopAgent)
}

func (s *Server) handleGetDesktopSignIns(w http.ResponseWriter, r *http.Request) {
	r, run, _, row, _, ok := s.desktopSignInsRun(w, r, false)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.desktopSignInsView(r.Context(), run, row))
}

func (s *Server) agentPauser(w http.ResponseWriter) (runner.AgentPauser, bool) {
	p, ok := s.cfg.Runner.(runner.AgentPauser)
	if !ok {
		writeErrorReason(w, http.StatusConflict, reasonDesktopSignInsUnavailable, "this deployment's runner cannot pause an agent")
	}
	return p, ok
}

// handlePauseDesktopAgent stops the agent inside the guest. Refused server-side when the run's
// store keeps none or the admin has since set Not allowed: the console's missing button is not the
// control.
func (s *Server) handlePauseDesktopAgent(w http.ResponseWriter, r *http.Request) {
	base := r.Context() // without the run's lock, so a pause that loses it can take it again
	r, run, st, row, unlock, ok := s.desktopSignInsRun(w, r, true)
	if !ok {
		return
	}
	unlock = sync.OnceFunc(unlock) // released once the last write it guards is done
	defer unlock()
	ctx, cancelCall := context.WithTimeout(r.Context(), desktopAgentCallWait)
	defer cancelCall()
	v := s.desktopSignInsView(ctx, run, row)
	if !v.CanPause {
		writeErrorReason(w, http.StatusForbidden, reasonDesktopSignInsOff, desktopSignInsOffMsg)
		return
	}
	if v.Paused {
		writeJSON(w, http.StatusOK, v)
		return
	}
	if s.refuseUIAppsDenied(w, r, run) || s.refuseInteractiveAttach(w, r, run) {
		return
	}
	p, ok := s.agentPauser(w)
	if !ok {
		return
	}
	at, principal := actorFromRequest(r)
	// The row is marked only once the runner has stopped the agent, so nothing says paused while the
	// agent may still be running. From the runner's answer on, the work is finished on a context the
	// person's closed tab cannot cancel.
	err := p.PauseAgent(ctx, run.SandboxRef)
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), desktopAgentCallWait)
	defer cancel()
	now := time.Now().UTC()
	stage := "pause"
	if err == nil {
		stage = "record"
		// Marked only under the run's lock: with it lost, another pause may have stopped and marked the
		// agent since.
		if err = s.lockHeld(bg); err == nil {
			var marked bool
			if marked, err = st.PauseDesktopVMAgent(bg, run.ID, principal, now); err == nil && !marked {
				err = errDesktopSignInUnmarked
			}
		}
	}
	if err != nil {
		// Never leave the agent stopped with no mark, which no resume would continue: a stop can land
		// even when the runner answers an error, so continue it, under the run's lock. One lost since
		// the call is taken again and the row read again: a pause that has marked it since owns the
		// stopped agent, which its resume continues.
		send, relocked := bg, func() {}
		if s.lockHeld(bg) != nil {
			unlock()
			var lctx context.Context
			var cur store.DesktopVMSignIns
			if lctx, cur, relocked, ok = s.desktopRelock(base, run, st); !ok {
				s.recordAudit(bg, s.auditEvent(&run.ID, at, principal, auditDesktopSignIn, run.ID.String(), "failure",
					mustJSON(map[string]any{"stage": "lock_lost", "error": err.Error()})))
				w.Header().Set("Retry-After", "5")
				writeErrorReason(w, http.StatusServiceUnavailable, reasonLockUnavailable, lockUnavailableMsg)
				return
			}
			defer relocked()
			if cur.PausedAt != nil {
				newer := s.desktopSignInsView(lctx, run, cur)
				relocked()
				writeJSON(w, http.StatusOK, newer)
				return
			}
			send = lctx
		}
		_ = p.ResumeAgent(send, run.SandboxRef)
		relocked()
		unlock()
		s.recordAudit(bg, s.auditEvent(&run.ID, at, principal, auditDesktopSignIn, run.ID.String(), "failure",
			mustJSON(map[string]any{"stage": stage, "error": err.Error()})))
		if stage == "record" {
			writeErrorReason(w, http.StatusConflict, reasonDesktopSignInsPauseFailed, "the agent's pause could not be recorded; try again")
			return
		}
		status := http.StatusBadGateway
		if errors.Is(err, runner.ErrAgentPauseUnsupported) {
			status = http.StatusConflict
		}
		writeErrorReason(w, status, reasonDesktopSignInsPauseFailed, "the agent could not be paused; it keeps running")
		return
	}
	// The desktop's screen recording keeps no frames from here until the resume ends this hold, as a
	// container's sign-in does. A run that ends while paused drops the hold with its last connection
	// (forgetEndedDesktopPause).
	if s.desktopRecorded(run) {
		s.PauseDesktopRecording(bg, run, desktopPauseSignin, vmSignInHold(run))
	}
	unlock()
	v.Paused, v.PausedAt = true, &now
	writeJSON(w, http.StatusOK, v)
}

// desktopAgentCallWait bounds a pause's or resume's runner call, and again the writes after its answer,
// so a hung runner holds the run's lock for about a minute at most.
const desktopAgentCallWait = 30 * time.Second

// desktopRelock takes the run's lock again for a pause or resume that lost it after the runner's
// answer, and has released the lost hold (one only reported lost, its connection busy, would
// otherwise hold this take off), waiting at most desktopAgentCallWait, and reads the row under it.
// ok is false when either fails or the new lock is lost too: nothing more is sent or written.
// relocked releases it.
func (s *Server) desktopRelock(base context.Context, run types.AgentRun, st store.DesktopVMSignInStore) (ctx context.Context, row store.DesktopVMSignIns, relocked func(), ok bool) {
	slog.WarnContext(base, "wardynd: a desktop sign-in pause or resume lost its run lock; reconciling under a new one", "run_id", run.ID)
	lctx, unlock, ok := s.lockRunOpWithin(context.WithoutCancel(base), run.ID, desktopAgentCallWait)
	if !ok {
		return nil, row, nil, false
	}
	ctx, cancel := context.WithTimeout(lctx, desktopAgentCallWait)
	relocked = sync.OnceFunc(func() { cancel(); unlock() })
	row, _, err := st.DesktopVMSignIns(ctx, run.ID)
	if err == nil {
		err = s.lockHeld(ctx)
	}
	if err != nil {
		relocked()
		return nil, row, nil, false
	}
	return ctx, row, relocked, true
}

// errDesktopSignInUnmarked is an acked pause whose row was no longer a 'memory' row with no mark.
var errDesktopSignInUnmarked = errors.New("the run's sign-in row was not marked paused")

// vmSignInHold is a VM sign-in pause's hold on the run's desktop recording: a run has at most one.
func vmSignInHold(run types.AgentRun) string {
	return "vm-sign-in:" + run.ID.String()
}

// holdOpenVMSignIn takes vmSignInHold for a recording that starts while the run's row says its agent is
// paused. The hold the pause took lives in one process's memory, so after a restart, or on another
// replica, this read of the row is what keeps a new desktop connection from recording the sign-in;
// the resume ends it as it ends the pause's own. The second read undoes a hold taken just after a
// resume cleared the mark, which that resume's endDesktopHold ran too early to end.
func (s *Server) holdOpenVMSignIn(run types.AgentRun) {
	hold := vmSignInHold(run)
	st, ok := s.cfg.Store.(store.DesktopVMSignInStore)
	if !ok || !run.MachineKind.IsVM() || s.desktopRecs.holds(run.ID, hold) {
		return
	}
	ctx := s.cfg.BaseCtx
	row, found, err := st.DesktopVMSignIns(ctx, run.ID)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: a VM desktop's recording could not read whether its agent is paused to sign in; it records", "run_id", run.ID, "err", err)
		return
	}
	if !found || row.PausedAt == nil {
		return
	}
	s.PauseDesktopRecording(ctx, run, desktopPauseSignin, hold)
	if row, found, err = st.DesktopVMSignIns(ctx, run.ID); err == nil && (!found || row.PausedAt == nil) {
		s.ResumeDesktopRecording(ctx, run, hold)
	}
}

// handleResumeDesktopAgent continues the agent and writes the session's one desktop.sign_in.pause row. It is
// never refused for policy: an agent is not left stopped because the setting changed. A pause in flight
// holds the run's lock, so a resume waits for its answer rather than overtaking it.
func (s *Server) handleResumeDesktopAgent(w http.ResponseWriter, r *http.Request) {
	base := r.Context() // without the run's lock, so a resume that loses it can take it again
	r, run, st, row, unlock, ok := s.desktopSignInsRun(w, r, true)
	if !ok {
		return
	}
	unlock = sync.OnceFunc(unlock) // released once the last write it guards is done
	defer unlock()
	ctx, cancelCall := context.WithTimeout(r.Context(), desktopAgentCallWait)
	defer cancelCall()
	if row.PausedAt == nil {
		writeJSON(w, http.StatusOK, s.desktopSignInsView(ctx, run, row))
		return
	}
	p, ok := s.agentPauser(w)
	if !ok {
		return
	}
	at, principal := actorFromRequest(r)
	err := p.ResumeAgent(ctx, run.SandboxRef)
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), desktopAgentCallWait)
	defer cancel()
	if err != nil {
		unlock()
		s.recordAudit(bg, s.auditEvent(&run.ID, at, principal, auditDesktopSignIn, run.ID.String(), "failure",
			mustJSON(map[string]any{"stage": "resume", "paused_by": row.PausedBy, "paused_at": row.PausedAt, "error": err.Error()})))
		writeErrorReason(w, http.StatusBadGateway, reasonDesktopSignInsResumeFailed, "the agent could not be resumed; it is still paused. Try again")
		return
	}
	// The agent runs again: clear its mark even if the person's tab has closed, or the run would say
	// paused while its agent runs. Cleared only under the run's lock: one lost since the call is taken
	// again and the row read again, and a mark that is no longer the one this resume read is another
	// pause's, left to its own resume.
	write, relocked := bg, func() {}
	if s.lockHeld(bg) != nil {
		unlock()
		var lctx context.Context
		var cur store.DesktopVMSignIns
		if lctx, cur, relocked, ok = s.desktopRelock(base, run, st); !ok {
			s.recordAudit(bg, s.auditEvent(&run.ID, at, principal, auditDesktopSignIn, run.ID.String(), "failure",
				mustJSON(map[string]any{"stage": "lock_lost", "paused_by": row.PausedBy, "paused_at": row.PausedAt})))
			writeErrorReason(w, http.StatusServiceUnavailable, reasonDesktopSignInsResumeFailed, desktopResumeUnrecordedMsg)
			return
		}
		defer relocked()
		if cur.PausedAt == nil || !cur.PausedAt.Equal(*row.PausedAt) {
			current := s.desktopSignInsView(lctx, run, cur)
			relocked()
			writeJSON(w, http.StatusOK, current)
			return
		}
		write = lctx
	}
	now := time.Now().UTC()
	by, pausedAt, cleared, err := st.ResumeDesktopVMAgent(write, run.ID)
	if err != nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonDesktopSignInsResumeFailed, desktopResumeUnrecordedMsg)
		return
	}
	if cleared {
		s.endDesktopHold(bg, run, vmSignInHold(run))
	}
	relocked()
	unlock()
	if cleared {
		s.recordAudit(bg, s.auditEvent(&run.ID, at, principal, auditDesktopSignIn, run.ID.String(), "success",
			mustJSON(map[string]any{"paused_by": by, "paused_at": pausedAt, "resumed_at": now, "paused_seconds": int(now.Sub(pausedAt).Seconds())})))
	}
	row.PausedBy, row.PausedAt = "", nil
	writeJSON(w, http.StatusOK, s.desktopSignInsView(bg, run, row))
}
