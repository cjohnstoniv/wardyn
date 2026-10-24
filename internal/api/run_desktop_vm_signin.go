// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
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
//
// Each pause takes the run's next pause generation before it asks the runner (migration 0231), and
// every stop and continue carries a generation to the guest agent, which drops one older than the
// newest it has seen (guestagent.PauseFence). A stop or continue that reaches the guest late, after
// the lock was lost, so cannot undo a newer pause. A runner or guest image that predates it signals each
// request as it arrives, as before: the session's audit row then says fenced false.

// auditDesktopSignIn is the Audit tab's "paused the agent to sign in inside the desktop".
const auditDesktopSignIn = "desktop.sign_in.pause"

const (
	desktopSignInsOffMsg         = "Your admin doesn't allow sign-ins in this desktop while this agent runs"
	desktopSignInsUnavailableMsg = "This run's desktop has no sign-in store: its runner or its guest image is older than sign-ins"
	desktopResumeUnrecordedMsg   = "The agent was resumed, but that could not be recorded; try again"
	desktopSignInsUnreadableMsg  = "The desktop's sign-ins could not be read; try again"
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
		writeErrorReason(w, http.StatusServiceUnavailable, reasonDesktopSignInsUnavailable, desktopSignInsUnreadableMsg)
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
	gen, err := st.NextDesktopVMPauseGeneration(ctx, run.ID)
	if err != nil { // nothing was sent: the agent runs on
		s.recordAudit(context.WithoutCancel(ctx), s.auditEvent(&run.ID, at, principal, auditDesktopSignIn, run.ID.String(), "failure",
			mustJSON(map[string]any{"stage": "record", "error": err.Error()})))
		writeErrorReason(w, http.StatusConflict, reasonDesktopSignInsPauseFailed, "The agent's pause could not be recorded; try again")
		return
	}
	// The row is marked only once the runner has stopped the agent, so nothing says paused while the
	// agent may still be running. From the runner's answer on, the work is finished on a context the
	// person's closed tab cannot cancel.
	_, err = p.PauseAgent(ctx, run.SandboxRef, gen)
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), desktopAgentCallWait)
	defer cancel()
	now := time.Now().UTC().Truncate(time.Microsecond) // Postgres keeps microseconds: the hold names the row's paused_at
	stage := "pause"
	if err == nil {
		stage = "record"
		// Marked only under the run's lock: with it lost, another pause may have stopped and marked the
		// agent since.
		if err = s.lockHeld(bg); err == nil {
			var marked bool
			if marked, err = st.PauseDesktopVMAgent(bg, run.ID, principal, now, gen); err == nil && !marked {
				err = errDesktopSignInUnmarked
			}
		}
	}
	if errors.Is(err, errDesktopSignInUnmarked) {
		// A mark that changed nothing: another pause marked the row first, or took a newer generation,
		// and owns the stopped agent, so it is not continued here, and that pause's row is the answer.
		if cur, _, rerr := st.DesktopVMSignIns(bg, run.ID); rerr == nil && cur.PausedAt != nil {
			unlock()
			s.supersededBy(bg, w, run, at, principal, cur, map[string]any{"error": err.Error()})
			return
		}
	}
	if err != nil {
		newer, restop, ok := s.continueStoppedAgent(base, r.Context(), bg, p, run, st, unlock, gen)
		unlock()
		switch {
		case !ok:
			s.recordAudit(bg, s.auditEvent(&run.ID, at, principal, auditDesktopSignIn, run.ID.String(), "failure",
				mustJSON(map[string]any{"stage": "lock_lost", "error": err.Error()})))
			w.Header().Set("Retry-After", "5")
			writeErrorReason(w, http.StatusServiceUnavailable, reasonLockUnavailable, lockUnavailableMsg)
			return
		case restop != nil:
			s.restopFailed(bg, w, run, at, principal, *newer, restop, map[string]any{"paused_at": now})
			return
		case newer != nil:
			s.supersededBy(bg, w, run, at, principal, *newer, map[string]any{"error": err.Error()})
			return
		}
		s.recordAudit(bg, s.auditEvent(&run.ID, at, principal, auditDesktopSignIn, run.ID.String(), "failure",
			mustJSON(map[string]any{"stage": stage, "error": err.Error()})))
		if stage == "record" {
			writeErrorReason(w, http.StatusConflict, reasonDesktopSignInsPauseFailed, "The agent's pause could not be recorded; try again")
			return
		}
		status := http.StatusBadGateway
		if errors.Is(err, runner.ErrAgentPauseUnsupported) {
			status = http.StatusConflict
		}
		writeDesktopPauseFailed(w, status)
		return
	}
	// The desktop's screen recording keeps no frames from here until the resume ends this hold, as a
	// container's sign-in does. A run that ends while paused drops the hold with its last connection
	// (forgetEndedDesktopPause).
	if s.desktopRecorded(run) {
		s.PauseDesktopRecording(bg, run, desktopPauseSignin, vmSignInHold(run.ID, now))
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

// continueStoppedAgent continues the agent a failed pause of generation gen may have stopped, so it is
// not left stopped with no mark, which no resume would continue: a stop can land even when the runner
// answers an error. It sends under the run's lock, and the continue is cancelled once that lock is seen
// lost during it (lockBound). With the lock seen lost before or after it, unlock releases the lost
// hold, the lock is taken again and the row read again (reconcileLostLock): newer is the row of a pause
// that has marked it since, which owns the stopped agent and whose resume continues it. The continue
// carries gen, so a guest agent that orders them drops it once a newer pause's stop has reached it; on
// one that predates that, this narrows the gap a lost lock opens and does not close it
// (docs/operations/linux-vm-sandboxes.md). ok is false when the lock could not be taken again: nothing
// more is sent. restop is the error of a stop again that failed.
func (s *Server) continueStoppedAgent(base, lockCtx, bg context.Context, p runner.AgentPauser, run types.AgentRun,
	st store.DesktopVMSignInStore, unlock func(), gen int64) (newer *store.DesktopVMSignIns, restop error, ok bool) {
	sent := false
	if s.lockHeld(bg) == nil {
		send, stop := s.lockBound(lockCtx, bg)
		_, _ = p.ResumeAgent(send, run.SandboxRef, gen)
		stop()
		if s.lockHeld(bg) == nil {
			return nil, nil, true
		}
		sent = true
	}
	unlock()
	lctx, cur, relocked, restop, ok := s.reconcileLostLock(base, p, run, st, nil, sent)
	if !ok {
		return nil, nil, false
	}
	defer relocked()
	if cur.PausedAt != nil {
		return &cur, restop, true
	}
	_, _ = p.ResumeAgent(lctx, run.SandboxRef, cur.PauseGeneration)
	return nil, nil, true
}

// reconcileLostLock takes the run's lock again for a pause or resume that lost it, and reads the row
// under it (desktopRelock). A mark other than own (the pause a resume read; nil for a pause) is a newer
// pause's, which owns the stopped agent. When this request's continue may have reached the runner
// (sent), it may land after that pause's stop, so the agent is stopped again here, with the row's
// generation, under the lock that pause's resume needs: stopping a stopped agent changes nothing. A
// guest agent that orders requests by generation drops that continue whenever it lands after either
// stop; one that predates it does not, so a continue it already has may still take effect after this
// stop too. restop is that stop's error, which nothing retries (restopFailed).
func (s *Server) reconcileLostLock(base context.Context, p runner.AgentPauser, run types.AgentRun, st store.DesktopVMSignInStore,
	own *time.Time, sent bool) (ctx context.Context, row store.DesktopVMSignIns, relocked func(), restop error, ok bool) {
	ctx, row, relocked, ok = s.desktopRelock(base, run, st)
	if ok && sent && row.PausedAt != nil && (own == nil || !row.PausedAt.Equal(*own)) {
		_, restop = p.PauseAgent(ctx, run.SandboxRef, row.PauseGeneration)
	}
	return ctx, row, relocked, restop, ok
}

// lockBound is bg, cancelled too, with db.ErrLockLost, once the run's lock that lockCtx carries is seen
// lost, and never by the person's closed tab. A lock's context the tab ended first keeps that cause for
// good (db.LockHeld), so from then on the lock itself is asked, every db.LockWatchInterval. A call the
// runner already received still lands: the lock check after the call and reconcileLostLock narrow that
// gap and do not close it. stop releases it.
func (s *Server) lockBound(lockCtx, bg context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(bg)
	after := context.AfterFunc(lockCtx, func() {
		if cause := context.Cause(lockCtx); errors.Is(cause, db.ErrLockLost) {
			cancel(cause)
			return
		}
		t := time.NewTicker(db.LockWatchInterval)
		defer t.Stop()
		for {
			if err := s.lockHeld(bg); err != nil {
				cancel(err)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	})
	return ctx, func() { after(); cancel(nil) }
}

// supersededBy answers a pause or resume that lost to a newer pause whose mark (cur) stands: its
// failure row, stage lock_lost with superseded_by that pause's paused_at, then that pause's view.
func (s *Server) supersededBy(ctx context.Context, w http.ResponseWriter, run types.AgentRun, at types.ActorType, principal string,
	cur store.DesktopVMSignIns, data map[string]any) {
	data["stage"], data["superseded_by"] = "lock_lost", cur.PausedAt
	s.recordAudit(ctx, s.auditEvent(&run.ID, at, principal, auditDesktopSignIn, run.ID.String(), "failure", mustJSON(data)))
	writeJSON(w, http.StatusOK, s.desktopSignInsView(ctx, run, cur))
}

// restopFailed answers a pause or resume whose stop again, for the newer pause whose mark (cur) stands,
// failed: the agent may run under that mark, so no paused view is given. Its failure row carries the
// error and both pauses' paused_at; nothing retries the stop.
func (s *Server) restopFailed(ctx context.Context, w http.ResponseWriter, run types.AgentRun, at types.ActorType, principal string,
	cur store.DesktopVMSignIns, err error, data map[string]any) {
	data["stage"], data["superseded_by"], data["error"] = "restop", cur.PausedAt, err.Error()
	s.recordAudit(ctx, s.auditEvent(&run.ID, at, principal, auditDesktopSignIn, run.ID.String(), "failure", mustJSON(data)))
	writeDesktopPauseFailed(w, http.StatusBadGateway)
}

// writeDesktopPauseFailed refuses a pause that left the agent running, or may have.
func writeDesktopPauseFailed(w http.ResponseWriter, status int) {
	writeErrorReason(w, status, reasonDesktopSignInsPauseFailed, "the agent could not be paused; it keeps running")
}

// errDesktopSignInUnmarked is an acked pause whose row was no longer a 'memory' row with no mark.
var errDesktopSignInUnmarked = errors.New("the run's sign-in row was not marked paused")

// vmSignInHold is a VM sign-in pause's hold on the run's desktop recording, named by the pause's
// paused_at, so ending one pause's hold never ends a newer pause's.
func vmSignInHold(run uuid.UUID, pausedAt time.Time) string {
	return vmSignInHolds(run) + strconv.FormatInt(pausedAt.UnixMicro(), 10)
}

// vmSignInHolds starts every vmSignInHold of run.
func vmSignInHolds(run uuid.UUID) string {
	return "vm-sign-in:" + run.String() + ":"
}

// vmSignInRecheck is how often a recorded VM desktop stream re-reads its run's sign-in mark, and
// vmSignInReadWait bounds each re-read, under it. A re-read that fails, has not answered by its
// deadline, or is still in flight at the next recheck leaves the mark unknown, and holds the
// recording until one answers. So a pause answered on another replica holds an open stream within
// vmSignInRecheck plus vmSignInReadWait, under the 2 seconds the docs promise.
const (
	vmSignInRecheck  = time.Second
	vmSignInReadWait = 900 * time.Millisecond
)

// vmSignInWatch keeps one recorded desktop stream's recording held while its run has an open take over,
// or, on a Linux VM, its row says the agent is paused to sign in, or either cannot be read. The hold a
// take over or pause takes lives in the memory of the replica that answered it, so after a restart, or
// on another replica, these reads are what keep the stream from recording the person's session.
type vmSignInWatch struct {
	s   *Server
	run types.AgentRun
	st  store.DesktopVMSignInStore // nil off a Linux VM: only a VM has sign-in rows
	tk  store.OpenTakeoverStore    // nil for a store that keeps no take overs
	// unread is this stream's own hold while a read fails, and unreadReason the gap it opens.
	unread       string
	unreadReason string

	mu      sync.Mutex
	reading bool // a re-read is in flight
	stopped bool // the stream has closed: unread is held no more
}

// watchVMSignIn reads the open take overs and, on a Linux VM, the sign-in mark of a desktop stream
// about to open on runID, holding its recording from the first frame while one is open or the row
// marks a pause. It is nil for a run whose screen is not recorded or whose store keeps neither. A run,
// row or take over that cannot be read is an error: the stream is refused, never recorded on a state
// nobody read.
func (s *Server) watchVMSignIn(ctx context.Context, runID uuid.UUID) (*vmSignInWatch, error) {
	st, signIns := s.cfg.Store.(store.DesktopVMSignInStore)
	tk, takeovers := s.cfg.Store.(store.OpenTakeoverStore)
	if !signIns && !takeovers {
		return nil, nil
	}
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: a desktop's run could not be read for its sign-in mark and take overs; the stream is refused", "run_id", runID, "err", err)
		return nil, err
	}
	if !run.MachineKind.IsVM() {
		st = nil
	}
	if (st == nil && tk == nil) || !s.desktopRecorded(run) {
		return nil, nil
	}
	w := &vmSignInWatch{s: s, run: run, st: st, tk: tk, unreadReason: desktopPauseUnread,
		unread: "vm-sign-in-unread:" + runID.String() + ":" + uuid.NewString()}
	if err := w.sync(ctx); err != nil {
		return nil, err
	}
	// The second read ends a hold the first took of a pause whose resume has since cleared it: that
	// resume's endDesktopHold ran before the hold was taken. One that fails holds the recording until
	// a re-read answers.
	if err := w.sync(ctx); err != nil {
		w.holdUnread(true)
	}
	return w, nil
}

// sync holds the recording while the row marks a pause or a take over is open, and ends this
// replica's hold of a pause the row has since moved past, of a take over handed back elsewhere, and
// the stream's own hold. A take over's hold carries its id (takeoverHold), the one the replica that
// answered it holds: taking it again joins that hold, so a replica keeps one gap and one pause row,
// which the hand back's one resume ends. It ends only holds taken before its read: one taken after is
// of a pause or take over the read may not have seen. A read that fails, or answers only after ctx has ended, changes nothing and
// is an error: the caller holds the recording or refuses the stream.
func (w *vmSignInWatch) sync(ctx context.Context) error {
	if w == nil {
		return nil
	}
	s, run, bg := w.s, w.run, w.s.cfg.BaseCtx
	before := s.desktopRecs.heldWith(run.ID, vmSignInHolds(run.ID))
	tookBefore := s.desktopRecs.heldWith(run.ID, takeoverHolds)
	row, found, open, err := w.read(ctx)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: a desktop's sign-in mark or take overs could not be read; its recording keeps no frames until they are", "run_id", run.ID, "err", err)
		return err
	}
	current := ""
	if found && row.PausedAt != nil {
		current = vmSignInHold(run.ID, *row.PausedAt)
		s.PauseDesktopRecording(bg, run, desktopPauseSignin, current)
	}
	held := make([]string, 0, len(open))
	for _, id := range open {
		held = append(held, takeoverHold(id))
		s.PauseDesktopRecording(bg, run, desktopPauseTakeover, takeoverHold(id))
	}
	for _, h := range before {
		if h != current {
			s.ResumeDesktopRecording(bg, run, h)
		}
	}
	for _, h := range tookBefore {
		if !slices.Contains(held, h) {
			s.ResumeDesktopRecording(bg, run, h)
		}
	}
	w.holdUnread(false)
	return nil
}

// read is the run's sign-in row and open take overs, each where the watch keeps it. One answered only
// after ctx has ended is an error: a pause or take over may have landed since.
func (w *vmSignInWatch) read(ctx context.Context) (row store.DesktopVMSignIns, found bool, open []uuid.UUID, err error) {
	if w.st != nil {
		row, found, err = w.st.DesktopVMSignIns(ctx, w.run.ID)
	}
	if err == nil && w.tk != nil {
		open, err = w.tk.OpenTakeovers(ctx, w.run.ID)
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return row, found, open, err
}

// watch rechecks the row every vmSignInRecheck until ctx ends, on a goroutine of its own, so nothing
// else the stream waits on delays it. s.vmSignInTick, when set, replaces the ticker (tests).
func (w *vmSignInWatch) watch(ctx context.Context) {
	if w == nil {
		return
	}
	var tick <-chan time.Time
	if w.s.vmSignInTick == nil {
		t := time.NewTicker(vmSignInRecheck)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			w.recheck(ctx, nil)
		case done := <-w.s.vmSignInTick:
			w.recheck(ctx, done)
		}
	}
}

// recheck re-reads the row on a goroutine of its own, bounded by vmSignInReadWait, so a read that
// stalls never keeps the stream recording: one still unanswered at the next recheck holds the
// recording, as a failed or late one does, until a re-read answers. A stream closing (ctx ended) takes
// no hold. done, when set, is closed once this recheck's read has answered, or at once when one was
// still in flight (tests).
func (w *vmSignInWatch) recheck(ctx context.Context, done chan struct{}) {
	finish := func() {
		if done != nil {
			close(done)
		}
	}
	w.mu.Lock()
	stalled := w.reading
	w.reading = true
	w.mu.Unlock()
	if stalled {
		if ctx.Err() == nil {
			w.holdUnread(true)
		}
		finish()
		return
	}
	go func() {
		defer finish()
		rctx, cancel := context.WithTimeout(ctx, vmSignInReadWait)
		defer cancel()
		if err := w.sync(rctx); err != nil && ctx.Err() == nil {
			w.holdUnread(true)
		}
		w.mu.Lock()
		w.reading = false
		w.mu.Unlock()
	}()
}

// holdUnread takes or ends the stream's own hold. One taken as the stream closes is ended again, so
// it never outlives the stream. mu is never held across the recording's own calls, which write audit
// rows: the watch must not wait on them.
func (w *vmSignInWatch) holdUnread(hold bool) {
	bg := w.s.cfg.BaseCtx
	if !hold || w.isStopped() {
		w.s.ResumeDesktopRecording(bg, w.run, w.unread)
		return
	}
	w.s.PauseDesktopRecording(bg, w.run, w.unreadReason, w.unread)
	if w.isStopped() {
		w.s.ResumeDesktopRecording(bg, w.run, w.unread)
	}
}

func (w *vmSignInWatch) isStopped() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stopped
}

// stop ends the stream's own hold as it closes. A pause's hold stays: it is the pause's, and its
// resume ends it.
func (w *vmSignInWatch) stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.stopped = true
	w.mu.Unlock()
	w.holdUnread(false)
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
	fenced, err := p.ResumeAgent(ctx, run.SandboxRef, row.PauseGeneration)
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), desktopAgentCallWait)
	defer cancel()
	failed := func(stage string) { // the continue answered an error: the mark stays
		unlock()
		s.recordAudit(bg, s.auditEvent(&run.ID, at, principal, auditDesktopSignIn, run.ID.String(), "failure",
			mustJSON(map[string]any{"stage": stage, "paused_by": row.PausedBy, "paused_at": row.PausedAt, "error": err.Error()})))
		writeErrorReason(w, http.StatusBadGateway, reasonDesktopSignInsResumeFailed, "the agent could not be resumed; it is still paused. Try again")
	}
	// The agent runs again: clear its mark even if the person's tab has closed, or the run would say
	// paused while its agent runs. Cleared only under the run's lock. One lost before or during the
	// call, whether or not it answered an error (one may land after its answer), is taken again and the
	// row read again (reconcileLostLock): a mark that is no longer the one this resume read is a newer
	// pause's, left to its own resume.
	write, relocked := bg, func() {}
	if s.lockHeld(bg) != nil {
		unlock()
		var lctx context.Context
		var cur store.DesktopVMSignIns
		var restop error
		if lctx, cur, relocked, restop, ok = s.reconcileLostLock(base, p, run, st, row.PausedAt, true); !ok {
			if err != nil {
				failed("lock_lost")
				return
			}
			s.recordAudit(bg, s.auditEvent(&run.ID, at, principal, auditDesktopSignIn, run.ID.String(), "failure",
				mustJSON(map[string]any{"stage": "lock_lost", "paused_by": row.PausedBy, "paused_at": row.PausedAt})))
			writeErrorReason(w, http.StatusServiceUnavailable, reasonDesktopSignInsResumeFailed, desktopResumeUnrecordedMsg)
			return
		}
		defer relocked()
		if cur.PausedAt == nil || !cur.PausedAt.Equal(*row.PausedAt) {
			relocked()
			if restop != nil {
				s.restopFailed(bg, w, run, at, principal, cur, restop, map[string]any{"paused_by": row.PausedBy, "paused_at": row.PausedAt})
				return
			}
			s.resumeOvertaken(bg, w, run, at, principal, row, cur)
			return
		}
		write = lctx
	}
	if err != nil {
		relocked()
		failed("resume")
		return
	}
	now := time.Now().UTC()
	by, pausedAt, cleared, err := st.ResumeDesktopVMAgent(write, run.ID, *row.PausedAt, row.PauseGeneration)
	if err != nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonDesktopSignInsResumeFailed, desktopResumeUnrecordedMsg)
		return
	}
	if !cleared {
		// The row no longer marks the pause this resume read: another resume cleared it, or a newer
		// pause marked it since, whose mark stays for its own resume. The answer is what it says.
		cur, _, err := st.DesktopVMSignIns(write, run.ID)
		relocked()
		unlock()
		if err != nil {
			writeErrorReason(w, http.StatusServiceUnavailable, reasonDesktopSignInsUnavailable, desktopSignInsUnreadableMsg)
			return
		}
		s.resumeOvertaken(bg, w, run, at, principal, row, cur)
		return
	}
	s.endDesktopHold(bg, run, vmSignInHold(run.ID, pausedAt))
	relocked()
	unlock()
	s.recordAudit(bg, s.auditEvent(&run.ID, at, principal, auditDesktopSignIn, run.ID.String(), "success",
		mustJSON(map[string]any{"paused_by": by, "paused_at": pausedAt, "resumed_at": now, "paused_seconds": int(now.Sub(pausedAt).Seconds()),
			"fenced": fenced})))
	row.PausedBy, row.PausedAt = "", nil
	writeJSON(w, http.StatusOK, s.desktopSignInsView(bg, run, row))
}

// resumeOvertaken answers a resume whose pause (row) the row no longer marks (cur): another resume
// cleared it, or a newer pause marked it since, which supersedes this resume.
func (s *Server) resumeOvertaken(ctx context.Context, w http.ResponseWriter, run types.AgentRun, at types.ActorType, principal string,
	row, cur store.DesktopVMSignIns) {
	if cur.PausedAt != nil && !cur.PausedAt.Equal(*row.PausedAt) {
		s.supersededBy(ctx, w, run, at, principal, cur, map[string]any{"paused_by": row.PausedBy, "paused_at": row.PausedAt})
		return
	}
	writeJSON(w, http.StatusOK, s.desktopSignInsView(ctx, run, cur))
}
