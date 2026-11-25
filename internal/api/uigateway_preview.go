// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// Preview URLs at the UI gateway (run_previews.go holds the routes). A live preview row is presented as the app
// types.PreviewAppPrefix + port: a private row as an owner app, which enters by the run's entry rule like every
// declared app, and an org row as an inbound app whose callers list holds the one reserved entry
// previewCallerOrg. Every door the gateway already has applies to it, and four more rules hold for it alone:
//
//   - its session is re-checked against the row, read fresh, on every re-check (uiPreviewDenies);
//   - every relayed request of it is a watched stream, cut within one re-check of losing its authority
//     (previewWatchCtx);
//   - the port is probed, never launched (uiPreviewProbe);
//   - viewing it is never the run's activity: no relayed request, dial or keepalive tick of a preview moves the
//     run's idle or pause clock, and a paused run stays paused (uiPreviewThaw, keepaliveNotPresence).

// previewCallerPublic is the reserved callers entry a public link's visitor's ticket carries.
const previewCallerPublic = "preview:public"

// The gateway's refusal sentences for a preview, each beside its reason in reasons_previews.go.
const (
	previewRunPausedMsg    = "This preview's run is paused. It opens again when the run is resumed."
	previewNotListeningMsg = "Nothing is listening on port %d in this run yet."
)

// uiPreviewSession reports whether sess is a preview's session, by its app's name: declared names start
// alphanumeric (policy.go), so no declared app has the prefix.
func uiPreviewSession(sess uiSession) bool {
	return strings.HasPrefix(sess.App, types.PreviewAppPrefix)
}

// withPreviewApps is apps with runID's live previews added, one app each. It adds nothing while previews are
// off, on a store that keeps none, or when the site config or the rows cannot be read: a preview that cannot be
// read is not declared, so every door fails closed on it.
func (s *Server) withPreviewApps(ctx context.Context, runID uuid.UUID, apps []types.UIApp) []types.UIApp {
	ps, ok := s.cfg.Store.(runPreviewStore)
	if !ok {
		return apps
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: ui gateway could not read the site config; the run's previews are not served", "run_id", runID, "err", err)
		return apps
	}
	if !previewURLsOn(sc) {
		return apps
	}
	rows, err := ps.ListRunPreviews(ctx, runID)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: ui gateway could not read the run's previews; none is served", "run_id", runID, "err", err)
		return apps
	}
	out := apps[:len(apps):len(apps)] // an append never writes into the caller's array
	for _, p := range rows {
		a := types.UIApp{Name: previewApp(p.Port), Port: p.Port, Path: "/"}
		switch p.Visibility {
		case types.PreviewOrg:
			a.Inbound, a.Callers = true, []string{previewCallerOrg}
		case types.PreviewPublic:
			a.Inbound, a.Callers = true, []string{previewCallerOrg, previewCallerPublic}
		}
		out = append(out, a)
	}
	return out
}

// previewAdmits reports whether a row of visibility vis admits a session entered as caller: the run's entry rule
// ("") any row, an org caller an org or public row, and a public link's visitor a public row.
func previewAdmits(vis types.PreviewVisibility, caller string) bool {
	switch caller {
	case "":
		return vis.Valid()
	case previewCallerOrg:
		return vis == types.PreviewOrg || vis == types.PreviewPublic
	case previewCallerPublic:
		return vis == types.PreviewPublic
	}
	return false
}

// uiPreviewDenies is uiSessionArms' preview arm, nil for any other session. A preview's session holds while no
// support grant rides it (a helper never enters a preview), the run's UI apps are not gone, previews are on, the
// port's row is live and admits the session's caller, the run's profile allows previews (previewProfileRefusal,
// for an admin the door exempts too), the row is within the ceiling, host mode serves a row
// wider than private, data_out is allowed, and the run is not background-only, for an inbound session too, so a
// preview is never the background door's exception. The row and the site config are read fresh, and one that
// cannot be read fails closed with the arms' 503.
func (s *Server) uiPreviewDenies(ctx context.Context, sess uiSession, run types.AgentRun) *uiDialError {
	if !uiPreviewSession(sess) {
		return nil
	}
	if sess.SupportGrant != uuid.Nil {
		return s.uiDenyReassert(sess, uiDeniedReasonNotAuthorized, http.StatusForbidden, uiSessionNoLongerAuthorizedMsg)
	}
	if de := uiRunGone(run); de != nil {
		return de
	}
	if backgroundOnlyRefusal(run, "web application gateway") != nil {
		return s.uiDenyReassert(sess, reasonRunBackgroundOnly, http.StatusConflict, runBackgroundOnlyMsg("web application gateway"))
	}
	ps, ok := s.cfg.Store.(runPreviewStore)
	if !ok || sess.App != previewApp(sess.Port) {
		return s.uiDenyReassert(sess, uiDeniedReasonNotAuthorized, http.StatusForbidden, uiSessionNoLongerAuthorizedMsg)
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: ui gateway could not read the site config for a preview", "run_id", sess.Run, "err", err)
		return s.uiDenyReassert(sess, uiDeniedReasonRevocationUnavailable, http.StatusServiceUnavailable, uiSessionUnverifiableMsg)
	}
	if !previewURLsOn(sc) {
		return s.uiDenyReassert(sess, reasonPreviewURLsOff, http.StatusForbidden, previewURLsOffMsg)
	}
	p, err := ps.LiveRunPreview(ctx, sess.Run, sess.Port)
	switch {
	case errors.Is(err, store.ErrNotFound): // revoked or expired
		return s.uiDenyReassert(sess, uiDeniedReasonRevoked, http.StatusForbidden, uiSessionNoLongerAuthorizedMsg)
	case err != nil:
		slog.ErrorContext(ctx, "wardynd: ui gateway could not read a preview", "run_id", sess.Run, "port", sess.Port, "err", err)
		return s.uiDenyReassert(sess, uiDeniedReasonRevocationUnavailable, http.StatusServiceUnavailable, uiSessionUnverifiableMsg)
	}
	if !previewAdmits(p.Visibility, sess.Caller) {
		return s.uiDenyReassert(sess, uiDeniedReasonNotAuthorized, http.StatusForbidden, uiSessionNoLongerAuthorizedMsg)
	}
	if rf := s.previewProfileRefusal(ctx, run); rf != nil {
		if d := rf.decision; d != nil { // a composed profile nothing satisfies
			return s.uiDenyReassert(sess, string(d.Reason), d.Status, d.Sentence)
		}
		return s.uiDenyReassert(sess, rf.body.Reason, rf.status, rf.body.Error)
	}
	if ceiling := previewCeiling(sc); !p.Visibility.AtMost(ceiling) {
		return s.uiDenyReassert(sess, reasonPreviewAboveCeiling, http.StatusForbidden, fmt.Sprintf(previewAboveCeilingMsg, ceiling, p.Visibility))
	}
	if p.Visibility != types.PreviewPrivate && s.uiRunOrigin(run.ID) == "" {
		return s.uiDenyReassert(sess, reasonPreviewNeedsHostMode, http.StatusForbidden, previewNeedsHostModeMsg)
	}
	if rf := s.previewDataOutRefusal(ctx, run); rf != nil {
		return s.uiDenyReassert(sess, rf.body.Reason, rf.status, rf.body.Error)
	}
	return nil
}

// previewWatchCtx is relayWatchCtx for a preview's session: every relayed request, a 101 and a streaming answer
// included, runs on a streamCut whose hard end is the earlier of end and the row's expires_at, read fresh (a
// missing or unreadable row ends it at once), and the full re-check runs every uiReassertInterval from
// checkStart, each lookup bounded by supportWatchWait, as for an issuer portal's session. A check that refuses
// cuts the request, so a revoke, an expiry, a lowered ceiling or visibility, previews switched off, the run
// stopping or a data_out tightening ends an open preview stream within one re-check. The watch only reads.
func (s *Server) previewWatchCtx(ctx context.Context, w http.ResponseWriter, sess uiSession, checkStart, end time.Time) (context.Context, http.ResponseWriter, func()) {
	e := s.cfg.Now()
	if ps, ok := s.cfg.Store.(runPreviewStore); ok {
		if p, err := ps.LiveRunPreview(ctx, sess.Run, sess.Port); err == nil {
			e = p.ExpiresAt
		}
	}
	if !end.IsZero() && end.Before(e) {
		e = end
	}
	ctx, cancel := context.WithCancel(ctx)
	cut := newStreamCut(w, s.uiStreamEnd(sess, e), cancel)
	every := time.Duration(cmp.Or(supportWatchEveryNS.Load(), int64(uiReassertInterval)))
	bound := time.Duration(cmp.Or(supportWatchWaitNS.Load(), int64(supportWatchWait)))
	b := s.newSessionBudget(ctx, uiWatchID(sess, sessionSurfaceUIApp))
	cut.watch(ctx, checkStart.Add(every), every, bound, b, func(lctx context.Context) error { return s.relayStillHolds(lctx, sess) })
	return ctx, cut.Writer(w), func() {
		defer cancel()
		cut.End()
	}
}

// uiPreviewThaw is uiThaw for a preview's session: a preview never wakes a paused run, so a paused run answers
// 409 preview_run_paused and stays paused.
func (s *Server) uiPreviewThaw(ctx context.Context, run types.AgentRun) error {
	if run.PausedAt == nil {
		return nil
	}
	return uiFail(ctx, http.StatusConflict, reasonPreviewRunPaused, previewRunPausedMsg)
}

// uiPreviewProbeCmd is the machine kind's listening check of the port in $p, as uiLauncherScriptFor runs it:
// socat in a container, the guest agent's forward -probe in a VM.
func uiPreviewProbeCmd(kind types.MachineKind) string {
	switch kind {
	case types.MachineLinuxVM:
		return "'" + guestagent.LinuxAgentPath + `' forward -probe "$p"`
	case types.MachineMacOSVM:
		return "'" + guestagent.MacAgentBinaryPath + `' forward -probe "$p"`
	}
	return `socat -u /dev/null "TCP:127.0.0.1:$p"`
}

// uiPreviewProbe is uiEnsureApp for a preview's session: a preview is a port the run already serves, so it is
// probed and never launched. A port probed listening within uiReadyTTL runs no exec; otherwise one exec runs
// the probe alone, an exec that fails answers as uiEnsureApp's does, and a port nothing listens on is 502
// preview_not_listening. The answer never names a path in the sandbox.
func (s *Server) uiPreviewProbe(ctx context.Context, run types.AgentRun, sess uiSession) error {
	key := sess.Run.String() + "/" + sess.App
	if s.uiAppReady(key) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, uiEnsureTimeout)
	defer cancel()
	exec, err := s.execFresh(ctx, run.ID, uiLiveRunWords, runner.ExecSpec{
		Argv: []string{"sh", "-c", fmt.Sprintf("p=%d\n%s >/dev/null 2>&1\n", sess.Port, uiPreviewProbeCmd(run.MachineKind))},
	})
	if err != nil {
		return uiExecFailure(ctx, reasonUIGatewayLauncherExecFailed, "probe a preview's port", err)
	}
	// Both streams are drained before Wait (runner.ExecSession's contract), as uiEnsureApp drains them.
	if exec.Stdout != nil {
		go func() { _, _ = io.Copy(io.Discard, exec.Stdout) }()
	}
	if exec.Stderr != nil {
		go func() { _, _ = io.Copy(io.Discard, exec.Stderr) }()
	}
	code := -1
	if exec.Wait != nil {
		if c, werr := exec.Wait(); werr == nil {
			code = c
		}
	}
	if exec.Close != nil {
		_ = exec.Close()
	}
	if code != 0 {
		return uiFail(ctx, http.StatusBadGateway, reasonPreviewNotListening, fmt.Sprintf(previewNotListeningMsg, sess.Port))
	}
	s.markUIAppReady(key)
	return nil
}

// keepaliveNotPresenceKey marks a context whose connection keepalive touches nothing (keepaliveNotPresence).
type keepaliveNotPresenceKey struct{}

// keepaliveNotPresence is ctx for attachKeepalive: unchanged for any connection but a preview's, and for a
// preview's marked so the keepalive ticks for the connection's life and never moves the run's idle clock.
func keepaliveNotPresence(ctx context.Context, preview bool) context.Context {
	if !preview {
		return ctx
	}
	return context.WithValue(ctx, keepaliveNotPresenceKey{}, true)
}

// keepaliveTouches reports whether attachKeepalive's tick on ctx moves the run's idle clock: on every context but
// a preview connection's.
func keepaliveTouches(ctx context.Context) bool { return ctx.Value(keepaliveNotPresenceKey{}) == nil }
