// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/authz"
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

// auditDesktopSignIn is the Audit tab's "paused the agent to sign in inside the desktop".
const auditDesktopSignIn = "desktop.sign_in.pause"

const (
	desktopSignInsOffMsg         = "Your admin doesn't allow sign-ins in this desktop while this agent runs"
	desktopSignInsUnavailableMsg = "This run's desktop has no sign-in store: its runner or its guest image is older than sign-ins"
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
// person who may enter the run's desktop (mayEnterRun), while it runs.
func (s *Server) desktopSignInsRun(w http.ResponseWriter, r *http.Request, enter bool) (types.AgentRun, store.DesktopVMSignInStore, store.DesktopVMSignIns, bool) {
	var none store.DesktopVMSignIns
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return types.AgentRun{}, nil, none, false
	}
	run, ok := s.getRunAuthorizedBy(w, r, id, s.ownsRunOrAdmin)
	if !ok {
		return run, nil, none, false
	}
	if enter && !s.mayPauseDesktopAgent(w, r, run) {
		return run, nil, none, false
	}
	st, ok := s.cfg.Store.(store.DesktopVMSignInStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonDesktopSignInsUnavailable, desktopSignInsUnavailableMsg)
		return run, nil, none, false
	}
	row, found, err := st.DesktopVMSignIns(r.Context(), run.ID)
	if err != nil {
		slog.WarnContext(r.Context(), "wardynd: desktop sign-ins unreadable", "run_id", run.ID, "err", err)
		writeErrorReason(w, http.StatusServiceUnavailable, reasonDesktopSignInsUnavailable, "the desktop's sign-ins could not be read; try again")
		return run, nil, none, false
	}
	if enter && !found {
		writeErrorReason(w, http.StatusConflict, reasonDesktopSignInsUnavailable, desktopSignInsUnavailableMsg)
		return run, nil, none, false
	}
	return run, st, row, true
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
	run, _, row, ok := s.desktopSignInsRun(w, r, false)
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
	run, st, row, ok := s.desktopSignInsRun(w, r, true)
	if !ok {
		return
	}
	ctx := r.Context()
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
	now := time.Now().UTC()
	marked, err := st.PauseDesktopVMAgent(ctx, run.ID, principal, now)
	if err != nil || !marked {
		writeErrorReason(w, http.StatusConflict, reasonDesktopSignInsPauseFailed, "the agent's pause could not be recorded; try again")
		return
	}
	if err := p.PauseAgent(ctx, run.SandboxRef); err != nil {
		// Never leave the agent stopped with no record of it: continue it, then clear the mark.
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = p.ResumeAgent(bg, run.SandboxRef)
		_, _, _, _ = st.ResumeDesktopVMAgent(bg, run.ID)
		s.recordAudit(ctx, s.auditEvent(&run.ID, at, principal, auditDesktopSignIn, run.ID.String(), "failure",
			mustJSON(map[string]any{"stage": "pause", "error": err.Error()})))
		status := http.StatusBadGateway
		if errors.Is(err, runner.ErrAgentPauseUnsupported) {
			status = http.StatusConflict
		}
		writeErrorReason(w, status, reasonDesktopSignInsPauseFailed, "the agent could not be paused; it keeps running")
		return
	}
	v.Paused, v.PausedAt = true, &now
	writeJSON(w, http.StatusOK, v)
}

// handleResumeDesktopAgent continues the agent and writes the session's one desktop.sign_in.pause row. It is
// never refused for policy: an agent is not left stopped because the setting changed.
func (s *Server) handleResumeDesktopAgent(w http.ResponseWriter, r *http.Request) {
	run, st, row, ok := s.desktopSignInsRun(w, r, true)
	if !ok {
		return
	}
	ctx := r.Context()
	if row.PausedAt == nil {
		writeJSON(w, http.StatusOK, s.desktopSignInsView(ctx, run, row))
		return
	}
	p, ok := s.agentPauser(w)
	if !ok {
		return
	}
	at, principal := actorFromRequest(r)
	if err := p.ResumeAgent(ctx, run.SandboxRef); err != nil {
		s.recordAudit(ctx, s.auditEvent(&run.ID, at, principal, auditDesktopSignIn, run.ID.String(), "failure",
			mustJSON(map[string]any{"stage": "resume", "paused_by": row.PausedBy, "paused_at": row.PausedAt, "error": err.Error()})))
		writeErrorReason(w, http.StatusBadGateway, reasonDesktopSignInsResumeFailed, "the agent could not be resumed; it is still paused. Try again")
		return
	}
	now := time.Now().UTC()
	by, pausedAt, cleared, err := st.ResumeDesktopVMAgent(ctx, run.ID)
	if err != nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonDesktopSignInsResumeFailed, "the agent was resumed, but that could not be recorded; try again")
		return
	}
	if cleared {
		s.recordAudit(ctx, s.auditEvent(&run.ID, at, principal, auditDesktopSignIn, run.ID.String(), "success",
			mustJSON(map[string]any{"paused_by": by, "paused_at": pausedAt, "resumed_at": now, "paused_seconds": int(now.Sub(pausedAt).Seconds())})))
	}
	row.PausedBy, row.PausedAt = "", nil
	writeJSON(w, http.StatusOK, s.desktopSignInsView(ctx, run, row))
}
