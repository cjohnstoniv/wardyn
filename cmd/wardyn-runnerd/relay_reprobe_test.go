// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestEveryReconnectReprobesTheRelay: a fresh session's caps re-check the relay before they go and carry
// its status and the waiting offline summary; a resumed session re-checks it too, and sends caps again
// once the relay's state changed.
func TestEveryReconnectReprobesTheRelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var probes atomic.Int32
	var down atomic.Bool
	watch := newRelayWatch(func(context.Context) error {
		probes.Add(1)
		if down.Load() {
			return errors.New("relay: nothing answers")
		}
		return nil
	}, nil)
	offline := newOfflineLog(t.TempDir(), nil)
	offline.connectFailed(&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "org.example"}})
	rt, err := runnerRuntimeWith(ctx, runneridentity.Identity{RunnerID: uuid.New(), PrivateKey: key}, t.TempDir(), runnertest.NewFake(),
		func(id uuid.UUID) string { return "fake-" + id.String() }, newLocalRelay(ctx), unsized,
		runtimeExtras{report: connectReport{log: offline, relay: watch}})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.close()

	caps := make(chan runnerwire.Caps, 4)
	a, b := runnerwire.Loopback()
	org := runnerwire.NewPeer(a, runnerwire.PeerConfig{Org: true, OnEventChecked: func(ev runnerwire.Event) error {
		var c runnerwire.Caps
		if ev.Kind == runnerwire.EventCaps && runnerwire.Unmarshal(ev.Data, &c) == nil {
			caps <- c
		}
		return nil
	}})
	run := runnerwire.NewPeer(b, runnerwire.PeerConfig{})
	defer org.Close()
	defer run.Close()
	go org.Run(ctx) //nolint:errcheck // the test owns shutdown
	go run.Run(ctx) //nolint:errcheck // the test owns shutdown
	next := func() runnerwire.Caps {
		t.Helper()
		select {
		case c := <-caps:
			return c
		case <-ctx.Done():
			t.Fatal("no caps sent")
			return runnerwire.Caps{}
		}
	}

	if err := rt.hooks.Replay(ctx, run); err != nil {
		t.Fatal(err)
	}
	c := next()
	if probes.Load() != 1 || c.Relay == nil || c.Relay.State != types.RelayReachable {
		t.Fatalf("fresh session: %d probes, relay %+v; want one probe before caps", probes.Load(), c.Relay)
	}
	if c.OfflineSummary == nil || c.OfflineSummary.Stage != types.RunnerStageDNS {
		t.Fatalf("fresh session carries summary %+v, want the dns failure", c.OfflineSummary)
	}

	down.Store(true)
	rt.hooks.Online(run, true)
	c = next()
	if probes.Load() != 2 || c.Relay == nil || c.Relay.State != types.RelayUnreachable {
		t.Fatalf("resumed session: %d probes, relay %+v; want a second probe reporting unreachable", probes.Load(), c.Relay)
	}
	if c.OfflineSummary != nil {
		t.Fatalf("an acknowledged summary is sent again: %+v", c.OfflineSummary)
	}
}
