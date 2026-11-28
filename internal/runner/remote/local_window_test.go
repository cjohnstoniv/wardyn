// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// windowRunner is a runner end that answers local_window: it refuses with refuse when set, else opens a
// local_window stream and writes line on it (none when line is ""), half-closes it, then reads it until the
// organisation closes it and sends how that read ended on readEnd.
type windowRunner struct {
	mu      sync.Mutex
	peer    *runnerwire.Peer
	args    []runnerwire.LocalWindowArgs
	refuse  error
	line    string
	readEnd chan error
}

func (l *windowRunner) handle(ctx context.Context, id uint32, method string, raw json.RawMessage) (any, error) {
	if method != runnerwire.MethodLocalWindow {
		return nil, runnerwire.Refuse("unknown runner method")
	}
	var a runnerwire.LocalWindowArgs
	_ = json.Unmarshal(raw, &a)
	l.mu.Lock()
	l.args = append(l.args, a)
	refuse, line := l.refuse, l.line
	l.mu.Unlock()
	if refuse != nil {
		return nil, refuse
	}
	st, err := l.peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindLocalWindow, Call: id})
	if err != nil {
		return nil, err
	}
	go func() {
		if line != "" {
			_, _ = st.Write([]byte(line + "\n"))
		}
		_ = st.CloseWrite()
		_, err := io.Copy(io.Discard, st)
		l.readEnd <- err
	}()
	return runnerwire.StreamResult{Stream: st.ID()}, nil
}

func (l *windowRunner) calls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.args)
}

// windowRig connects an org Substrate to a windowRunner whose caps carry (or lack) LocalWindow.
func windowRig(t *testing.T, capWindow bool) (*remote.Substrate, *windowRunner, string) {
	t.Helper()
	var link remote.Link
	sub := remote.New(rid, &link, remote.Options{})
	org, run := runnerwire.Loopback()
	wr := &windowRunner{readEnd: make(chan error, 1)}
	wr.peer = runnerwire.NewPeer(run, runnerwire.PeerConfig{OnCall: wr.handle})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	peer := link.Attach(ctx, org)
	go func() { _ = wr.peer.Run(ctx) }()
	t.Cleanup(func() { peer.Close(); wr.peer.Close() })
	caps, _ := json.Marshal(runnerwire.Caps{Version: "test", LocalWindow: capWindow})
	sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: caps})
	return sub, wr, "runner:" + rid + "/sandbox"
}

func waitEnded(t *testing.T, w *remote.LocalWindowSession) {
	t.Helper()
	select {
	case <-w.Ended():
	case <-time.After(3 * time.Second):
		t.Fatal("the window did not end")
	}
}

// The org reads the runner's one end line: the runner's four reasons as they are, the filter's refusal kind and
// the dropped cut-text count; anything else, no line, or a line past the bound reads as the link lost. The call
// carries the nonce, the grant and the title, and is sent only to a runner whose caps serve local windows. A
// runner's refusal (the nonce) is told apart from a failed call.
func TestLocalWindowSessionEndLine(t *testing.T) {
	long := `{"reason":"person","refusal_kind":"` + strings.Repeat("x", 600) + `"}`
	for name, tc := range map[string]struct {
		line            string
		reason, refusal string
		dropped         int
	}{
		"person":        {line: `{"reason":"person","cut_text_dropped":3}`, reason: runnerwire.LocalWindowEndPerson, dropped: 3},
		"run ended":     {line: `{"reason":"run_ended"}`, reason: runnerwire.LocalWindowEndRunEnded},
		"refused":       {line: `{"reason":"refused","refusal_kind":"not_rfb"}`, reason: runnerwire.LocalWindowEndRefused, refusal: runnerwire.LocalWindowNotRFB},
		"helper failed": {line: `{"reason":"helper_failed"}`, reason: runnerwire.LocalWindowEndHelperFailed},
		"unknown":       {line: `{"reason":"something_else"}`, reason: remote.LocalWindowEndLinkLost},
		"no line":       {reason: remote.LocalWindowEndLinkLost},
		"not json":      {line: `nope`, reason: remote.LocalWindowEndLinkLost},
		"too long":      {line: long, reason: remote.LocalWindowEndLinkLost},
	} {
		t.Run(name, func(t *testing.T) {
			sub, wr, ref := windowRig(t, true)
			wr.line = tc.line
			grant := uuid.New()
			w, err := sub.LocalWindow(ctxT(t), ref, remote.LocalWindowRequest{Nonce: "n1", GrantID: grant, Title: "Fix the build"})
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			waitEnded(t, w)
			select { // the org closes its end once it has the line, before anything calls Close
			case err := <-wr.readEnd:
				var reset *runnerwire.ResetError
				if !errors.As(err, &reset) {
					t.Fatalf("the runner's read ended with %v, want the organisation's reset", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the organisation kept the stream open after the end line")
			}
			if w.EndReason() != tc.reason || w.RefusalKind() != tc.refusal || w.CutTextDropped() != tc.dropped {
				t.Fatalf("end = %q %q %d, want %q %q %d", w.EndReason(), w.RefusalKind(), w.CutTextDropped(), tc.reason, tc.refusal, tc.dropped)
			}
			wr.mu.Lock()
			a := wr.args[0]
			wr.mu.Unlock()
			if a.Ref != ref || a.Nonce != "n1" || a.GrantID != grant || a.Title != "Fix the build" {
				t.Fatalf("args = %+v", a)
			}
		})
	}
	t.Run("no cap", func(t *testing.T) {
		sub, wr, ref := windowRig(t, false)
		if _, err := sub.LocalWindow(ctxT(t), ref, remote.LocalWindowRequest{Nonce: "n"}); !errors.Is(err, remote.ErrLocalWindowUnsupported) {
			t.Fatalf("open on a runner without the cap = %v", err)
		}
		if s, rep := sub.LocalWindowSupported(); s || !rep {
			t.Fatalf("supported %v, reported %v", s, rep)
		}
		if n := wr.calls(); n != 0 {
			t.Fatalf("%d calls reached a runner that did not advertise local windows", n)
		}
	})
	t.Run("nonce refused", func(t *testing.T) {
		sub, wr, ref := windowRig(t, true)
		wr.refuse = runnerwire.Refuse("unknown or expired local window nonce")
		if _, err := sub.LocalWindow(ctxT(t), ref, remote.LocalWindowRequest{Nonce: "n"}); !errors.Is(err, remote.ErrLocalWindowRefused) {
			t.Fatalf("a refused nonce = %v, want ErrLocalWindowRefused", err)
		}
		wr.refuse = &runnerwire.Error{Code: runnerwire.CodeInternal, Message: "display gone"}
		if _, err := sub.LocalWindow(ctxT(t), ref, remote.LocalWindowRequest{Nonce: "n"}); err == nil || errors.Is(err, remote.ErrLocalWindowRefused) {
			t.Fatalf("a failed call = %v, want a plain error", err)
		}
	})
}
