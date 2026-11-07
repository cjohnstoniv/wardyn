// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// remoteWindowRig is the organisation's remote substrate over a served runner whose substrate opens fake
// windows, with the runner's caps (window set or not) delivered.
func remoteWindowRig(t *testing.T, window bool) (*remote.Substrate, *windowSub, string) {
	t.Helper()
	var ws *windowSub
	_, sub, _, _, ref := streamRigWith(t, func(f *runnertest.Fake) substrate.Substrate {
		ws = &windowSub{contextSubstrate: contextSubstrate{f}}
		return ws
	})
	if s, r := sub.MacWindowSupported(); s || r {
		t.Fatalf("before any caps: supported %v, reported %v", s, r)
	}
	caps, err := runnerwire.Marshal(runnerwire.Caps{Version: "test", MacOSVM: &runnerwire.MacVMCaps{Max: 1, Window: window}})
	if err != nil {
		t.Fatal(err)
	}
	sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: caps})
	if s, r := sub.MacWindowSupported(); s != window || !r {
		t.Fatalf("after caps: supported %v, reported %v, want %v and true", s, r, window)
	}
	return sub, ws, ref
}

func ended(t *testing.T, w *remote.MacWindowSession) string {
	t.Helper()
	select {
	case <-w.Ended():
	case <-time.After(5 * time.Second):
		t.Fatal("the window did not end")
	}
	return w.EndReason()
}

// The organisation opens a window through the runner and hears why it ended: the runner's four reasons
// as they are, helper_lost and a reason it does not know as link_lost.
func TestRemoteMacWindowEndReasons(t *testing.T) {
	for helper, want := range map[string]string{
		runner.MacWindowEndPerson:       "closed_by_person",
		runner.MacWindowEndHostLocked:   "host_locked",
		runner.MacWindowEndRunEnded:     "run_ended",
		runner.MacWindowEndHelperFailed: "helper_failed",
		runner.MacWindowEndHelperLost:   "link_lost",
		"a newer reason":                "link_lost",
	} {
		sub, ws, ref := remoteWindowRig(t, true)
		w, err := sub.MacWindow(t.Context(), ref, remote.MacWindowRequest{GrantID: "g1", Title: "Run, for Pat"})
		if err != nil {
			t.Fatalf("%s: %v", helper, err)
		}
		if ws.opens() != 1 || ws.reqs[0].GrantID != "g1" || ws.reqs[0].Title != "Run, for Pat" {
			t.Fatalf("%s: the helper was asked %+v", helper, ws.reqs)
		}
		ws.win(0).helperEnds(helper)
		if got := ended(t, w); got != want {
			t.Errorf("helper reason %q read as %q, want %q", helper, got, want)
		}
		_ = w.Close()
	}
}

// Closing the session ends the window on the runner, once or twice, and a runner whose link is lost
// reads as link_lost.
func TestRemoteMacWindowCloseEndsTheHelperConnection(t *testing.T) {
	sub, ws, ref := remoteWindowRig(t, true)
	w, err := sub.MacWindow(t.Context(), ref, remote.MacWindowRequest{GrantID: "g1"})
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	_ = w.Close()
	waitClosed(t, ws.win(0), "the organisation closed the session")
	if got := ended(t, w); got != remote.MacWindowEndLinkLost {
		t.Errorf("a session the organisation closed reads %q", got)
	}
}

// The runner's refusals the organisation words itself are told apart; anything else stays an error.
func TestRemoteMacWindowRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		err                   error
		notAtMac, op, unknown bool
	}{
		"already open":    {err: runner.ErrMacWindowOpen, op: true},
		"not at the Mac":  {err: &runner.MacWindowRefusal{Code: macvm.WindowCodeNotAtMac, Sentence: macvm.WindowNotAtMacSentence}, notAtMac: true},
		"screen unread":   {err: &runner.MacWindowRefusal{Code: macvm.WindowCodeNotAtMac, Sentence: macvm.WindowUnreadableSentence}, notAtMac: true},
		"lock unknown":    {err: &runner.MacWindowRefusal{Code: macvm.WindowCodeLockUnknown, Sentence: macvm.WindowLockUnknownSentence}, unknown: true},
		"helper too old":  {err: runner.ErrMacWindowHelperOld},
		"not running":     {err: &runner.MacWindowRefusal{Code: macvm.WindowCodeNotRunning, Sentence: macvm.WindowNotRunningSentence}},
		"runner's own":    {err: errors.New("disk on fire")},
		"no window to be": {err: runner.ErrMacWindowUnsupported},
	} {
		sub, ws, ref := remoteWindowRig(t, true)
		ws.err = tc.err
		w, err := sub.MacWindow(t.Context(), ref, remote.MacWindowRequest{GrantID: "g1"})
		if err == nil || w != nil {
			t.Fatalf("%s: opened (%v, %v)", name, w, err)
		}
		if got := errors.Is(err, remote.ErrMacWindowNotAtMac); got != tc.notAtMac {
			t.Errorf("%s: not-at-Mac = %v for %v", name, got, err)
		}
		if got := errors.Is(err, remote.ErrMacWindowAlreadyOpen); got != tc.op {
			t.Errorf("%s: already-open = %v for %v", name, got, err)
		}
		if got := errors.Is(err, remote.ErrMacWindowLockUnknown); got != tc.unknown {
			t.Errorf("%s: lock-unknown = %v for %v", name, got, err)
		}
	}
}

// A second open of a run that holds a window is the runner's slot refusing it, and reads as already open.
func TestRemoteMacWindowSecondOpenIsAlreadyOpen(t *testing.T) {
	sub, _, ref := remoteWindowRig(t, true)
	w, err := sub.MacWindow(t.Context(), ref, remote.MacWindowRequest{GrantID: "g1"})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := sub.MacWindow(t.Context(), ref, remote.MacWindowRequest{GrantID: "g2"}); !errors.Is(err, remote.ErrMacWindowAlreadyOpen) {
		t.Fatalf("second open = %v, want already open", err)
	}
}

// A runner without the window cap reports it unsupported, and a sandbox of another runner is no window's.
func TestRemoteMacWindowSupportComesFromCaps(t *testing.T) {
	sub, _, _ := remoteWindowRig(t, false)
	if s, r := sub.MacWindowSupported(); s || !r {
		t.Fatalf("a runner without the cap: supported %v, reported %v", s, r)
	}
	if _, err := sub.MacWindow(t.Context(), "runner:other/x", remote.MacWindowRequest{GrantID: "g1"}); !errors.Is(err, remote.ErrForeignRef) {
		t.Fatalf("a foreign ref = %v", err)
	}
}
