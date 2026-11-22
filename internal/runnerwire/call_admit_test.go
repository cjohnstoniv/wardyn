// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

func TestRunnerwireCall_AdmitByRefusesAtTheWriter(t *testing.T) {
	rig := func(t *testing.T) (*abortRig, *atomic.Int32) {
		var served atomic.Int32
		r := newAbortRig(t, PeerConfig{OnCall: func(ctx context.Context, id uint32, method string, args json.RawMessage) (any, error) {
			served.Add(1)
			return echoHandler(ctx, id, method, args)
		}})
		return r, &served
	}

	t.Run("WriterHeldPastTheCutoff", func(t *testing.T) {
		r, served := rig(t)
		r.org.wmu.Lock() // the link's writer is held from before the cutoff until after it
		by := time.Now().Add(100 * time.Millisecond)
		done := make(chan error, 1)
		go func() {
			var res map[string]string
			done <- r.org.Call(ctxT(t), "echo", map[string]string{"a": "b"}, &res, AdmitBy(by))
		}()
		time.Sleep(time.Until(by) + 50*time.Millisecond)
		select {
		case err := <-done:
			t.Fatalf("the call returned while the writer was held: %v", err)
		default:
		}
		r.org.wmu.Unlock()
		var err error
		select {
		case err = <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("the call never returned once the writer freed")
		}
		if !errors.Is(err, runner.ErrStartCutoff) {
			t.Fatalf("call = %v, want the start cutoff", err)
		}
		r.org.mu.Lock()
		pending := len(r.org.calls)
		r.org.mu.Unlock()
		if pending != 0 {
			t.Fatalf("%d calls still pending, want the refused call's entry gone", pending)
		}
		r.org.replay.mu.Lock()
		buffered, held := len(r.org.replay.frames), r.org.replay.bytes
		r.org.replay.mu.Unlock()
		if buffered != 0 || held != 0 {
			t.Fatalf("replay buffer holds %d frames and %d bytes, want nothing for the refused call", buffered, held)
		}
		// A later call on the same link is served; had the refused CALL gone out, it would have arrived first.
		var res map[string]string
		if err := r.org.Call(ctxT(t), "echo", map[string]string{"c": "d"}, &res); err != nil || res["c"] != "d" {
			t.Fatalf("a later call = %v, %v", res, err)
		}
		if n := served.Load(); n != 1 {
			t.Fatalf("the runner served %d calls, want only the later one", n)
		}
		if n := r.log.count("arrive", TypeCall, 0); n != 1 {
			t.Fatalf("%d CALL frames reached the runner, want only the later one", n)
		}
	})

	t.Run("AdmittedBeforeTheCutoff", func(t *testing.T) {
		r, served := rig(t)
		var res map[string]string
		if err := r.org.Call(ctxT(t), "echo", map[string]string{"a": "b"}, &res, AdmitBy(time.Now().Add(5*time.Second))); err != nil || res["a"] != "b" {
			t.Fatalf("call = %v, %v", res, err)
		}
		if n := served.Load(); n != 1 {
			t.Fatalf("the runner served %d calls, want 1", n)
		}
	})
}
