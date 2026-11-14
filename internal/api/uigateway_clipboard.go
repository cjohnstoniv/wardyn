// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/rfbfilter"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// validateDesktopControls refuses a desktop rule that is neither allowed nor blocked, or review on any direction
// but download (types.ValidateDesktopControls).
func validateDesktopControls(d *types.DesktopControls) error {
	return types.ValidateDesktopControls(d)
}

// errNoPolicyEnvelope is a run with no run.policy.resolve envelope to read its desktop rules from.
var errNoPolicyEnvelope = errors.New("the run has no recorded policy envelope")

// desktopRulesUnreadableMsg is the refusal when runDesktopPolicy cannot answer.
const desktopRulesUnreadableMsg = "This run's desktop rules could not be read. Try again."

// runDesktopPolicy is one read of the run's run.policy.resolve envelope (effectiveUIApps' source):
// its UI apps, its effective desktop rules under its data_out rule (types.WithDataOut), and that rule, all
// from the same snapshot. A failed read, or a run with no envelope, is an error and never the defaults, which
// allow paste in and upload where the run's rules may block them. The defaults are only for an envelope that
// has no desktop block.
func (s *Server) runDesktopPolicy(ctx context.Context, runID uuid.UUID) ([]types.UIApp, types.DesktopControls, types.TransferRule, error) {
	envelope, _, ok, err := s.runEffectivePolicy(ctx, runID)
	if err != nil {
		return nil, types.DesktopControls{}, "", err
	}
	if !ok {
		return nil, types.DesktopControls{}, "", errNoPolicyEnvelope
	}
	return envelope.UIApps, types.WithDataOut(envelope.Desktop, envelope.DataOut), envelope.DataOut.DataOut(), nil
}

// uiDesktopApp is whether app is a desktop the run's desktop rules govern: the noVNC image's
// app, declared as "novnc" (docs/UI-SANDBOXES.md), and the person's sign-in display inside it
// (desktopSigninApp). Every other app's sockets relay unfiltered.
func uiDesktopApp(app string) bool { return app == "novnc" || app == desktopSigninApp }

// uiFilterUpgrade puts the clipboard filter on a desktop's relayed WebSocket. That socket is RFB
// or it closes (rfbfilter): a desktop whose server does not speak RFB is closed and audited. A 101
// that negotiates an extension is refused: uiRewrite offers none on a desktop, and a compressed
// frame would hide the RFB stream from the filter. The browser's frames are checked first, as a
// WebSocket server checks a client's (rfbfilter.CheckBrowserFrames): one that breaks the protocol is
// refused here, and named, rather than passed on to the desktop's own server.
// The check is on ReverseProxy's writes to the desktop, so every byte the desktop gets goes through it,
// and it checks them up to the browser's close frame; what net/http buffered past the upgrade request is
// not relayed at all. Other apps' sockets pass untouched.
func (s *Server) uiFilterUpgrade(resp *http.Response, sess uiSession) error {
	if !uiDesktopApp(sess.App) {
		return nil
	}
	if resp.Header.Get("Sec-WebSocket-Extensions") != "" {
		return errors.New("the sandbox negotiated a WebSocket extension the relay did not offer")
	}
	body, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		return nil // ReverseProxy refuses a 101 without a writable body itself
	}
	ctx := resp.Request.Context()
	_, d, _, err := s.runDesktopPolicy(ctx, sess.Run)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: desktop rules unreadable; the desktop socket is refused", "run_id", sess.Run, "err", err)
		return uiFail(ctx, http.StatusServiceUnavailable, reasonDesktopRulesUnreadable, desktopRulesUnreadableMsg)
	}
	rules := rfbfilter.Rules{PasteIn: d.PasteIn == types.TransferAllowed, CopyOut: d.CopyOut == types.TransferAllowed}
	// The dialed socket under body is the one recordDesktopConn records (Rules.Recorded). A run that cannot be
	// read is taken as recorded: that only refuses pixels in a format the browser does not ask for.
	run, rerr := s.cfg.Store.GetRun(ctx, sess.Run)
	rules.Recorded = rerr != nil || s.recordsDesktopApp(run, sess)
	rec := desktopConnRecorder(resp.Request)
	filtered := rfbfilter.Wrap(body, rules, func(ev rfbfilter.Event) { s.auditUIClipboard(sess, ev) }, func(err error) {
		slog.WarnContext(ctx, "wardynd: ui relay closed a desktop socket it could not follow", "run_id", sess.Run, "app", sess.App, "err", err)
		stopRefusedRecording(rec, err)
		if errors.Is(err, rfbfilter.ErrNotRFB) {
			s.auditUISession(sess, "ui.clipboard.transfer", "", "failure", map[string]any{"app": sess.App, "reason": "not_rfb"})
		}
	})
	if s.uiDesktopSink != nil {
		filtered.SetSink(s.uiDesktopSink(sess))
	}
	resp.Body = filtered.CheckBrowserFrames()
	return nil
}

// auditUIClipboard records one cut text by direction and size. Never its content, nor a digest
// of it (a hash of a pasted password confirms a guess offline): the filter hands over neither.
func (s *Server) auditUIClipboard(sess uiSession, ev rfbfilter.Event) {
	if ev.Input != "" {
		s.auditUISession(sess, "ui.input.drop", ev.Input, "denied", map[string]any{"app": sess.App, "input": ev.Input})
		return
	}
	outcome := "success"
	if !ev.Allowed {
		outcome = "denied"
	}
	s.auditUISession(sess, "ui.clipboard.transfer", string(ev.Direction), outcome,
		map[string]any{"app": sess.App, "direction": string(ev.Direction), "bytes": ev.Bytes})
}
