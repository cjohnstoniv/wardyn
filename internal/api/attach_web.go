// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// webAttach is one attach WebSocket's handler state: what the holder's
// callbacks (notify, displace) and the exec opener share.
//
// modeMu orders THIS socket's attach-mode frames. The library permits
// concurrent writes, but not concurrent truth: the opening frame and a
// promotion frame from another client's release goroutine can cross, and a
// client whose LAST frame says read_only:true while the registry has already
// made it the writer greys out a terminal it is allowed to type into. Both
// sends take this mutex and read the mode live inside it, so whichever lands
// second is the one that is still true.
type webAttach struct {
	s             *Server
	c             *websocket.Conn
	run           types.AgentRun
	principalType types.ActorType
	principal     string
	finishCtx     context.Context // daemon-lifetime: the audit rows outlive the request
	pumpCtx       context.Context // ends the whole connection
	cancel        context.CancelFunc
	holder        *attachHolder
	modeMu        sync.Mutex
	failed        atomic.Bool // the exec could not be opened or replaced
}

// fail ends a connection whose exec could not be opened or replaced: the
// outcome is audited and the socket closed with a status the console can tell
// from a dropped link.
func (wa *webAttach) fail(err error, promotion bool) {
	wa.failed.Store(true)
	data := map[string]any{"state": "attaching", "error": err.Error()}
	if promotion {
		data["promotion"] = true
	}
	wa.audit("failure", data)
	// The close frame first, then the cancel: cancelling first would have the
	// library tear the connection down without one.
	_ = wa.c.Close(websocket.StatusInternalError, "attach failed")
	wa.cancel()
}

func (wa *webAttach) audit(outcome string, data map[string]any) {
	id := wa.run.ID
	wa.s.recordStreamAudit(wa.finishCtx, wa.run.SandboxRef, wa.s.auditEvent(&id, wa.principalType, wa.principal, "session.attach",
		id.String(), outcome, mustJSON(data)))
}

// open opens the exec and writes the second session.attach row, the outcome,
// when the attach settles.
func (wa *webAttach) open() error {
	err := wa.s.establishExec(wa.pumpCtx, wa.run.ID, wa.holder, wa.run.SandboxRef)
	if errors.Is(err, errAttachCancelled) {
		// The connection ended some other way (a client close, a take-over)
		// while the exec was opening; the outcome is still recorded.
		wa.audit("failure", map[string]any{"state": "attaching", "error": err.Error()})
		return err
	}
	if err != nil {
		wa.fail(err, false)
		return err
	}
	wa.audit("success", map[string]any{
		"sandbox_ref": wa.run.SandboxRef, "state": "ready", "read_only": !wa.holder.writable.Load(),
	})
	return nil
}

// notify is attachHolder.notify for the WebSocket lane.
//
// Promotion (readOnly=false): this socket may now type, but its exec is still
// the observer's, so it is re-attached as a writer at its own size first, on
// this same socket, and only then told. attach-terminal.tsx already implements
// the read_only true->false transition (it force-refits and focuses the
// terminal). A writer resize (readOnly=true): the observer's grid follows the
// writer's, so it is re-sent the frame.
func (wa *webAttach) notify(readOnly bool, h *attachHolder) {
	if readOnly {
		wa.modeMu.Lock()
		defer wa.modeMu.Unlock()
		if !wa.holder.canWrite() { // promoted meanwhile; its own frame follows
			_ = writeAttachMode(wa.pumpCtx, wa.c, true, h)
		}
		return
	}
	if err := wa.s.establishExec(wa.pumpCtx, wa.run.ID, wa.holder, wa.run.SandboxRef); err != nil {
		if errors.Is(err, errAttachCancelled) {
			return // evicted or gone meanwhile: its socket is closing by other means
		}
		slog.WarnContext(wa.pumpCtx, "wardynd: promoted attach could not open a writer exec", "run_id", wa.run.ID, "err", err)
		wa.fail(err, true)
		return
	}
	wa.modeMu.Lock()
	defer wa.modeMu.Unlock()
	_ = writeAttachMode(wa.pumpCtx, wa.c, false, h)
}

// displace ends this socket with a reason the DISPLACED CLIENT can read.
//
// Close, do NOT cancel: c.Read honours pumpCtx by tearing the connection down
// abruptly, and an abruptly-killed socket delivers no close frame — the
// displaced UI would see a bare drop and take its bounded-reconnect path,
// landing straight back on top of the new holder. c.Close writes the 1008 +
// reason frame FIRST, and that frame is the entire signal the UI distinguishes
// "taken over" by.
//
// On its own goroutine because Close performs the close HANDSHAKE: it waits for
// the peer's echo (5s cap) and for the pump goroutines to exit (15s cap), and
// the take-over HTTP request must not block on the displaced client's teardown.
// Those caps are also the backstop — the socket dies even if the peer never
// answers.
func (wa *webAttach) displace(reason string) {
	go func() { _ = wa.c.Close(websocket.StatusPolicyViolation, reason) }()
}
