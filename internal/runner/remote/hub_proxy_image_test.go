// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A claimed runner's AUTH_OK carries the proxy image the organisation chose for that runner, and says
// nothing when no chooser is set.
func TestHubAuthOKCarriesTheProxyImage(t *testing.T) {
	eachTransport(t, func(t *testing.T, realWire bool) {
		for _, chooser := range []func(types.Runner) string{nil, func(rn types.Runner) string { return "org/proxy-" + rn.Arch }} {
			f := newInventoryFixture(t, realWire)
			f.row.Arch = "arm64"
			f.hub.cfg.ProxyImage = chooser
			want := ""
			if chooser != nil {
				want = "org/proxy-arm64"
			}
			if _, ok, _ := f.open(t, nil); ok.ProxyImage != want {
				t.Fatalf("AUTH_OK named %q, want %q", ok.ProxyImage, want)
			}
		}
	})
}

// A claimed runner's AUTH_OK carries the per-run CPU and memory its declared limits name, and nothing
// when it has declared none (the runner then sizes as before).
func TestHubAuthOKCarriesThePerRunLimits(t *testing.T) {
	eachTransport(t, func(t *testing.T, realWire bool) {
		for _, limits := range []*types.RunnerLimits{nil,
			{RunsAtOnce: 2, PerRun: types.RunnerAmounts{CPUMillis: 4000, MemoryMiB: 8192, DiskMiB: 25 << 10}}} {
			f := newInventoryFixture(t, realWire)
			f.row.Limits = limits
			var cpu, mem int64
			if limits != nil {
				cpu, mem = 4000, 8192
			}
			if _, ok, _ := f.open(t, nil); ok.PerRunCPUMillis != cpu || ok.PerRunMemoryMiB != mem {
				t.Fatalf("AUTH_OK named %d milli-CPU and %d MiB, want %d and %d", ok.PerRunCPUMillis, ok.PerRunMemoryMiB, cpu, mem)
			}
		}
	})
}

// Changed limits reach a connected runner by a GOAWAY and a fresh session: the new AUTH_OK names the
// per-run pair the row holds by then, and the runner's hook hears it.
func TestHubGoawayThenReconnectDeliversTheNewPerRunLimits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	row := types.Runner{ID: uuid.New(), PublicKey: pub, State: types.RunnerClaimed, OrgURLSHA256: federation.OrgURLSHA256("https://org.example")}
	limitsOf := func(cpu, mem int64) *types.RunnerLimits {
		return &types.RunnerLimits{RunsAtOnce: 1, PerRun: types.RunnerAmounts{CPUMillis: cpu, MemoryMiB: mem, DiskMiB: 25 << 10}}
	}
	var stored atomic.Pointer[types.RunnerLimits]
	stored.Store(limitsOf(2000, 4096))
	link := &Link{}
	sub := New(row.ID.String(), link, Options{})
	hub := NewHub(ctx, HubConfig{OrgURL: "https://org.example",
		Lookup: func(context.Context, uuid.UUID) (types.Runner, error) {
			r := row
			r.Limits = stored.Load()
			return r, nil
		},
		Admit: func(context.Context, types.Runner) (Admission, error) {
			return Admission{Substrate: sub, Link: link,
				Pending:        func(context.Context) ([]runnerwire.PendingAction, error) { return nil, nil },
				BeginInventory: func(context.Context, string) error { return nil },
				StatePage:      func(context.Context, string, runnerwire.StatePage) error { return nil },
				Event:          func(context.Context, runnerwire.Event) error { return nil },
			}, nil
		}})
	defer hub.Close()
	online, resumed := make(chan *runnerwire.Peer, 2), make(chan bool, 2)
	heard := make(chan [2]int64, 4)
	hooks := sessionHooks(online, resumed)
	hooks.PerRunLimits = func(cpu, mem int64) { heard <- [2]int64{cpu, mem} }
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: "https://org.example", RunnerID: row.ID.String(), PrivateKey: key, Hooks: hooks,
		Dial: func(context.Context) (runnerwire.Conn, error) {
			org, run := runnerwire.Loopback()
			go func() { _ = hub.Serve(ctx, org, row.ID) }()
			return run, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() { done <- client.Connect(ctx) }()
	awaitValue(t, ctx, online)
	waitOnline(t, ctx, link)
	if got := awaitValue(t, ctx, heard); got != [2]int64{2000, 4096} {
		t.Fatalf("first session heard %v", got)
	}
	stored.Store(limitsOf(4000, 8192))
	if err := hub.Goaway(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	if err := awaitValue(t, ctx, done); !errors.Is(err, runnerwire.ErrGoaway) {
		t.Fatalf("GOAWAY control: %v", err)
	}
	go func() { done <- client.Connect(ctx) }()
	awaitValue(t, ctx, online)
	if got := awaitValue(t, ctx, heard); got != [2]int64{4000, 8192} {
		t.Fatalf("session after the GOAWAY heard %v, want the new limits", got)
	}
}
