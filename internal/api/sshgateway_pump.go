// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// The SSH shell's pump: attachPump's twin over an ssh.Channel. Split out of sshgateway_channels.go
// for scripts/check-file-size.sh.

// sshShellPump mirrors attach.go's attachPump over an ssh.Channel instead of
// a WebSocket: raw bytes need no framing (no resize control-message hack —
// window-change already arrives out-of-band as its own SSH request, fed in
// via resizeCh by handleSSHSessionChannel). castTee (nil when RecordingStore
// is unset or the run is unrecordable) receives a copy of every chunk of PTY
// output, exactly like the web-terminal attach.
//
// holder mirrors attachPump's: this channel's registry entry, whose canWrite
// says whether it may drive the PTY right now. A read-only observer's
// keystrokes are dropped here, server-side, and its window-change is kept as
// its own geometry (for its promotion) without ever reaching the shared tmux
// window: tmux sizes a shared window to its latest client, which is why an
// observer attaches with ignore-size and follows the writer's size instead. The
// channel is still READ from while read-only, because that read is how this
// pump learns the client hung up.
func (s *Server) sshShellPump(ctx context.Context, runID uuid.UUID, channel ssh.Channel, castTee io.Writer, resizeCh <-chan sshWindowChangeMsg, holder *attachHolder) string {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := &sshPumpState{
		s: s, ctx: ctx, cancel: cancel, runID: runID, channel: channel, castTee: castTee, holder: holder,
		// The holder's mux follows its current exec, so a promotion's re-attach
		// replaces the exec under these goroutines.
		sess:     &holder.mux,
		reasonC:  make(chan string, 2),
		resizeCh: resizeCh,
	}
	go p.output()
	go p.input()
	go p.resize()

	<-ctx.Done()
	select {
	case reason := <-p.reasonC:
		return reason
	default:
		return "context cancelled"
	}
}

// sshPumpState is one SSH shell's relay: the goroutines sshShellPump starts,
// and what they share.
type sshPumpState struct {
	s        *Server
	ctx      context.Context
	cancel   context.CancelFunc
	runID    uuid.UUID
	channel  ssh.Channel
	castTee  io.Writer
	holder   *attachHolder
	sess     runner.Session
	reasonC  chan string
	resizeCh <-chan sshWindowChangeMsg
}

func (p *sshPumpState) end(reason string) {
	select {
	case p.reasonC <- reason:
	default:
	}
	p.cancel()
}

// output is Session -> cast tee -> channel. The tee goes first, as the web pump's does (attach_pump.go), and
// nothing is sent once the pump has ended: the caller saves the recording after the pump returns, while this
// goroutine may still be reading, so a chunk sent after that is one the recording lacks. A chunk that was sent
// was teed before the pump ended, so it is in the recording when the recording is saved.
func (p *sshPumpState) output() {
	buf := make([]byte, attachReadBuf)
	for {
		n, rerr := p.sess.Read(buf)
		if n > 0 {
			if p.castTee != nil {
				_, _ = p.castTee.Write(buf[:n])
			}
			if p.ctx.Err() != nil {
				return
			}
			if _, werr := p.channel.Write(buf[:n]); werr != nil {
				p.end("client write failed")
				return
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				p.end("shell exited")
			} else {
				p.end("session read error")
			}
			return
		}
	}
}

// input is channel -> Session (keystrokes).
func (p *sshPumpState) input() {
	buf := make([]byte, attachReadBuf)
	for {
		n, rerr := p.channel.Read(buf)
		if n > 0 {
			// Held while a promotion replaces the exec: nothing is written until
			// it matches the role, and writeGated re-tests authority.
			if !p.holder.ready.wait(p.ctx) {
				return
			}
			// Same eviction-aware write path as the web pump — one gate for both
			// transports (attach_holder.go writeGated).
			if werr := p.holder.writeGated(p.sess, buf[:n]); werr != nil {
				p.end("session write failed")
				return
			}
		}
		if rerr != nil {
			p.end("client closed")
			return
		}
	}
}

// resize applies window-changes.
func (p *sshPumpState) resize() {
	for {
		select {
		case m, ok := <-p.resizeCh:
			if !ok {
				return
			}
			if !p.applyResize(uint16(m.Columns), uint16(m.Rows)) {
				return
			}
		case <-p.ctx.Done():
			return
		}
	}
}

// applyResize handles one window-change and reports false when the pump ended.
func (p *sshPumpState) applyResize(cols, rows uint16) bool {
	if cols == 0 || rows == 0 {
		return true
	}
	// Keep the registry's geometry LIVE — the pty-req value is stale the moment
	// the operator resizes their terminal. For an observer it is its OWN
	// geometry, which its promotion opens a writer exec at.
	p.holder.setSize(cols, rows)
	// canWrite, not `holder == nil`: an observer's size never reaches the shared
	// tmux window, and neither does a holder whose authority a take-over already
	// revoked. Gating on nil alone let an EVICTED ssh client keep resizing the
	// window under the new holder until its channel died — the web twin
	// (attach_pump.go's reader) gates on canWrite too.
	if !p.holder.canWrite() {
		return true
	}
	if !p.holder.ready.wait(p.ctx) {
		return false
	}
	if !p.holder.canWrite() {
		return true
	}
	if err := p.sess.Resize(p.ctx, cols, rows); err != nil {
		slog.WarnContext(p.ctx, "wardynd: ssh attach resize failed", "run_id", p.runID, "err", err)
		return true
	}
	p.s.fanoutWriterResize(p.ctx, p.runID, p.holder, cols, rows)
	p.holder.recordResize(cols, rows)
	return true
}
