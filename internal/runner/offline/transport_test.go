// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/coder/websocket"
	"github.com/google/uuid"
)

type wireManager struct {
	manager  *Manager
	link     *remote.Link
	peer     *runnerwire.Peer
	orgTime  time.Time
	elapsed  atomic.Int64
	wallJump atomic.Int64
	activity chan uuid.UUID
	done     chan error
	ctx      context.Context
}

func connectWireManager(t *testing.T, dir string, sub substrate.Substrate, ref func(uuid.UUID) string, realWire bool) *wireManager {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	row := types.Runner{ID: uuid.New(), PublicKey: pub, State: types.RunnerClaimed, Owner: "runner-owner"}
	m, err := New(dir, row.ID, sub, ref, noOutput, noErase)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	f := &wireManager{manager: m, link: &remote.Link{}, ctx: ctx, done: make(chan error, 1), activity: make(chan uuid.UUID, 16)}
	base := time.Now()
	m.now = func() time.Time { return base.Add(time.Duration(f.wallJump.Load())) }
	m.since = func(time.Time) time.Duration { return time.Duration(f.elapsed.Load()) }
	m.clock = NewClock(m.since)
	orgURL := "https://org.example"
	var hub *remote.Hub
	if realWire {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := websocket.Accept(w, r, nil)
			if err == nil {
				_ = hub.Serve(ctx, runnerwire.NewWebSocket(c), row.ID)
			}
		}))
		t.Cleanup(server.Close)
		orgURL = server.URL
	}
	row.OrgURLSHA256 = federation.OrgURLSHA256(orgURL)
	remoteSub := remote.New(row.ID.String(), f.link, remote.Options{})
	hub = remote.NewHub(ctx, remote.HubConfig{OrgURL: orgURL, Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil }, Admit: func(context.Context, types.Runner) (remote.Admission, error) {
		return remote.Admission{Substrate: remoteSub, Link: f.link, Pending: func(context.Context) ([]runnerwire.PendingAction, error) { return nil, nil }, BeginInventory: func(context.Context, string) error { return nil }, StatePage: func(context.Context, string, runnerwire.StatePage) error { return nil }, Event: func(context.Context, runnerwire.Event) error { return nil }, Activity: func(_ context.Context, id uuid.UUID) error { f.activity <- id; return nil }}, nil
	}})
	t.Cleanup(hub.Close)
	online := make(chan *runnerwire.Peer, 1)
	hooks := runnerwire.RunnerHooks{
		Call: func(context.Context, *runnerwire.Peer, uint32, string, json.RawMessage) (any, error) {
			return true, nil
		},
		Open: func(*runnerwire.Peer, *runnerwire.Stream, runnerwire.Open) error { return runnerwire.ErrNotReady },
		Clock: func(h runnerwire.Hello, received time.Time) error {
			f.orgTime = h.ServerTime
			return m.Clock(h, received)
		},
		Pending: m.Pending, StatePages: m.StatePages, Replay: m.Replay, BeforeReady: m.BeforeReady, Lease: m.Lease, Revoked: m.Revoked, Offline: m.Offline,
		Online: func(p *runnerwire.Peer, _ bool) { m.Online(p); online <- p },
	}
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: orgURL, RunnerID: row.ID.String(), PrivateKey: key, Hooks: hooks, Dial: func(ctx context.Context) (runnerwire.Conn, error) {
		if realWire {
			return runnerwire.DialWebSocket(ctx, orgURL, row.ID.String(), nil)
		}
		org, run := runnerwire.Loopback()
		go func() { _ = hub.Serve(ctx, org, row.ID) }()
		return run, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	go func() { f.done <- client.Connect(ctx) }()
	select {
	case f.peer = <-online:
	case err := <-f.done:
		t.Fatalf("connect: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	t.Cleanup(func() { f.peer.Close() })
	for {
		var reply bool
		if err := f.link.Call(ctx, "probe", nil, &reply); err == nil && reply {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	return f
}

func (f *wireManager) initialLease(t *testing.T, id uuid.UUID) {
	t.Helper()
	b, err := runnerwire.Marshal(runnerwire.Lease{RunID: id, TokenRenewedAt: f.orgTime, EndsAt: f.orgTime.Add(3 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.link.Send(f.ctx, runnerwire.Frame{Type: runnerwire.TypeLease, Payload: b}); err != nil {
		t.Fatal(err)
	}
	// A checked LEASE is processed before the subsequent CALL on this stream.
	var reply bool
	if err := f.link.Call(f.ctx, "probe", nil, &reply); err != nil {
		t.Fatal(err)
	}
}
func (f *wireManager) drop(t *testing.T) {
	t.Helper()
	f.peer.Close()
	select {
	case <-f.done:
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	f.manager.mu.Lock()
	online := f.manager.online
	f.manager.mu.Unlock()
	if online {
		t.Fatal("transport teardown did not clear offline admission")
	}
}
func TestOfflineTransportDropUsesMonotonicLease(t *testing.T) {
	for _, realWire := range []bool{false, true} {
		name := "loopback"
		if realWire {
			name = "websocket"
		}
		t.Run(name, func(t *testing.T) {
			sub := &localSub{status: runner.Status{State: types.RunRunning}, agent: runner.Status{State: types.RunRunning}}
			f := connectWireManager(t, t.TempDir(), sub, uuid.UUID.String, realWire)
			id := uuid.New()
			f.initialLease(t, id)
			spec := runner.SandboxSpec{RunID: id}
			if err := f.manager.BeforeCreate(f.ctx, spec); err != nil {
				t.Fatal(err)
			}
			if err := f.manager.Created(f.ctx, spec, id.String()); err != nil {
				t.Fatal(err)
			}
			// A lease accepted before the drop authorizes nothing once offline.
			other := uuid.New()
			f.initialLease(t, other)
			f.drop(t)
			if err := f.manager.BeforeCreate(f.ctx, runner.SandboxSpec{RunID: other}); err == nil {
				t.Fatal("offline create admitted")
			}
			f.wallJump.Store(int64(-365 * 24 * time.Hour))
			f.elapsed.Store(int64(identity.RunTokenLapseAfter - time.Nanosecond))
			if err := f.manager.CheckLapses(f.ctx); err != nil {
				t.Fatal(err)
			}
			if sub.stops != 0 || sub.kills != 0 {
				t.Fatal("drop destroyed live agent or cut unexpired lease")
			}
			f.elapsed.Store(int64(identity.RunTokenLapseAfter))
			if err := f.manager.CheckLapses(f.ctx); err != nil {
				t.Fatal(err)
			}
			if sub.stops != 1 || sub.kills != 0 {
				t.Fatal("monotonic lapse did not stop only proxy")
			}
		})
	}
}
