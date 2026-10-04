// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// A holder's exec: opened AFTER the registry has decided who writes, replaced
// when an observer is promoted, and the size every observer follows.
//
// Order is the point. The exec used to be created first and the registry
// consulted second, so an observer's tmux client existed (and, under tmux's
// `window-size latest`, resized the shared window) before anyone knew it was an
// observer. Now registration comes first, the role picks the attach options (a
// writer at its own size, an observer with tmux's ignore-size flag and the
// writer's size), and a promotion swaps the observer's exec for a writer's.
package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// attachPrepareTimeout bounds one Runner.Attach (the exec create and hijack):
// a sandbox that never answers must not hold a registered writer slot, with the
// socket reading nothing, forever.
const attachPrepareTimeout = 30 * time.Second

// attachPrepareEvery is attachPrepareTimeout unless THIS server was built with
// an override (Server.attachPrepare, tests only).
func (s *Server) attachPrepareEvery() time.Duration {
	if s.attachPrepare > 0 {
		return s.attachPrepare
	}
	return attachPrepareTimeout
}

var (
	// errAttachCancelled: the holder was evicted, released, or its connection
	// ended while its exec was being opened.
	errAttachCancelled = errors.New("attach cancelled")
	// errAttachTimedOut: Runner.Attach did not return within attachPrepareTimeout.
	errAttachTimedOut = errors.New("attach timed out")
	// errAttachUncovered: a promotion's new exec was refused because the run's
	// masking corpus can no longer be proven whole (mask_manifest.go).
	errAttachUncovered = errors.New("run masking state unavailable")
	errAttachNotReady  = errors.New("attach not ready")
)

// attachReady is a holder's attach state: `attaching` (the zero value) until its
// exec matches its role, `ready` after. Held input waits on it.
type attachReady struct {
	mu    sync.Mutex
	ready bool
	ch    chan struct{} // closed while ready
}

// setReadyIf marks the holder ready when ok, evaluated under the lock so that a
// promotion's setAttaching cannot slip between the check and the store.
func (r *attachReady) setReadyIf(ok func() bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !ok() {
		return false
	}
	if !r.ready {
		r.ready = true
		if r.ch == nil {
			r.ch = make(chan struct{})
		}
		close(r.ch)
	}
	return true
}

// setAttaching sends a ready holder back to attaching (a promotion: its exec is
// about to be replaced). A holder that is not ready stays as it is.
func (r *attachReady) setAttaching() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ready {
		r.ready = false
		r.ch = make(chan struct{})
	}
}

// isReady reports whether the holder is ready, without waiting.
func (r *attachReady) isReady() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ready
}

// wait blocks until the holder is ready, and reports false when ctx ended first.
func (r *attachReady) wait(ctx context.Context) bool {
	r.mu.Lock()
	if r.ready {
		r.mu.Unlock()
		return true
	}
	if r.ch == nil {
		r.ch = make(chan struct{})
	}
	ch := r.ch
	r.mu.Unlock()
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return false
	}
}

// muxSession is the runner.Session a holder's pumps read and write. It forwards
// to the holder's CURRENT exec, so a promotion can replace the exec under a
// pump that never learns of it: Read follows the swap instead of reporting the
// replaced exec's end as the shell exiting. Before the first exec exists Read
// blocks, and Write and Resize have nothing to reach.
type muxSession struct {
	mu     sync.Mutex
	cur    runner.Session
	gen    int
	wake   chan struct{} // closed on every install and on close
	closed bool
	cols   uint16 // the size last applied to cur
	rows   uint16
}

func (m *muxSession) snapshot() (cur runner.Session, gen int, wake chan struct{}, closed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.wake == nil {
		m.wake = make(chan struct{})
	}
	return m.cur, m.gen, m.wake, m.closed
}

// install makes sess the current exec and returns the one it replaced (nil for
// the first). It reports false, installing nothing, once the mux is closed:
// the caller owns closing sess then.
func (m *muxSession) install(sess runner.Session, cols, rows uint16) (old runner.Session, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, false
	}
	old, m.cur = m.cur, sess
	m.gen++
	m.cols, m.rows = cols, rows
	if m.wake != nil {
		close(m.wake)
	}
	m.wake = make(chan struct{})
	return old, true
}

// lastSize is the size last applied to the current exec (0,0 when unknown).
func (m *muxSession) lastSize() (cols, rows uint16) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cols, m.rows
}

func (m *muxSession) Read(p []byte) (int, error) {
	for {
		cur, gen, wake, closed := m.snapshot()
		if closed {
			return 0, io.EOF
		}
		if cur == nil {
			<-wake
			continue
		}
		n, err := cur.Read(p)
		if err != nil && n == 0 {
			m.mu.Lock()
			swapped := m.gen != gen
			m.mu.Unlock()
			if swapped {
				continue // the exec was replaced; read the new one
			}
		}
		return n, err
	}
}

func (m *muxSession) Write(p []byte) (int, error) {
	cur, _, _, closed := m.snapshot()
	if closed || cur == nil {
		return 0, errAttachNotReady
	}
	return cur.Write(p)
}

func (m *muxSession) Resize(ctx context.Context, cols, rows uint16) error {
	cur, _, _, closed := m.snapshot()
	if closed || cur == nil {
		return nil
	}
	if err := cur.Resize(ctx, cols, rows); err != nil {
		return err
	}
	if cols > 0 && rows > 0 {
		m.mu.Lock()
		m.cols, m.rows = cols, rows
		m.mu.Unlock()
	}
	return nil
}

// Close ends the mux and the exec it holds, and unblocks a Read waiting for one.
func (m *muxSession) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	cur := m.cur
	if m.wake != nil {
		close(m.wake)
	}
	m.mu.Unlock()
	if cur != nil {
		return cur.Close()
	}
	return nil
}

// attachOptionsFor is the Runner.Attach options for h in its CURRENT role. A
// writer opens at its own geometry. An observer opens at the writer's live
// registry size, with the Observer flag so tmux leaves it out of window sizing;
// on a tmux without that flag the matching size is what keeps it from clamping
// the writer.
func (s *Server) attachOptionsFor(runID uuid.UUID, h *attachHolder, writer bool) runner.AttachOptions {
	if writer {
		cols, rows := h.size()
		if cols == 0 || rows == 0 {
			cols, rows = h.mux.lastSize()
		}
		return runner.AttachOptions{Cols: cols, Rows: rows}
	}
	opts := runner.AttachOptions{Observer: true}
	if w := s.attachHolderFor(runID); w != nil && w != h {
		opts.Cols, opts.Rows = w.size()
	}
	return opts
}

// establishExec makes h's exec match its role: it opens the first one, and
// replaces an observer's by a writer's once h has been promoted. Calls are
// serialised, so a promotion that lands during the first attach waits for it and
// then re-attaches.
//
// Authority is decided around every Runner.Attach, never before it alone: a
// session that comes back after the holder was evicted, released, or promoted
// to the other role is closed here by its own identity (the object Attach
// returned), and the loop opens again only if the role changed. A replacement
// exec also re-proves the run's masking corpus whole.
func (s *Server) establishExec(ctx context.Context, runID uuid.UUID, h *attachHolder, sandboxRef string) error {
	h.openMu.Lock()
	defer h.openMu.Unlock()
	for {
		if ctx.Err() != nil || h.evicted.Load() {
			return errAttachCancelled
		}
		writer := h.writable.Load()
		if h.execOpened && h.execWriter == writer {
			h.ready.setReadyIf(func() bool { return h.writable.Load() == writer })
			return nil
		}
		if h.execOpened && !s.maskCovered(ctx, runID) {
			return errAttachUncovered
		}
		opts := s.attachOptionsFor(runID, h, writer)

		actx, acancel := context.WithCancel(ctx)
		timer := time.AfterFunc(s.attachPrepareEvery(), acancel)
		sess, err := s.cfg.Runner.Attach(actx, sandboxRef, opts)
		inTime := timer.Stop()
		if err != nil {
			acancel()
			if !inTime {
				err = errAttachTimedOut
			}
			return err
		}
		switch {
		case !inTime:
			_ = sess.Close()
			acancel()
			return errAttachTimedOut
		case ctx.Err() != nil || h.evicted.Load():
			_ = sess.Close()
			acancel()
			return errAttachCancelled
		case h.writable.Load() != writer:
			_ = sess.Close() // promoted while attaching: open again in the new role
			acancel()
			continue
		}
		old, ok := h.mux.install(sess, opts.Cols, opts.Rows)
		if !ok {
			_ = sess.Close()
			acancel()
			return errAttachCancelled
		}
		if old != nil {
			_ = old.Close()
		}
		if h.execCancel != nil {
			h.execCancel()
		}
		h.execCancel, h.execOpened, h.execWriter = acancel, true, writer
		if !writer {
			// An observer opened at the writer's size of a moment ago; re-apply the
			// live one so a resize that landed meanwhile is not missed.
			if w := s.attachHolderFor(runID); w != nil && w != h {
				if cols, rows := w.size(); cols > 0 && rows > 0 && (cols != opts.Cols || rows != opts.Rows) {
					if err := h.mux.Resize(ctx, cols, rows); err != nil {
						slog.WarnContext(ctx, "wardynd: observer attach resize failed", "run_id", runID, "err", err)
					}
				}
			}
		}
		h.ready.setReadyIf(func() bool { return h.writable.Load() == writer })
		if writer {
			// A newly installed writer sets the window: carry its size to the
			// observers still queued, as a window-change would. A promoted SSH
			// client sends none, so this is the only place they hear of it.
			s.fanoutWriterResize(ctx, runID, h, opts.Cols, opts.Rows)
		}
		return nil
	}
}

// attachObservers is a snapshot of runID's queued observers, oldest first.
func (s *Server) attachObservers(runID uuid.UUID) []*attachHolder {
	reg := s.attachRegistry()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if ra := reg.attaches[runID]; ra != nil {
		return append([]*attachHolder(nil), ra.observers...)
	}
	return nil
}

// fanoutWriterResize carries an accepted writer resize to every observer: each
// observer's PTY follows the writer's size (safe under ignore-size, which keeps
// it out of window sizing, and what keeps a tmux without the flag from clamping
// the writer), and each observer's socket is re-sent the attach-mode frame so
// its browser grid follows too. A client that was displaced in the meantime
// fans nothing out.
func (s *Server) fanoutWriterResize(ctx context.Context, runID uuid.UUID, writer *attachHolder, cols, rows uint16) {
	if !writer.canWrite() {
		return
	}
	for _, o := range s.attachObservers(runID) {
		if o == writer {
			continue
		}
		if err := o.mux.Resize(ctx, cols, rows); err != nil {
			slog.WarnContext(ctx, "wardynd: observer resize failed", "run_id", runID, "err", err)
		}
		if o.notify != nil {
			go o.notify(true, writer)
		}
	}
}
