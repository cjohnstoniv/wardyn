// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

// Attended window: the org's mac_window call puts a keyboard-and-pointer window on a macOS VM run this
// runner holds. The runner opens a mac_window stream back as the call's reply and holds the helper's
// connection for the life of the window; either end ends the other. One window per run: a second open
// is refused whatever its grant, with the sentence the organisation and the helper use.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sync"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// macWindowSession is one open window: the helper's connection and the org's stream. It is in the
// server's registry from the moment its run's slot is taken, before either exists.
type macWindowSession struct {
	s     *Server
	ref   string
	peer  *runnerwire.Peer
	grant string
	once  sync.Once

	mu   sync.Mutex
	win  runner.MacWindow // guarded by mu, with st and dead
	st   *runnerwire.Stream
	dead bool // end ran: a window or stream armed after it is closed at once
}

// macWindowGrant is what a grant id may be: the helper's rule, held here too so a bad id is refused
// before the run's slot is taken.
var macWindowGrant = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// macWindowRefusal turns a MacWindower's refusal into the sentence the org shows; an error it does not
// know is the runner's own failure.
func macWindowRefusal(err error) error {
	var helper *runner.MacWindowRefusal
	switch {
	case errors.Is(err, runner.ErrMacWindowOpen):
		return runnerwire.Refuse("%s", runnerwire.MacWindowOpenSentence)
	case errors.Is(err, runner.ErrMacWindowHelperOld):
		return runnerwire.Refuse("%s", runnerwire.MacWindowHelperOldSentence)
	case errors.Is(err, runner.ErrMacWindowUnsupported):
		return runnerwire.Refuse("%s", runnerwire.MacWindowNotMacSentence)
	case errors.As(err, &helper):
		return runnerwire.Refuse("%s", helper.Sentence)
	}
	return err
}

// macWindow is MethodMacWindow. The run's slot is taken before the helper is asked, so two grants for
// one run never both reach it.
func (s *Server) macWindow(ctx context.Context, peer *runnerwire.Peer, id uint32, raw json.RawMessage) (any, error) {
	args, err := decode[runnerwire.MacWindowArgs](raw)
	if err != nil {
		return nil, err
	}
	if !macWindowGrant.MatchString(args.GrantID) {
		return nil, runnerwire.Refuse("a window needs a grant id of letters, digits and . _ : - (at most 128)")
	}
	ref, err := s.local(args.Ref)
	if err != nil {
		return nil, err
	}
	if err := s.held(ref); err != nil {
		return nil, err
	}
	mw, ok := s.sub.(runner.MacWindower)
	if !ok {
		return nil, macWindowRefusal(runner.ErrMacWindowUnsupported)
	}
	m := &macWindowSession{s: s, ref: ref, peer: peer, grant: args.GrantID}
	s.mu.Lock()
	old := s.windows[ref]
	s.mu.Unlock()
	if old != nil && old.orgEnded() {
		// The org ended that window's stream before it asked again (a peer handles its frames in order),
		// and the window's own goroutine may not have torn it down yet: finish the end here, so the slot
		// is free before this open looks at it. end runs once; a concurrent end returns only when done.
		old.end("", false)
	}
	s.mu.Lock()
	if s.windows == nil {
		s.windows = map[string]*macWindowSession{}
	}
	if s.windows[ref] != nil {
		s.mu.Unlock()
		return nil, macWindowRefusal(runner.ErrMacWindowOpen)
	}
	s.windows[ref] = m
	s.mu.Unlock()
	life := s.life(peer)
	win, err := mw.OpenWindow(ctx, ref, runner.MacWindowRequest{GrantID: args.GrantID, Title: args.Title})
	if err != nil {
		m.release()
		return nil, macWindowRefusal(err)
	}
	st, err := peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindMacWindow, Target: ref, Call: id})
	if err != nil {
		_ = win.Close()
		m.release()
		return nil, err
	}
	m.mu.Lock()
	m.win, m.st = win, st
	dead := m.dead
	m.mu.Unlock()
	if dead || life.Err() != nil || !peer.Online() {
		// The link went while the helper was being asked. Offline may already have ended this session
		// (end runs once, before there was a window to close), so close what this call holds directly.
		_ = win.Close()
		m.release()
		_ = st.Close()
		return nil, runner.ErrRunnerOffline
	}
	go m.run()
	return runnerwire.StreamResult{Stream: st.ID()}, nil
}

// orgEnded reports whether the org's end of the window's stream is gone (reset, or both halves closed).
func (m *macWindowSession) orgEnded() bool {
	m.mu.Lock()
	st := m.st
	m.mu.Unlock()
	if st == nil {
		return false
	}
	select {
	case <-st.Done():
		return true
	default:
		return false
	}
}

// release gives up the run's slot, if it is still this window's.
func (m *macWindowSession) release() {
	m.s.mu.Lock()
	if m.s.windows[m.ref] == m {
		delete(m.s.windows, m.ref)
	}
	m.s.mu.Unlock()
}

// run holds the window until the first of: the helper's connection ends (the window is over, and the org
// is told why on the stream) or the org's end of the stream does (the helper's connection is closed,
// which closes the window). The org never writes the stream; anything it sends is dropped.
func (m *macWindowSession) run() {
	orgGone := make(chan struct{})
	go func() {
		defer close(orgGone)
		_, _ = io.Copy(io.Discard, m.st)
	}()
	select {
	case <-m.win.Ended():
		m.end(m.win.Reason(), true)
	case <-orgGone:
		m.end("", false)
	}
}

// end ends the window once: the helper's connection first, then the run's slot (a reopen must not reach a
// helper that still holds the old window, nor meet a slot that is still taken once the org hears of this),
// then the stream. tell writes the reason to the org and
// half-closes the stream; otherwise the stream is closed (the org already ended it, or its link is gone).
func (m *macWindowSession) end(reason string, tell bool) {
	m.once.Do(func() {
		m.mu.Lock()
		m.dead = true
		win, st := m.win, m.st
		m.mu.Unlock()
		if win != nil {
			_ = win.Close()
		}
		m.release()
		if st == nil {
			return
		}
		if !tell {
			_ = st.Close()
			return
		}
		if reason == "" {
			reason = runner.MacWindowEndHelperLost
		}
		b, _ := json.Marshal(runnerwire.MacWindowEnd{Reason: reason})
		if _, err := st.Write(append(b, '\n')); err != nil {
			_ = st.Reset(runnerwire.ResetInternal)
			return
		}
		_ = st.CloseWrite()
	})
}
