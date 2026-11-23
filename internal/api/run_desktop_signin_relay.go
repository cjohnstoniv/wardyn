// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The sign-in display through the UI gateway: the reserved app "_signin",
// offered at enter only to the person who holds the run's open sign-in, and
// dialled as that person's uid to the noVNC socket in their store. The
// agent's own desktop app ("novnc") is untouched by any of this.

// desktopSigninDisplayPort stands in for a port in the gateway's session: the
// display is a unix socket, never a loopback port the agent could also open.
const desktopSigninDisplayPort = 1

// desktopSigninSocket is the person's noVNC (websockify) socket.
const desktopSigninSocket = runner.SigninStorePath + "/ws.sock"

// signinDisplayRecheck spaces an open sign-in display relay's re-reads of its sign-in (watchSigninDisplay).
const signinDisplayRecheck = 5 * time.Second

// withDesktopSigninApp adds the sign-in display to apps when principal holds
// the run's open sign-in. Any read failure leaves apps as they were.
func (s *Server) withDesktopSigninApp(ctx context.Context, run types.AgentRun, principal string, apps []types.UIApp) []types.UIApp {
	if s.holdsDesktopSignin(ctx, run, principal) != nil {
		return apps
	}
	return append(apps, types.UIApp{Name: desktopSigninApp, Port: desktopSigninDisplayPort, Path: "/vnc.html?autoconnect=1&resize=remote"})
}

// holdsDesktopSignin is nil when principal may open the run's sign-in display
// now: sign-ins are on, the run's entry rule admits them, and they hold its
// open sign-in, which no end has been asked of.
func (s *Server) holdsDesktopSignin(ctx context.Context, run types.AgentRun, principal string) error {
	st, on := s.desktopSigninStore()
	if !on {
		return errors.New("desktop sign-ins are off")
	}
	if !signinEntry(run, types.ActorHuman, principal) {
		return errors.New("not the run's creator")
	}
	si, err := st.OpenDesktopSignin(ctx, run.ID)
	if err != nil {
		return err
	}
	if si.Holder != principal || si.EndRequested != "" || !s.cfg.Now().Before(si.ExpiresAt) {
		return store.ErrNotFound
	}
	return nil
}

// watchSigninDisplay closes an open sign-in display relay, by closeConn, once principal no longer holds run's open
// sign-in: its end was asked for, a stop that failed writes `EndRequested` and the end is being retried, its window passed, or its row
// closed (holdsDesktopSignin answers store.ErrNotFound). A read that fails keeps the relay until a later read answers.
// It returns when ctx is done or once it has closed the relay.
func (s *Server) watchSigninDisplay(ctx context.Context, run types.AgentRun, principal string, every time.Duration, closeConn func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := s.holdsDesktopSignin(ctx, run, principal)
		if err == nil {
			continue
		}
		if !errors.Is(err, store.ErrNotFound) {
			slog.WarnContext(ctx, "wardynd: an open sign-in display could not re-read its sign-in; it stays open until a read answers", "run_id", run.ID, "err", err)
			continue
		}
		slog.InfoContext(ctx, "wardynd: an open sign-in display closed: its sign-in is ending or over", "run_id", run.ID)
		closeConn()
		return
	}
}

// desktopSigninDial is uiDial's connection for the sign-in display. The
// holder is re-checked on every connection, as uiDial re-checks the run.
func (s *Server) desktopSigninDial(ctx context.Context, run types.AgentRun, sess uiSession, release func()) (net.Conn, error) {
	if err := s.holdsDesktopSignin(ctx, run, sess.Principal); err != nil {
		release()
		return nil, uiFail(ctx, http.StatusForbidden, reasonDesktopSigninNotOpen, "no sign-in of yours is open on this run")
	}
	se, ok := s.cfg.Runner.(runner.SigninExecer)
	if !ok {
		release()
		return nil, uiFail(ctx, http.StatusConflict, reasonDesktopSigninRunnerUnsupported, desktopSigninRunnerMsg)
	}
	if uiDispatchPast(ctx) { // an issuer portal's request starts nothing at or after its dispatch time
		release()
		return nil, uiDispatchRefusal(ctx)
	}
	// The relay starts under the run's start lock, on the run as it stands there (startFresh).
	execSess, err := startFresh(s, uiStartCtx(ctx), run.ID, uiLiveRunWords, execStartBudget, func(ctx context.Context, run types.AgentRun) (*runner.ExecSession, error) {
		return se.ExecStreamAs(ctx, run.SandboxRef, runner.SigninPersonUser, runner.ExecSpec{
			Argv: []string{"socat", "-", "UNIX-CONNECT:" + desktopSigninSocket},
		})
	}, closeExecSession)
	if err != nil {
		release()
		return nil, uiExecFailure(ctx, reasonUIGatewayExecFailed, "start sign-in display relay", err)
	}
	keepCtx, stopKeepalive := context.WithCancel(context.WithoutCancel(ctx))
	go s.attachKeepalive(keepCtx, sess.Run)
	s.auditUISession(sess, "ui.open", desktopSigninSocket, "success", map[string]any{"app": sess.App})
	conn := &uiConn{execConn: newExecConn(execSess, run.SandboxRef+":"+desktopSigninApp), closeFn: func() {
		stopKeepalive()
		release()
		s.auditUISession(sess, "ui.close", desktopSigninSocket, "success", map[string]any{"app": sess.App})
	}}
	// The relay closes itself once its sign-in's end is asked for (watchSigninDisplay); closing it ends keepCtx.
	go s.watchSigninDisplay(keepCtx, run, sess.Principal, signinDisplayRecheck, func() { _ = conn.Close() })
	return s.uiGate(ctx, conn, execSess, sess, desktopSigninSocket), nil
}
