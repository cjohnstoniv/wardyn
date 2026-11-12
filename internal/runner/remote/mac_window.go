// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"bufio"
	"context"
	"errors"
	"sync"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// MacWindowEndLinkLost is the end reason of a window whose runner said nothing, said helper_lost, or said
// something this organisation does not know.
const MacWindowEndLinkLost = "link_lost"

// The runner's window refusals the organisation words itself. Any other refusal stays the runner's error.
var (
	// ErrMacWindowNotAtMac: the person is not at the Mac's unlocked screen.
	ErrMacWindowNotAtMac = errors.New("remote: the person is not at the Mac's unlocked screen")
	// ErrMacWindowAlreadyOpen: the runner already holds a window for the run.
	ErrMacWindowAlreadyOpen = errors.New("remote: the runner already holds a window for this run")
	// ErrMacWindowLockUnknown: the Mac can't tell whether its screen is locked, so the window stays closed.
	ErrMacWindowLockUnknown = errors.New("remote: whether the Mac's screen is locked can't be told")
)

// MacWindowRequest is one open: the organisation's grant id and the window's title.
type MacWindowRequest struct {
	GrantID string
	Title   string
}

// MacWindowSupported is the runner's live caps: supported is MacVMCaps.Window, reported is false until the
// runner's caps have arrived.
func (s *Substrate) MacWindowSupported() (supported, reported bool) {
	c, ok := s.Caps()
	if !ok {
		return false, false
	}
	return c.MacOSVM != nil && c.MacOSVM.Window, true
}

// MacWindow asks the runner to put an attended window on the live VM of ref. The runner answers with a
// mac_window stream that lives as long as the window; ctx bounds the open only.
func (s *Substrate) MacWindow(ctx context.Context, ref string, req MacWindowRequest) (*MacWindowSession, error) {
	if err := s.own(ref); err != nil {
		return nil, err
	}
	sess, err := s.pin()
	if err != nil {
		return nil, err
	}
	var res runnerwire.StreamResult
	if err := sess.Call(ctx, runnerwire.MethodMacWindow, runnerwire.MacWindowArgs{Ref: ref, GrantID: req.GrantID, Title: req.Title}, &res); err != nil {
		return nil, macWindowRefusal(err)
	}
	st, err := sess.TakeStream(res.Stream)
	if err != nil {
		return nil, err
	}
	if st.Kind() != runnerwire.KindMacWindow {
		_ = st.Close()
		return nil, runnerwire.Refuse("the runner answered the window with a %q stream", st.Kind())
	}
	w := &MacWindowSession{st: st, ended: make(chan struct{}), reason: MacWindowEndLinkLost}
	go w.watch()
	return w, nil
}

// macWindowRefusal maps the runner's refusals that the organisation words itself. The runner's reply carries
// the sentence, not the helper's code: the sentences are runnerwire's.
func macWindowRefusal(err error) error {
	var re *runnerwire.Error
	if !errors.As(err, &re) || re.Code != runnerwire.CodeRefused {
		return err
	}
	switch re.Message {
	case runnerwire.MacWindowOpenSentence:
		return ErrMacWindowAlreadyOpen
	case runnerwire.MacWindowNotAtMacSentence, runnerwire.MacWindowUnreadableSentence:
		return ErrMacWindowNotAtMac
	case runnerwire.MacWindowLockUnknownSentence:
		return ErrMacWindowLockUnknown
	}
	return err
}

// MacWindowSession is the organisation's end of one open window: the mac_window stream. Closing it resets
// the stream, which is how the organisation ends the window.
type MacWindowSession struct {
	st     *runnerwire.Stream
	ended  chan struct{}
	reason string // written before ended closes
	once   sync.Once
}

// maxMacWindowEnd bounds the one end line the runner writes.
const maxMacWindowEnd = 512

// watch reads the runner's one end line, then closes the stream: the organisation holds no stream for a
// window that is over. A stream that ends with no line, or a line it cannot read, is a lost link.
func (w *MacWindowSession) watch() {
	defer close(w.ended)
	line, err := bufio.NewReaderSize(w.st, maxMacWindowEnd).ReadSlice('\n')
	if err == nil {
		var end runnerwire.MacWindowEnd
		if runnerwire.Unmarshal(line, &end) == nil {
			w.reason = macWindowEndReason(end.Reason)
		}
	}
	_ = w.st.Close()
}

// macWindowEndReason is the reason the organisation records for the runner's: its own four as they are,
// helper_lost and anything it does not know as link_lost.
func macWindowEndReason(r string) string {
	switch r {
	case runner.MacWindowEndPerson, runner.MacWindowEndHostLocked, runner.MacWindowEndRunEnded, runner.MacWindowEndHelperFailed:
		return r
	}
	return MacWindowEndLinkLost
}

// Ended is closed once the window has ended on the runner's side, or its stream is gone.
func (w *MacWindowSession) Ended() <-chan struct{} { return w.ended }

// EndReason is why, after Ended.
func (w *MacWindowSession) EndReason() string { return w.reason }

// Close ends the window by resetting the stream; the runner tears the view down. Idempotent.
func (w *MacWindowSession) Close() error {
	w.once.Do(func() { _ = w.st.Close() })
	return nil
}
