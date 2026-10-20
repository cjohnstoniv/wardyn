// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/rfbfilter"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// validateDesktopControls refuses a desktop rule that is neither allowed nor blocked.
func validateDesktopControls(d *types.DesktopControls) error {
	if d == nil {
		return nil
	}
	for field, r := range map[string]types.TransferRule{"paste_in": d.PasteIn, "copy_out": d.CopyOut, "upload": d.Upload, "download": d.Download} {
		if !types.ValidTransferRule(r) {
			return fmt.Errorf("desktop.%s: %q is not allowed or blocked", field, r)
		}
	}
	return nil
}

// errNoPolicyEnvelope is a run with no run.policy.resolve envelope to read its desktop rules from.
var errNoPolicyEnvelope = errors.New("the run has no recorded policy envelope")

// desktopRulesUnreadableMsg is the refusal when runDesktopPolicy cannot answer.
const desktopRulesUnreadableMsg = "this run's desktop rules could not be read. Try again."

// runDesktopPolicy is one read of the run's run.policy.resolve envelope (effectiveUIApps' source):
// its UI apps and its effective desktop rules, from the same snapshot. A failed read, or a run with
// no envelope, is an error and never the defaults, which allow paste in and upload where the run's
// rules may block them. The defaults are only for an envelope that has no desktop block.
func (s *Server) runDesktopPolicy(ctx context.Context, runID uuid.UUID) ([]types.UIApp, types.DesktopControls, error) {
	if s.cfg.Store == nil {
		return nil, types.DesktopControls{}, errNoPolicyEnvelope
	}
	events, err := s.cfg.Store.QueryAuditEvents(ctx, runID, effectivePolicyAuditScan)
	if err != nil {
		return nil, types.DesktopControls{}, err
	}
	envelope, _, ok := latestPolicyResolve(events)
	if !ok {
		return nil, types.DesktopControls{}, errNoPolicyEnvelope
	}
	return envelope.UIApps, envelope.Desktop.Effective(), nil
}

// uiDesktopApp is whether app is the desktop the run's desktop rules govern: the noVNC image's
// app, declared as "novnc" (docs/UI-SANDBOXES.md). Every other app's sockets relay unfiltered.
func uiDesktopApp(app string) bool { return app == "novnc" }

// uiFilterUpgrade puts the clipboard filter on a desktop's relayed WebSocket. That socket is RFB
// or it closes (rfbfilter): a desktop whose server does not speak RFB is closed and audited. A 101
// that negotiates an extension is refused: uiRewrite offers none on a desktop, and a compressed
// frame would hide the RFB stream from the filter. Other apps' sockets pass untouched.
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
	_, d, err := s.runDesktopPolicy(ctx, sess.Run)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: desktop rules unreadable; the desktop socket is refused", "run_id", sess.Run, "err", err)
		return uiFail(ctx, http.StatusServiceUnavailable, reasonDesktopRulesUnreadable, desktopRulesUnreadableMsg)
	}
	rules := rfbfilter.Rules{PasteIn: d.PasteIn == types.TransferAllowed, CopyOut: d.CopyOut == types.TransferAllowed}
	filtered := rfbfilter.Wrap(body, rules, func(ev rfbfilter.Event) { s.auditUIClipboard(sess, ev) }, func(err error) {
		slog.WarnContext(ctx, "wardynd: ui relay closed a desktop socket it could not follow", "run_id", sess.Run, "app", sess.App, "err", err)
		if errors.Is(err, rfbfilter.ErrNotRFB) {
			s.auditUISession(sess, "ui.clipboard.transfer", "", "failure", map[string]any{"app": sess.App, "reason": "not_rfb"})
		}
	})
	if s.uiDesktopSink != nil {
		filtered.SetSink(s.uiDesktopSink(sess))
	}
	resp.Body = filtered
	return nil
}

// auditUIClipboard records one cut text by direction, size and hash. Never its content: the
// filter does not hand the content over.
func (s *Server) auditUIClipboard(sess uiSession, ev rfbfilter.Event) {
	outcome := "success"
	if !ev.Allowed {
		outcome = "denied"
	}
	s.auditUISession(sess, "ui.clipboard.transfer", string(ev.Direction), outcome,
		map[string]any{"app": sess.App, "direction": string(ev.Direction), "bytes": ev.Bytes, "sha256": ev.SHA256})
}
