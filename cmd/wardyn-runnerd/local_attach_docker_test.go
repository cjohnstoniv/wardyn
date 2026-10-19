// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// The runtime opens the socket only when --local-attach is on and the organisation's AUTH_OK allows
// it, and closes it when a later AUTH_OK does not.
func TestLocalAttachRuntimeOpensOnlyWhenOnAndAllowed(t *testing.T) {
	for _, on := range []bool{false, true} {
		cfg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", cfg)
		t.Setenv("HOME", cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, key, _ := ed25519.GenerateKey(rand.Reader)
		id := uuid.New()
		rt, err := runnerRuntimeWith(ctx, runneridentity.Identity{RunnerID: id, PrivateKey: key}, t.TempDir(), runnertest.NewFake(),
			func(id uuid.UUID) string { return "fake-" + id.String() }, newLocalRelay(ctx), unsized, runtimeExtras{localAttach: on, orgURL: "https://org.example"})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		pointer := filepath.Join(cfg, "wardyn", "local-attach", id.String()+".json")
		rt.hooks.LocalAttach(true)
		if _, err := os.Lstat(pointer); (err == nil) != on {
			t.Errorf("--local-attach on=%v, AUTH_OK true: pointer file present = %v", on, err == nil)
		}
		rt.hooks.LocalAttach(false)
		if _, err := os.Lstat(pointer); err == nil {
			t.Errorf("--local-attach on=%v, AUTH_OK false: the pointer file stayed", on)
		}
		_ = rt.close()
		cancel()
	}
}

// The caps event carries LocalAttach exactly when --local-attach is on (a Docker runner); off
// withdraws it.
func TestLocalAttachCapsFollowTheFlag(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, key, _ := ed25519.GenerateKey(rand.Reader)
		rt, err := runnerRuntimeWith(ctx, runneridentity.Identity{RunnerID: uuid.New(), PrivateKey: key}, t.TempDir(), runnertest.NewFake(),
			func(id uuid.UUID) string { return "fake-" + id.String() }, newLocalRelay(ctx), unsized, runtimeExtras{localAttach: on, orgURL: "https://org.example"})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		caps := make(chan runnerwire.Caps, 1)
		orgEnd, runEnd := runnerwire.Loopback()
		org := runnerwire.NewPeer(orgEnd, runnerwire.PeerConfig{Org: true, OnEvent: func(e runnerwire.Event) {
			var c runnerwire.Caps
			if e.Kind == runnerwire.EventCaps && json.Unmarshal(e.Data, &c) == nil {
				select {
				case caps <- c:
				default:
				}
			}
		}})
		run := runnerwire.NewPeer(runEnd, runnerwire.PeerConfig{})
		go func() { _ = org.Run(ctx) }()
		go func() { _ = run.Run(ctx) }()
		go func() { _ = rt.hooks.Replay(ctx, run) }()
		select {
		case c := <-caps:
			if c.LocalAttach != on {
				t.Errorf("--local-attach on=%v: caps LocalAttach = %v", on, c.LocalAttach)
			}
		case <-ctx.Done():
			t.Errorf("--local-attach on=%v: no caps event", on)
		}
		cancel()
		org.Close()
		run.Close()
		_ = rt.close()
	}
}
