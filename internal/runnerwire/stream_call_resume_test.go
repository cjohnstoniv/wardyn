// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// A CALL whose write fails after it was sequenced stays in the replay buffer while its caller is told the
// runner is offline. For a stream call such as exec_stream the replay a resume would send is flagged, so the
// session starts fresh and the runner never starts the exec; a lifecycle call such as stop keeps its replay.
func TestAStreamCallLostInItsWriteIsFlaggedInTheReplay(t *testing.T) {
	for _, tc := range []struct {
		method string
		want   bool
	}{{MethodExecStream, true}, {MethodStop, false}} {
		t.Run(tc.method, func(t *testing.T) {
			r := newRaw(t, PeerConfig{Org: true})
			r.raw.SeverMidFrame() // the CALL is the next frame written: it arrives cut short and the link drops
			if err := r.p.Call(ctxT(t), tc.method, nil, nil); !errors.Is(err, runner.ErrRunnerOffline) {
				t.Fatalf("call = %v, want the runner offline", err)
			}
			cfg, err := r.p.ResumeConfig(ctxT(t))
			if err != nil {
				t.Fatal(err)
			}
			replay, err := cfg.Replay.Since(0) // the runner received nothing
			if err != nil || len(replay) != 1 || replay[0].Type != TypeCall {
				t.Fatalf("replay = %+v, %v; want the one CALL", replay, err)
			}
			if got := ReplaysStreamCall(replay); got != tc.want {
				t.Fatalf("ReplaysStreamCall = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReplaysStreamCallReadsOnlyCalls(t *testing.T) {
	call := func(method string) Frame {
		b, _ := json.Marshal(Call{Method: method})
		return Frame{Type: TypeCall, Stream: 2, Payload: b}
	}
	for name, tc := range map[string]struct {
		replay []Frame
		want   bool
	}{
		"empty":                 {nil, false},
		"lifecycle calls":       {[]Frame{call(MethodStop), call(MethodKill), call(MethodEnd)}, false},
		"a reply and an event":  {[]Frame{replyFrame(2, 1), {Type: TypeEvent, Payload: []byte(`{}`)}}, false},
		"attach after a stop":   {[]Frame{call(MethodStop), call(MethodAttach)}, true},
		"sync_folder":           {[]Frame{call(MethodSyncFolder)}, true},
		"a CALL it cannot read": {[]Frame{{Type: TypeCall, Stream: 2, Payload: []byte("{")}}, true},
	} {
		if got := ReplaysStreamCall(tc.replay); got != tc.want {
			t.Errorf("%s: ReplaysStreamCall = %v, want %v", name, got, tc.want)
		}
	}
}

// onReply removes the call and leaves the streams it opened to the caller; when the caller's cancellation
// then wins its select over the queued REPLY, the caller still resets those streams instead of leaving
// them parked until the link drops. The select picks at random between the two, so the race is run until
// the cancellation has won several times.
func TestACancellationAfterTheReplyResetsTheCallsStreams(t *testing.T) {
	cancelled := 0
	for i := 0; i < 200 && cancelled < 8; i++ {
		r := newRaw(t, PeerConfig{Org: true})
		ctx, cancel := context.WithCancel(ctxT(t))
		inEvent, release := make(chan struct{}), make(chan struct{})
		errc := make(chan error, 1)
		go func() {
			errc <- r.p.Call(ctx, MethodExecStream, nil, nil, WithEvents(func(Event) {
				close(inEvent)
				<-release // the caller is out of its select while the REPLY and the cancellation land
			}))
		}()
		call := r.readType(t, TypeCall)
		open, _ := json.Marshal(Open{Kind: KindExec, Call: call.Stream})
		r.write(t, Frame{Type: TypeOpen, Stream: 1, Payload: open})
		r.readType(t, TypeOpenOK)
		ev, _ := json.Marshal(Event{Kind: EventWaiting, Call: call.Stream})
		r.write(t, Frame{Type: TypeEvent, Seq: 1, Payload: ev})
		<-inEvent
		r.write(t, replyFrame(call.Stream, 2))
		waitFor(t, 3*time.Second, "onReply to remove the call", func() bool { return !r.pending(call.Stream) })
		cancel()
		close(release)
		err := <-errc
		r.p.mu.Lock()
		parked, byCall := len(r.p.parked), len(r.p.byCall)
		r.p.mu.Unlock()
		if err == nil {
			if _, terr := r.p.TakeStream(1); terr != nil {
				t.Fatalf("the REPLY won but its stream is not there to take: %v", terr)
			}
			continue
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("call = %v, want the cancellation or the REPLY", err)
		}
		cancelled++
		if parked != 0 || byCall != 0 {
			t.Fatalf("after the cancellation won: %d streams parked, %d calls hold streams; want none", parked, byCall)
		}
		for {
			if f := r.readType(t, TypeReset); f.Stream == 1 {
				if code, _ := DecodeReset(f.Payload); code != ResetCancelled {
					t.Fatalf("stream 1 reset with code %d, want cancelled", code)
				}
				break
			}
		}
	}
	if cancelled == 0 {
		t.Fatal("the cancellation never won its select over the REPLY")
	}
}
