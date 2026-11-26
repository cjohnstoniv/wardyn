// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// LocalWindowEndLinkLost is the end reason of a local window whose runner said nothing, or said something this
// organisation does not know.
const LocalWindowEndLinkLost = "link_lost"

var (
	// ErrLocalWindowUnsupported: the runner's caps do not say it serves local windows, so nothing is sent.
	ErrLocalWindowUnsupported = errors.New("remote: this runner does not serve local windows")
	// ErrLocalWindowRefused: the runner refused the grant: its window nonce is unknown, expired, a terminal's,
	// or was asked for another run.
	ErrLocalWindowRefused = errors.New("remote: the runner refused the local window's nonce")
)

// LocalWindowRequest is one open: the window nonce the CLI got from the runner's local socket, the organisation's
// grant id and the window's title.
type LocalWindowRequest struct {
	Nonce   string
	GrantID uuid.UUID
	Title   string
}

// LocalWindowSupported is the runner's live caps: supported is Caps.LocalWindow, reported is false until the
// runner's caps have arrived.
func (s *Substrate) LocalWindowSupported() (supported, reported bool) {
	c, ok := s.Caps()
	if !ok {
		return false, false
	}
	return c.LocalWindow, true
}

// LocalWindow asks the runner to show ref's display to the CLI on its local socket that holds req.Nonce. The
// runner answers with a local_window stream that lives as long as the window; ctx bounds the open only.
func (s *Substrate) LocalWindow(ctx context.Context, ref string, req LocalWindowRequest) (*LocalWindowSession, error) {
	if supported, _ := s.LocalWindowSupported(); !supported {
		return nil, ErrLocalWindowUnsupported
	}
	if err := s.own(ref); err != nil {
		return nil, err
	}
	sess, err := s.pin()
	if err != nil {
		return nil, err
	}
	var res runnerwire.StreamResult
	args := runnerwire.LocalWindowArgs{Ref: ref, Nonce: req.Nonce, GrantID: req.GrantID, Title: req.Title}
	if err := sess.Call(ctx, runnerwire.MethodLocalWindow, args, &res); err != nil {
		var re *runnerwire.Error
		if errors.As(err, &re) && re.Code == runnerwire.CodeRefused {
			return nil, fmt.Errorf("%w: %w", ErrLocalWindowRefused, err)
		}
		return nil, err
	}
	st, err := sess.TakeStream(res.Stream)
	if err != nil {
		return nil, err
	}
	if st.Kind() != runnerwire.KindLocalWindow {
		_ = st.Close()
		return nil, runnerwire.Refuse("the runner answered the local window with a %q stream", st.Kind())
	}
	w := &LocalWindowSession{st: st, ended: make(chan struct{}), reason: LocalWindowEndLinkLost}
	go w.watch()
	return w, nil
}

// LocalWindowSession is the organisation's end of one local window: the local_window stream. Closing it resets
// the stream, which is how the organisation ends the window.
type LocalWindowSession struct {
	st    *runnerwire.Stream
	ended chan struct{}
	// reason, refusal and dropped are written before ended closes.
	reason  string
	refusal string
	dropped int
	once    sync.Once
}

// maxLocalWindowEnd bounds the one end line the runner writes.
const maxLocalWindowEnd = 512

// maxLocalWindowRefusal bounds the refusal kind kept from the end line: a fixed word, never stream content.
const maxLocalWindowRefusal = 64

// watch reads the runner's one end line, then closes the stream. A stream that ends with no line, or a line it
// cannot read, is a lost link.
func (w *LocalWindowSession) watch() {
	defer close(w.ended)
	line, err := bufio.NewReaderSize(w.st, maxLocalWindowEnd).ReadSlice('\n')
	if err == nil {
		var end runnerwire.LocalWindowEnd
		if runnerwire.Unmarshal(line, &end) == nil {
			w.reason = localWindowEndReason(end.Reason)
			if len(end.RefusalKind) <= maxLocalWindowRefusal {
				w.refusal = end.RefusalKind
			}
			w.dropped = max(end.CutTextDropped, 0)
		}
	}
	_ = w.st.Close()
}

// localWindowEndReason is the reason the organisation records for the runner's: its four as they are, anything
// it does not know as link_lost.
func localWindowEndReason(r string) string {
	switch r {
	case runnerwire.LocalWindowEndPerson, runnerwire.LocalWindowEndRunEnded, runnerwire.LocalWindowEndRefused, runnerwire.LocalWindowEndHelperFailed:
		return r
	}
	return LocalWindowEndLinkLost
}

// Ended is closed once the window has ended on the runner's side, or its stream is gone.
func (w *LocalWindowSession) Ended() <-chan struct{} { return w.ended }

// EndReason is why, after Ended.
func (w *LocalWindowSession) EndReason() string { return w.reason }

// RefusalKind is what the runner's display filter refused, in its fixed words, after Ended; "" for none.
func (w *LocalWindowSession) RefusalKind() string { return w.refusal }

// CutTextDropped is how many cut-text messages the runner's display filter dropped, after Ended.
func (w *LocalWindowSession) CutTextDropped() int { return w.dropped }

// Close ends the window by resetting the stream; the runner closes the CLI's socket. Idempotent.
func (w *LocalWindowSession) Close() error {
	w.once.Do(func() { _ = w.st.Close() })
	return nil
}
