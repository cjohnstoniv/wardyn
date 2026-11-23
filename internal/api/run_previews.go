// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Preview URLs: a URL on a port a running run serves, opened through the UI gateway as the app
// types.PreviewAppPrefix + port. Private (the run's entry rule) is the default, and none is wider than the
// organisation's ceiling (site_features_previews.go). These routes list, create or change, revoke, and mint the
// ticket that opens one. GET and DELETE answer with the switch off, so an owner can always see and revoke. No
// route here moves the run's idle or pause clock: viewing a preview is never the run's activity.

// previewCallerOrg is the reserved callers entry an org preview's ticket carries: a person of this deployment
// who is not the run's owner.
const previewCallerOrg = "preview:org"

// The refusal sentences of the preview routes, each beside its reason in reasons_previews.go.
const (
	previewURLsOffMsg        = "Preview URLs are off on this deployment. An administrator turns them on with features.preview_urls.enabled in site config."
	previewConsoleOnlyMsg    = "Only a person signed in to the console can create a preview, or open one shared with the organisation."
	previewVisibilityMsg     = "A preview's visibility is private, org or public; %q is none of them."
	previewTTLMsg            = "A preview lives from 1 hour to 7 days (ttl_seconds 3600 to 604800); %d is outside that."
	previewAboveCeilingMsg   = "This deployment allows previews no wider than %s, so a %s preview can't be created or opened."
	previewNotRunningMsg     = "This run is not running, so its previews can't be created or opened."
	previewProfileMsg        = "Previews are not allowed under the governance profile %q this run was launched under."
	previewPortReservedMsg   = "A preview needs a port from 1024 to 65535, other than 3128, which the run's proxy uses."
	previewPortDeclaredMsg   = "Port %d is this run's declared web app %q. Open it as that app."
	previewDataOutMsg        = "This run's data_out rule keeps content in, so it has no previews."
	previewNeedsHostModeMsg  = "Organisation previews need per-run hosts. Ask an administrator to set WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE."
	previewSiteUnreadableMsg = "The site configuration could not be read. Try again."
	previewUnitUnreadableMsg = "This run's governance profile could not be read. Try again."
)

// errPreviewStoreUnsupported is a store that has no run_previews table.
var errPreviewStoreUnsupported = errors.New("this store keeps no run previews")

// runPreviewStore is the store's run_previews table (store_run_previews.go).
type runPreviewStore interface {
	UpsertRunPreview(ctx context.Context, runID uuid.UUID, port int, vis types.PreviewVisibility, ttl time.Duration, by string, linkHash []byte) (types.RunPreview, bool, error)
	ListRunPreviews(ctx context.Context, runID uuid.UUID) ([]types.RunPreview, error)
	LiveRunPreview(ctx context.Context, runID uuid.UUID, port int) (types.RunPreview, error)
	RevokeRunPreview(ctx context.Context, runID uuid.UUID, port int, by string) (types.RunPreview, error)
	RevokeRunPreviewsAbove(ctx context.Context, ceiling types.PreviewVisibility, by string) ([]types.RunPreview, error)
}

// mountRunPreviewRoutes registers a run's preview routes on the authenticated group.
func (s *Server) mountRunPreviewRoutes(r chi.Router) {
	r.Get("/runs/{id}/previews", s.handleListRunPreviews)
	r.Put("/runs/{id}/previews/{port}", s.handlePutRunPreview)
	r.Delete("/runs/{id}/previews/{port}", s.handleDeleteRunPreview)
	r.Post("/runs/{id}/previews/{port}/ticket", s.handleRunPreviewTicket)
}

// previewRequest is PUT /runs/{id}/previews/{port}'s body. An absent visibility is private, on a create and on a
// change alike, whatever the ceiling; an absent ttl_seconds is 24 hours.
type previewRequest struct {
	Visibility string `json:"visibility,omitempty"`
	TTLSeconds *int64 `json:"ttl_seconds,omitempty"`
}

// previewAsk is what runPreviewCheck is asked about. A create (PUT) also runs the body's row and the port's two
// rows; a ticket asks about a row that already exists, whose port and visibility the store gave.
type previewAsk struct {
	create  bool
	port    int
	portOK  bool
	vis     types.PreviewVisibility
	ttl     time.Duration
	body    previewRequest
	bodyErr string
}

// previewPort is the {port} path segment as a port a preview may use: decimal as written, 1024 to 65535, and not
// the proxy's (runner.ProxyListenPort). The desktop sign-in display (1) is below the range.
func previewPort(raw string) (int, bool) {
	p, err := strconv.Atoi(raw)
	if err != nil || strconv.Itoa(p) != raw || p < 1024 || p > 65535 || p == runner.ProxyListenPort {
		return 0, false
	}
	return p, true
}

// previewApp is the name the UI gateway presents a preview of port under.
func previewApp(port int) string { return types.PreviewAppPrefix + strconv.Itoa(port) }

// previewConsolePerson reports whether the request's caller is a person signed in to the console: a human actor
// with a name, not acting through a portal's delegated token, and neither an issuer portal's person nor a public
// link's visitor. A service identity, an agent, a device and the admin token are not human actors.
func previewConsolePerson(r *http.Request) bool {
	at, principal := actorFromRequest(r)
	_, via := audit.DelegationFrom(r.Context())
	return at == types.ActorHuman && principal != "" && !via && !isPortalPersonName(principal) &&
		!strings.HasPrefix(principal, types.PreviewLinkPrincipalPrefix)
}

// settle checks a create's body: decoded, the visibility one of three (absent: private) and the ttl inside its bounds
// (absent: 24 hours).
func (a *previewAsk) settle() *runRefusal {
	if a.bodyErr != "" {
		return runError(http.StatusBadRequest, reasonPreviewInvalid, a.bodyErr)
	}
	a.vis = types.PreviewVisibility(a.body.Visibility)
	if a.vis == "" {
		a.vis = types.PreviewPrivate
	}
	if !a.vis.Valid() {
		return runError(http.StatusBadRequest, reasonPreviewInvalid, fmt.Sprintf(previewVisibilityMsg, a.body.Visibility))
	}
	a.ttl = types.PreviewTTLDefault
	if a.body.TTLSeconds != nil {
		a.ttl = time.Duration(*a.body.TTLSeconds) * time.Second
		if *a.body.TTLSeconds < int64(types.PreviewTTLMin/time.Second) || *a.body.TTLSeconds > int64(types.PreviewTTLMax/time.Second) {
			return runError(http.StatusBadRequest, reasonPreviewInvalid, fmt.Sprintf(previewTTLMsg, *a.body.TTLSeconds))
		}
	}
	return nil
}

// runPreviewCheck asks what a create and a ticket refuse after the caller's own checks, in order: the
// switch (an unreadable site config refuses), the body, the ceiling, the run running, the background door, the
// profile, the port, data_out, and host mode for a preview wider than private. A ticket skips the body and the
// port, and asks the ceiling and host mode about the row it opens.
func (s *Server) runPreviewCheck(ctx context.Context, run types.AgentRun, ask *previewAsk) *runRefusal {
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: a preview's site config could not be read; refusing", "run_id", run.ID, "err", err)
		return runError(http.StatusServiceUnavailable, reasonRosterUnreadable, previewSiteUnreadableMsg)
	}
	if !previewURLsOn(sc) {
		return runError(http.StatusForbidden, reasonPreviewURLsOff, previewURLsOffMsg)
	}
	if ask.create {
		if rf := ask.settle(); rf != nil {
			return rf
		}
	}
	if ceiling := previewCeiling(sc); !ask.vis.AtMost(ceiling) {
		return runError(http.StatusForbidden, reasonPreviewAboveCeiling, fmt.Sprintf(previewAboveCeilingMsg, ceiling, ask.vis))
	}
	if run.State != types.RunRunning || runIsKept(run) {
		return runError(http.StatusConflict, reasonPreviewNotRunning, previewNotRunningMsg)
	}
	if rf := backgroundOnlyRefusal(run, "web application gateway"); rf != nil {
		return rf
	}
	if rf := s.previewProfileRefusal(ctx, run); rf != nil {
		return rf
	}
	if ask.create {
		if rf := s.previewPortRefusal(ctx, run, ask); rf != nil {
			return rf
		}
	}
	if rf := s.previewDataOutRefusal(ctx, run); rf != nil {
		return rf
	}
	if ask.vis != types.PreviewPrivate && s.uiRunOrigin(run.ID) == "" {
		return runError(http.StatusForbidden, reasonPreviewNeedsHostMode, previewNeedsHostModeMsg)
	}
	return nil
}

// previewProfileRefusal is the profile row: a profile that denies UI apps or interactive sessions has no
// previews. A profile that cannot be read refuses.
func (s *Server) previewProfileRefusal(ctx context.Context, run types.AgentRun) *runRefusal {
	p, err := s.runUnit(ctx, run)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: a preview's governance profile could not be read; refusing", "run_id", run.ID, "err", err)
		return runError(http.StatusServiceUnavailable, reasonRunUnreadable, previewUnitUnreadableMsg)
	}
	if p != nil && (p.Limits.DenyUIApps || p.Limits.DenyInteractive) {
		return runError(http.StatusForbidden, reasonPreviewProfile, fmt.Sprintf(previewProfileMsg, p.Name))
	}
	return nil
}

// previewPortRefusal is a create's two port rows: a port a preview may use (previewPort), and none of the run's
// declared web apps' ports.
func (s *Server) previewPortRefusal(ctx context.Context, run types.AgentRun, ask *previewAsk) *runRefusal {
	if !ask.portOK {
		return runError(http.StatusBadRequest, reasonPreviewPortReserved, previewPortReservedMsg)
	}
	apps, err := s.effectiveUIApps(ctx, run.ID)
	if err != nil {
		return runServerError("read the run's web apps", err)
	}
	for _, app := range apps {
		if app.Port == ask.port {
			return runError(http.StatusConflict, reasonPreviewPortDeclared, fmt.Sprintf(previewPortDeclaredMsg, ask.port, app.Name))
		}
	}
	return nil
}

// previewDataOutRefusal is the data_out row: a preview is a web app, so it needs the rule every web app needs
// (refuseAppDataOut). An unreadable rule refuses.
func (s *Server) previewDataOutRefusal(ctx context.Context, run types.AgentRun) *runRefusal {
	rule, _, err := s.runDataOutRule(ctx, run.ID)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: a run's data_out rule is unreadable; the preview is refused", "run_id", run.ID, "err", err)
		return runError(http.StatusServiceUnavailable, reasonDataOutUnreadable, dataOutUnreadableMsg)
	}
	if rule != types.TransferAllowed {
		return runError(http.StatusForbidden, reasonPreviewDataOut, previewDataOutMsg)
	}
	return nil
}

// previewData is a preview row's audit data.
func previewData(p types.RunPreview) map[string]any {
	return map[string]any{"preview_id": p.ID.String(), "port": p.Port, "visibility": string(p.Visibility),
		"expires_at": p.ExpiresAt.UTC().Format(time.RFC3339)}
}

// auditPreview writes one preview row on runID as the request's actor, from the request's address.
func (s *Server) auditPreview(r *http.Request, runID uuid.UUID, port int, action, outcome string, data map[string]any) {
	at, actor := actorFromRequest(r)
	target := ""
	if port > 0 {
		target = previewApp(port)
	}
	ev := s.auditEvent(&runID, at, actor, action, target, outcome, mustJSON(data))
	ev.SourceIP = r.RemoteAddr
	s.recordAudit(r.Context(), ev)
}

// denyPreview answers a refusal of a create or a ticket after the run's entry check, and records it as preview.deny within
// authFailedAllowed's bound. A server fault is logged by its answer and writes no row.
func (s *Server) denyPreview(w http.ResponseWriter, r *http.Request, run types.AgentRun, ask previewAsk, request string, rf *runRefusal) {
	if rf.err == nil && s.authFailedAllowed() {
		data := map[string]any{"request": request, "reason": rf.body.Reason}
		port := 0
		if ask.portOK {
			port, data["port"] = ask.port, ask.port
		}
		if ask.vis.Valid() {
			data["visibility"] = string(ask.vis)
		}
		s.auditPreview(r, run.ID, port, "preview.deny", "denied", data)
	}
	rf.write(s, w, r)
}

// handleListRunPreviews lists a run's live previews:
//
//	GET /api/v1/runs/{id}/previews
//
// for its owner or a security operator (inspect-or-stop), with the switch on or off.
func (s *Server) handleListRunPreviews(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorized(w, r, id)
	if !ok {
		return
	}
	ps, ok := s.cfg.Store.(runPreviewStore)
	if !ok {
		writeServerError(w, r, "read run previews", errPreviewStoreUnsupported)
		return
	}
	rows, err := ps.ListRunPreviews(r.Context(), run.ID)
	if err != nil {
		writeServerError(w, r, "list run previews", err)
		return
	}
	if rows == nil {
		rows = []types.RunPreview{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"previews": rows})
}

// handlePutRunPreview creates or changes the preview of one port:
//
//	PUT /api/v1/runs/{id}/previews/{port}   {"visibility": "private"|"org"|"public", "ttl_seconds": N}
//
// It asks the run's entry first (getRunForEntry: the foreign-run 404, or 403 run_owner_only for a super admin on a
// person's run), then a console person (previewConsolePerson), then runPreviewCheck. The same
// visibility on a live row extends it (preview.update); another one replaces it with a new row (preview.update,
// naming the row it replaced); a port with no live row gets one (preview.create). It answers {"preview": row}.
func (s *Server) handlePutRunPreview(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunForEntry(w, r, id)
	if !ok {
		return
	}
	ask := previewAsk{create: true}
	ask.port, ask.portOK = previewPort(chi.URLParam(r, "port"))
	ask.bodyErr = decodeStrictMsg(w, r, &ask.body)
	if !previewConsolePerson(r) {
		s.denyPreview(w, r, run, ask, "create", runError(http.StatusForbidden, reasonPreviewConsoleOnly, previewConsoleOnlyMsg))
		return
	}
	if rf := s.runPreviewCheck(r.Context(), run, &ask); rf != nil {
		s.denyPreview(w, r, run, ask, "create", rf)
		return
	}
	ps, ok := s.cfg.Store.(runPreviewStore)
	if !ok {
		writeServerError(w, r, "read run previews", errPreviewStoreUnsupported)
		return
	}
	prev, err := ps.LiveRunPreview(r.Context(), run.ID, ask.port)
	had := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeServerError(w, r, "read a run preview", err)
		return
	}
	_, by := actorFromRequest(r)
	p, created, err := ps.UpsertRunPreview(r.Context(), run.ID, ask.port, ask.vis, ask.ttl, by, nil)
	if err != nil {
		writeServerError(w, r, "create a run preview", err)
		return
	}
	data := previewData(p)
	if created && !had {
		s.auditPreview(r, run.ID, p.Port, "preview.create", "success", data)
	} else {
		if created {
			data["replaced"], data["from"] = prev.ID.String(), string(prev.Visibility)
		}
		s.auditPreview(r, run.ID, p.Port, "preview.update", "success", data)
	}
	writeJSON(w, http.StatusOK, map[string]any{"preview": p})
}

// handleDeleteRunPreview revokes the preview of one port:
//
//	DELETE /api/v1/runs/{id}/previews/{port}
//
// for its owner or a security operator (inspect-or-stop), with the switch on or off. A port with no unrevoked
// preview answers the same 204, with no row.
func (s *Server) handleDeleteRunPreview(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorized(w, r, id)
	if !ok {
		return
	}
	port, ok := previewPort(chi.URLParam(r, "port"))
	if !ok {
		writeErrorReason(w, http.StatusBadRequest, reasonPreviewPortReserved, previewPortReservedMsg)
		return
	}
	ps, ok := s.cfg.Store.(runPreviewStore)
	if !ok {
		writeServerError(w, r, "read run previews", errPreviewStoreUnsupported)
		return
	}
	_, by := actorFromRequest(r)
	p, err := ps.RevokeRunPreview(r.Context(), run.ID, port, by)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		writeServerError(w, r, "revoke a run preview", err)
		return
	default:
		s.auditPreview(r, run.ID, p.Port, "preview.revoke", "success", previewData(p))
	}
	w.WriteHeader(http.StatusNoContent)
}

// previewTicketCaller is who may mint the ticket: the live preview of the {port} segment, and the callers entry
// the caller opens it as: "" for a person the run's entry rule admits (mayEnterRunOrSponsor, as an app's ticket
// asks it, never a role), and previewCallerOrg for any other console person on an org or public row. admitted is
// false when there is no live row or the caller is neither.
func (s *Server) previewTicketCaller(r *http.Request, run types.AgentRun, principal string, since time.Time) (p types.RunPreview, caller string, admitted bool, err error) {
	port, ok := previewPort(chi.URLParam(r, "port"))
	if !ok {
		return types.RunPreview{}, "", false, nil
	}
	ps, ok := s.cfg.Store.(runPreviewStore)
	if !ok {
		return types.RunPreview{}, "", false, errPreviewStoreUnsupported
	}
	p, err = ps.LiveRunPreview(r.Context(), run.ID, port)
	if errors.Is(err, store.ErrNotFound) {
		return types.RunPreview{}, "", false, nil
	}
	if err != nil {
		return types.RunPreview{}, "", false, err
	}
	if s.mayEnterRunOrSponsor(r.Context(), run, principal, false, since) {
		return p, "", true, nil
	}
	if p.Visibility != types.PreviewPrivate && previewConsolePerson(r) {
		return p, previewCallerOrg, true, nil
	}
	return types.RunPreview{}, "", false, nil
}

// handleRunPreviewTicket mints the single-use ticket that opens one preview in a browser:
//
//	POST /api/v1/runs/{id}/previews/{port}/ticket
//
// Whoever previewTicketCaller does not admit gets one foreign-run 404, so a person who may not open the preview
// learns nothing about its row or the switch; runPreviewCheck then asks about the row. The ticket is
// member-stamped whoever mints it, so the gateway's enter asks the entry rule this route asked; an org caller's
// ticket enters only while the app lists its callers entry. The answer is the attach ticket's.
func (s *Server) handleRunPreviewTicket(w http.ResponseWriter, r *http.Request) {
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
	at, principal := actorFromRequest(r)
	p, caller, admitted, err := s.previewTicketCaller(r, run, principal, authorizedAt)
	if err != nil {
		writeServerError(w, r, "read a run preview", err)
		return
	}
	if !admitted {
		s.refuse(w, r, authz.Deny(authz.ReasonAttachTicketForeignRun, run.ID.String(), "run not found").OnRun(run.ID).AsIf(authz.Reason(reasonRunNotFound)))
		return
	}
	ask := previewAsk{port: p.Port, portOK: true, vis: p.Visibility}
	if rf := s.runPreviewCheck(r.Context(), run, &ask); rf != nil {
		s.denyPreview(w, r, run, ask, "ticket", rf)
		return
	}
	ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
	s.issueAttachTicket(ww, r, store.AttachTicket{RunID: run.ID, ActorType: at, Principal: principal, Role: oidc.RoleUser,
		Caller: caller, App: previewApp(p.Port)}, authorizedAt)
	if ww.Status() == http.StatusOK {
		data := previewData(p)
		data["caller"] = cmp.Or(caller, "owner")
		s.auditPreview(r, run.ID, p.Port, "preview.open", "success", data)
	}
}
