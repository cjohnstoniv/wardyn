// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// attachPump runs the PTY relay and returns a short reason for the side that
// ended first. Four goroutines share one context, and when any ends it cancels
// it so the others unblock and the function returns:
//
//   - Session.Read -> client (and the recording tee);
//   - the socket READER, which only parses frames: control frames (resize; pong
//     and close are consumed by the library inside the same Read) are handled at
//     once, and keystroke frames go to a bounded queue. It starts BEFORE the
//     exec is opened, so a slow Runner.Attach never leaves a live peer's pong or
//     close unprocessed (the library needs a concurrent reader for pong);
//   - the DRAIN goroutine, the only one that writes keystrokes: it waits for the
//     holder to be ready, then writes through holder.writeGated, which re-tests
//     write authority per chunk;
//   - the RESIZE worker, which applies the writer's latest size off the reader.
//
// attach opens the exec (establishExec) once the reader is running. Its error
// ends the pump with the reason "attach failed".
//
// castTee, when non-nil, receives a copy of EVERY chunk of PTY output (after it
// is written to the client) for the session recording. It is the masked
// asciicast sink. A castTee write error never affects the live session — the
// recording is best-effort provenance, not part of the data path.
//
// holder is this client's registry entry; canWrite (attach_holder.go) is what
// says whether it may drive the PTY right now — false for an observer, true
// from the instant one is promoted in place, on the same object. Both
// client->server directions are dropped while it is false:
//
//   - binary frames (keystrokes), because two clients typing into one shared
//     tmux session is the exact interleaving this registry exists to prevent;
//   - resize frames (for the PTY), because an observer's size is not the
//     terminal's. tmux sizes a shared window to its LATEST client, so an
//     observer's window used to clamp the holder's terminal — that clamp is
//     precisely the symptom the UI's Redraw button was invented to mop up (see
//     refit in attach-terminal.tsx). Observers now attach with ignore-size and
//     follow the writer's size instead (attach_exec.go); the size an observer's
//     browser reports is kept as its own geometry, for its promotion.
//
// Dropping happens HERE, server-side, and never by asking the client to refrain
// from sending: the client is the one component we do not control. A read-only
// client still learns its mode from the attach-mode control frame the handler
// sent on open, so it can grey out its input rather than type into a void.
func (s *Server) attachPump(ctx context.Context, c *websocket.Conn, runID uuid.UUID, holder *attachHolder, castTee io.Writer, attach func() error) string {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := &attachPumpState{
		s: s, ctx: ctx, cancel: cancel, c: c, runID: runID, holder: holder, castTee: castTee,
		// The holder's mux follows its current exec, so a promotion's re-attach
		// replaces the exec under these goroutines.
		sess:    &holder.mux,
		reasonC: make(chan string, 6),
		queue:   newAttachInputQueue(),
		// Latest-wins, one slot: a burst of resizes never blocks the reader, and
		// only the most recent size matters (sshgateway_channels.go's
		// window-change model).
		resizeC: make(chan resizeMsg, 1),
	}
	go p.ping()
	go p.output()
	go p.read()
	go p.drain()
	go p.resizeWorker()
	// Open the exec now that the reader runs.
	go func() {
		if err := attach(); err != nil {
			p.end("attach failed")
		}
	}()

	<-ctx.Done()
	// Drain a reason if one is available; otherwise the parent ctx was cancelled.
	select {
	case reason := <-p.reasonC:
		return reason
	default:
		return "context cancelled"
	}
}

// attachPumpState is one attach socket's relay: the goroutines attachPump
// starts, and what they share.
type attachPumpState struct {
	s       *Server
	ctx     context.Context
	cancel  context.CancelFunc
	c       *websocket.Conn
	runID   uuid.UUID
	holder  *attachHolder
	sess    runner.Session
	castTee io.Writer
	reasonC chan string
	queue   *attachInputQueue
	resizeC chan resizeMsg
}

// end records why the pump ended and cancels it.
func (p *attachPumpState) end(reason string) {
	select {
	case p.reasonC <- reason:
	default:
	}
	p.cancel()
}

// ping is the liveness probe (attachPingInterval): a silent PTY produces no
// server->client Write for attachWriteTimeout to bound, so without this a dead
// peer holds its slot until the daemon restarts. Runs for every attach (holder
// or read-only observer) — cheap, and an observer's dead socket is worth
// reaping too.
func (p *attachPumpState) ping() {
	every := p.s.attachPingEvery()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-t.C:
			pctx, pcancel := context.WithTimeout(p.ctx, every)
			err := p.c.Ping(pctx)
			pcancel()
			if err != nil {
				p.end("ping timeout")
				return
			}
		}
	}
}

// output is Session.Read -> client (binary PTY frames). The blocking Read is
// unblocked by closing the session (the handler's deferred holder.mux.Close),
// so this goroutine exits.
func (p *attachPumpState) output() {
	buf := make([]byte, attachReadBuf)
	for {
		n, rerr := p.sess.Read(buf)
		if n > 0 {
			wctx, wcancel := context.WithTimeout(p.ctx, attachWriteTimeout)
			werr := p.c.Write(wctx, websocket.MessageBinary, buf[:n])
			wcancel()
			if werr != nil {
				p.end("client write failed")
				return
			}
			p.holder.lastOutput.Store(time.Now().UnixNano())
			// Tee the output into the session recording (best-effort: a
			// recording write error must not break the live terminal).
			if p.castTee != nil {
				_, _ = p.castTee.Write(buf[:n])
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				p.reasonC <- "shell exited"
				// A real close handshake BEFORE cancelling ctx (#1112): the
				// socket reader is blocked in c.Read(ctx) on the SAME ctx
				// cancelled just below, and coder/websocket arms that Read call
				// to forcibly tear down the raw connection (c.close(), no close
				// frame) the instant ctx is Done — cancelling first would race
				// that teardown and the client would see a bare EOF ("failed to
				// read frame header: EOF") instead of a clean detach. c.Close
				// writes the close frame, then blocks briefly for the peer's
				// echo before tearing the connection down itself, so a normal
				// shell exit reaches the client as an actual
				// StatusNormalClosure. Best-effort: if the peer is already
				// gone, this is a no-op and cancel() below still reaps the
				// goroutines.
				_ = p.c.Close(websocket.StatusNormalClosure, "shell exited")
			} else {
				p.reasonC <- "session read error"
			}
			p.cancel()
			return
		}
	}
}

// read is the socket reader. It never calls Session.Write, Session.Resize or
// anything else that can block on the sandbox.
func (p *attachPumpState) read() {
	for {
		typ, data, rerr := p.c.Read(p.ctx)
		if rerr != nil {
			if websocket.CloseStatus(rerr) != -1 {
				p.end("client closed")
			} else {
				p.end("client read error")
			}
			return
		}
		switch typ {
		case websocket.MessageText:
			p.control(data)
		case websocket.MessageBinary:
			// Raw PTY input (keystrokes). A read-only observer's is dropped
			// here; a writer's is held for the drain goroutine, which re-tests
			// authority when it dequeues (a take-over decided while it waited
			// discards it). A full queue ends the connection with 1009: the
			// reader never blocks, and no chunk is dropped without saying so.
			if !p.holder.canWrite() {
				continue
			}
			if !p.queue.push(data) {
				_ = p.c.Close(websocket.StatusMessageTooBig, "input queue full")
				p.end("input queue full")
				return
			}
		}
	}
}

// control handles one control frame: only resize is understood. An
// unparseable or unknown control message is ignored (it is never injected into
// the PTY, so it cannot smuggle keystrokes).
func (p *attachPumpState) control(data []byte) {
	var msg resizeMsg
	if json.Unmarshal(data, &msg) != nil || msg.Type != "resize" || msg.Cols == 0 || msg.Rows == 0 {
		return
	}
	// Keep the registry's geometry LIVE: the handshake ?cols=&rows= is stale the
	// moment the operator drags their window, and GET /runs/{id}/attach-holder
	// reporting a stale size with confidence is worse than reporting none. For an
	// observer this is its OWN geometry, which its promotion opens a writer exec
	// at.
	p.holder.setSize(msg.Cols, msg.Rows)
	if !p.holder.canWrite() {
		return // an observer's size never reaches the shared window
	}
	select {
	case p.resizeC <- msg:
	default:
		select {
		case <-p.resizeC:
		default:
		}
		select {
		case p.resizeC <- msg:
		default:
		}
	}
}

// drain is keystrokes -> Session.Write, only once the exec matches the role.
// writeGated re-tests write authority before EVERY chunk, so a 1 MiB paste in
// flight when an eviction lands stops there instead of finishing into the new
// holder's session, and input held while attaching is discarded when the holder
// was evicted or the attach failed (ready never comes).
func (p *attachPumpState) drain() {
	for {
		// Ready before the pop, so held input stays in the bounded queue (and
		// counts against it) for as long as the exec is being opened; ready
		// again after it, because a promotion can send the holder back to
		// attaching while this goroutine waits for a chunk.
		if !p.holder.ready.wait(p.ctx) {
			return
		}
		data, ok := p.queue.pop(p.ctx)
		if !ok || !p.holder.ready.wait(p.ctx) {
			return
		}
		if werr := p.holder.writeGated(p.sess, data); werr != nil {
			p.end("session write failed")
			return
		}
	}
}

// resizeWorker applies the writer's latest size once the exec is ready and then
// carries it to every observer.
func (p *attachPumpState) resizeWorker() {
	for {
		select {
		case m := <-p.resizeC:
			if !p.holder.ready.wait(p.ctx) {
				return
			}
			if !p.holder.canWrite() {
				continue
			}
			if err := p.sess.Resize(p.ctx, m.Cols, m.Rows); err != nil {
				slog.WarnContext(p.ctx, "wardynd: attach resize failed", "run_id", p.runID, "err", err)
				continue
			}
			p.s.fanoutWriterResize(p.ctx, p.runID, p.holder, m.Cols, m.Rows)
		case <-p.ctx.Done():
			return
		}
	}
}
