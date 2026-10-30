// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An app's ticket is how a browser opens an inbound app (ui_apps[].inbound) as someone other than the run's
// owner: the attach ticket of POST /runs/{id}/attach/ticket?purpose=ui, recording the app it opens and the
// callers entry its person matched. The gateway's enter redeems it for that app only (uiTicketMayEnter),
// and the terminal WebSocket refuses it (refuseAttachEntry).

// inboundNeedsHostModeMsg is the refusal of a named caller on a deployment that serves apps from one host.
const inboundNeedsHostModeMsg = "This deployment serves apps from one shared host, so only the run's owner can open them in a browser. Ask an administrator to set per-run hosts."

// inboundDenyNeedsHostMode is the inbound.deny reason of that refusal.
const inboundDenyNeedsHostMode = "needs_host_mode"

// mountInboundTicketRoutes registers the app's ticket on the authenticated group.
func (s *Server) mountInboundTicketRoutes(r chi.Router) {
	r.Post("/runs/{id}/apps/{app}/ticket", s.handleAppTicket)
}

// handleAppTicket mints a single-use ticket that opens one inbound app in a browser:
//
//	POST /api/v1/runs/{id}/apps/{app}/ticket
//
// It admits the run's owner or its service identity's sponsor (inboundCaller's owner arm, never a role), and
// a person the app's callers list names by subject, by an email sign-in vouches for, or by a group of their
// sign-in. Anything else (no such run, an app the run does not declare inbound, a person who is neither) is
// the foreign-run 404, with an authz.denied row when the run exists. A named caller needs host mode: in
// path mode every run's apps share one origin, where the caller's own apps could script this one, so the
// answer is 403 inbound_needs_host_mode, audited as inbound.deny. A background run is no refusal: an
// inbound app is the one door it keeps.
func (s *Server) handleAppTicket(w http.ResponseWriter, r *http.Request) {
	// The ticket's authority time, taken before any store call (handleAttachTicket's convention).
	authorizedAt := s.cfg.Now().UTC()
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunOr404(w, r, id)
	if !ok {
		return
	}
	ctx := r.Context()
	at, principal := actorFromRequest(r)
	app, declared := s.inboundApp(ctx, run.ID, chi.URLParam(r, "app"))
	caller, admitted := "", false
	if declared {
		caller, admitted = s.inboundCaller(ctx, run, app, principal, oidcEmailFromContext(ctx), oidcGroupsFromContext(ctx), authorizedAt)
	}
	if !admitted {
		s.refuse(w, r, authz.Deny(authz.ReasonAttachTicketForeignRun, run.ID.String(), "run not found").OnRun(run.ID).AsIf(authz.Reason(reasonRunNotFound)))
		return
	}
	if caller != "" && s.uiRunOrigin(run.ID) == "" {
		if s.authFailedAllowed() {
			ev := s.auditEvent(&run.ID, at, principal, "inbound.deny", app.Name, "denied", mustJSON(map[string]any{
				"reason": inboundDenyNeedsHostMode, "via": "session", "caller": caller, "app": app.Name, "port": app.Port}))
			ev.SourceIP = r.RemoteAddr
			s.recordAudit(ctx, ev)
		}
		writeErrorReason(w, http.StatusForbidden, reasonInboundNeedsHostMode, inboundNeedsHostModeMsg)
		return
	}
	if run.State != types.RunRunning {
		writeErrorReason(w, http.StatusConflict, reasonAttachNotRunning, "run is not RUNNING; cannot open a UI app (state="+string(run.State)+")")
		return
	}
	if runIsKept(run) {
		writeErrorReason(w, http.StatusConflict, reasonAttachRunKept, "run has ended; cannot open a UI app")
		return
	}
	// Member-stamped whoever mints it: no role reaches an inbound app, so the enter's owner rule asks the
	// same question this route did.
	s.issueAttachTicket(w, r, store.AttachTicket{RunID: run.ID, ActorType: at, Principal: principal, Role: oidc.RoleUser,
		Caller: caller, App: app.Name}, authorizedAt)
}

// uiTicketMayEnter is the gateway enter's entry rule for a redeemed ticket. A ticket minted for one app opens
// that app only. A caller's ticket opens it while the run declares it inbound with the callers entry the
// ticket was minted under (a run's callers are fixed at create), and never passes the owner rule; any other
// ticket answers to the owner rule, the sponsor arm included.
func (s *Server) uiTicketMayEnter(ctx context.Context, run types.AgentRun, ta ticketActor, app string, superAdmin bool) bool {
	if ta.app != "" && ta.app != app {
		return false
	}
	if ta.caller == "" {
		return s.mayEnterRunOrSponsor(ctx, run, ta.principal, superAdmin, ta.authorizedAt)
	}
	declared, ok := s.inboundApp(ctx, run.ID, app)
	return ok && slices.Contains(declared.Callers, ta.caller)
}
