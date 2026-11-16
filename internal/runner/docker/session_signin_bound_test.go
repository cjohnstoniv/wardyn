// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// signalHangsDocker is fakeDocker whose ContainerKill, and with inspectHangs its ContainerInspect, never answers:
// each sends the time left on its context's deadline (-1 for none) on seen, then waits for the context to end, as a
// daemon that took the connection and never replied.
type signalHangsDocker struct {
	*fakeDocker
	inspectHangs bool
	seen         chan time.Duration
}

func (f signalHangsDocker) hang(ctx context.Context) error {
	left := time.Duration(-1)
	if dl, ok := ctx.Deadline(); ok {
		left = time.Until(dl)
	}
	f.seen <- left
	<-ctx.Done()
	return ctx.Err()
}

func (f signalHangsDocker) ContainerKill(ctx context.Context, _ string, _ client.ContainerKillOptions) (client.ContainerKillResult, error) {
	return client.ContainerKillResult{}, f.hang(ctx)
}

func (f signalHangsDocker) ContainerInspect(ctx context.Context, id string, opts client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if !f.inspectHangs {
		return f.fakeDocker.ContainerInspect(ctx, id, opts)
	}
	return client.ContainerInspectResult{}, f.hang(ctx)
}

// TestSignalMain_BoundsADaemonThatNeverAnswers: SignalMain gives the daemon call a deadline of its own,
// signalMainTimeout at most, when its caller's context has none, for the kill and for the lookup before it, and it
// returns once its context ends.
func TestSignalMain_BoundsADaemonThatNeverAnswers(t *testing.T) {
	for name, inspectHangs := range map[string]bool{"the kill never answers": false, "the lookup never answers": true} {
		t.Run(name, func(t *testing.T) {
			f := signalHangsDocker{fakeDocker: newFakeDocker(), inspectHangs: inspectHangs, seen: make(chan time.Duration, 1)}
			d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev"})
			const ref = "wardyn-agent-signal-test"
			if !inspectHangs {
				d.notVMs.Store(ref, struct{}{}) // a container the lookup already judged, so the kill is reached
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- d.SignalMain(ctx, ref, "SIGCONT") }()
			select {
			case left := <-f.seen:
				if left <= 0 || left > signalMainTimeout {
					t.Fatalf("the daemon call had %v left on its deadline, want within (0, %v]", left, signalMainTimeout)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the daemon was never called")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("SignalMain = %v, want context.Canceled once its context ended", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("SignalMain did not return once its context ended")
			}
		})
	}
}
