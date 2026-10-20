// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// lostEvent faults after Peer has buffered the event, before the transport
// writes it. Resume must deliver that event, not merely reconnect successfully.
type lostEvent struct {
	runnerwire.Conn
	lose atomic.Bool
}

func (c *lostEvent) WriteFrame(ctx context.Context, f runnerwire.Frame) error {
	if f.Type == runnerwire.TypeEvent && c.lose.CompareAndSwap(true, false) {
		_ = c.Conn.Close()
		return runnerwire.ErrLinkDown
	}
	return c.Conn.WriteFrame(ctx, f)
}

func emptyInventory(_ context.Context, send func(runnerwire.StatePage) error) error {
	return send(runnerwire.StatePage{InventoryID: uuid.New(), Final: true})
}

func emptyStatePage(t *testing.T) []byte {
	t.Helper()
	payload, err := runnerwire.Marshal(runnerwire.StatePage{InventoryID: uuid.New(), Final: true})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func sessionHooks(online chan<- *runnerwire.Peer, resumed chan<- bool) runnerwire.RunnerHooks {
	return runnerwire.RunnerHooks{
		Call: func(_ context.Context, _ *runnerwire.Peer, _ uint32, _ string, _ json.RawMessage) (any, error) {
			return "reply", nil
		},
		Open:        func(*runnerwire.Peer, *runnerwire.Stream, runnerwire.Open) error { return runnerwire.ErrNotReady },
		Clock:       func(runnerwire.Hello, time.Time) error { return nil },
		Pending:     func(context.Context, *runnerwire.Peer, []runnerwire.PendingAction) error { return nil },
		StatePages:  emptyInventory,
		Replay:      func(context.Context, *runnerwire.Peer) error { return nil },
		BeforeReady: func(context.Context, *runnerwire.Peer, bool) error { return nil },
		Lease:       func(context.Context, runnerwire.Lease) error { return nil },
		Revoked:     func(context.Context) error { return nil },
		Offline:     func(context.Context) {},
		Online:      func(p *runnerwire.Peer, r bool) { resumed <- r; online <- p },
	}
}

func TestHubDropResumeReplaysLostEvent(t *testing.T) {
	for _, realWire := range []bool{false, true} {
		name := "loopback"
		if realWire {
			name = "websocket"
		}
		t.Run(name, func(t *testing.T) { testHubResume(t, realWire) })
	}
}

func testHubResume(t *testing.T, realWire bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	row := types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: federation.OrgURLSHA256("https://org.example"), State: types.RunnerClaimed}
	link := &Link{}
	sub := New(row.ID.String(), link, Options{})
	var begins, states, events atomic.Int32
	config := HubConfig{OrgURL: "https://org.example", Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil },
		Admit: func(context.Context, types.Runner) (Admission, error) {
			return Admission{Substrate: sub, Link: link,
				Pending:        func(context.Context) ([]runnerwire.PendingAction, error) { return nil, nil },
				BeginInventory: func(context.Context, string) error { begins.Add(1); return nil },
				StatePage:      func(context.Context, string, runnerwire.StatePage) error { states.Add(1); return nil },
				Event:          func(context.Context, runnerwire.Event) error { events.Add(1); return nil },
			}, nil
		},
	}
	var hub *Hub
	hubDone := make(chan error, 4)
	if realWire {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := websocket.Accept(w, r, nil)
			if err == nil {
				err = hub.Serve(ctx, runnerwire.NewWebSocket(conn), row.ID)
			}
			hubDone <- err
		}))
		defer server.Close()
		config.OrgURL = server.URL
	}
	row.OrgURLSHA256 = federation.OrgURLSHA256(config.OrgURL)
	hub = NewHub(ctx, config)
	defer hub.Close()
	online, resumed := make(chan *runnerwire.Peer, 2), make(chan bool, 2)
	connections := make(chan *lostEvent, 2)
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: config.OrgURL, RunnerID: row.ID.String(), PrivateKey: key, Hooks: sessionHooks(online, resumed),
		Dial: func(ctx context.Context) (runnerwire.Conn, error) {
			var conn runnerwire.Conn
			if realWire {
				var err error
				conn, err = runnerwire.DialWebSocket(ctx, config.OrgURL, row.ID.String(), nil)
				if err != nil {
					return nil, err
				}
			} else {
				org, run := runnerwire.Loopback()
				conn = run
				go func() { hubDone <- hub.Serve(ctx, org, row.ID) }()
			}
			fault := &lostEvent{Conn: conn}
			connections <- fault
			return fault, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() { done <- client.Connect(ctx) }()
	p := awaitValue(t, ctx, online)
	if awaitValue(t, ctx, resumed) {
		t.Fatal("first session resumed")
	}
	waitOnline(t, ctx, link)
	var reply string
	if err := link.Call(ctx, "probe", nil, &reply); err != nil || reply != "reply" {
		t.Fatalf("call %q: %v", reply, err)
	}
	fault := awaitValue(t, ctx, connections)
	fault.lose.Store(true)
	if err := p.Event(ctx, runnerwire.Event{Kind: runnerwire.EventSpool}); err == nil {
		t.Fatal("event fault did not fire")
	}
	_ = awaitValue(t, ctx, done)
	_ = awaitValue(t, ctx, hubDone)
	go func() { done <- client.Connect(ctx) }()
	p = awaitValue(t, ctx, online)
	if !awaitValue(t, ctx, resumed) {
		t.Fatal("connection restarted instead of resuming")
	}
	waitOnline(t, ctx, link)
	for events.Load() != 1 {
		select {
		case <-ctx.Done():
			t.Fatal("lost event was not replayed")
		case <-time.After(time.Millisecond):
		}
	}
	if begins.Load() != 1 || states.Load() != 1 {
		t.Fatalf("resume repeated full admission: %d inventories, %d STATE pages", begins.Load(), states.Load())
	}
	if err := link.Call(ctx, "probe", nil, &reply); err != nil {
		t.Fatal(err)
	}
	p.Close()
	_ = awaitValue(t, ctx, done)
}

func awaitValue[T any](t *testing.T, ctx context.Context, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		var zero T
		return zero
	}
}

func waitOnline(t *testing.T, ctx context.Context, link *Link) {
	t.Helper()
	for !link.Online() {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

func TestHubAuthenticationBeforeAdmission(t *testing.T) {
	for _, state := range []types.RunnerState{types.RunnerUnclaimed, types.RunnerRevoked, types.RunnerClaimed} {
		t.Run(string(state), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			pub, key, _ := ed25519.GenerateKey(rand.Reader)
			row := types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: federation.OrgURLSHA256("https://org.example"), State: state, CreatedAt: time.Now()}
			var admitted atomic.Int32
			hub := NewHub(ctx, HubConfig{OrgURL: "https://org.example", Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil },
				Admit: func(context.Context, types.Runner) (Admission, error) {
					admitted.Add(1)
					return Admission{}, errors.New("test admission stopped")
				},
			})
			defer hub.Close()
			org, run := runnerwire.Loopback()
			defer run.Close()
			done := make(chan error, 1)
			go func() { done <- hub.Serve(ctx, org, row.ID) }()
			_, err := runnerwire.AuthenticateOrg(ctx, run, row.ID.String(), "https://org.example", key, nil)
			_ = awaitValue(t, ctx, done)
			want := int32(0)
			if state == types.RunnerClaimed {
				want = 1
			}
			if admitted.Load() != want {
				t.Fatalf("admitted %s: %d", state, admitted.Load())
			}
			if state == types.RunnerRevoked && !errors.Is(err, runnerwire.ErrRevoked) {
				t.Fatal(err)
			}
		})
	}
}

// An unclaimed runner is sent its AUTH_OK and then a GOAWAY, so the refusal arrives after the handshake. The
// client still reports the registration stage, from the typed reason, and a runner the organisation revoked
// after admitting it does too.
func TestHubUnclaimedRunnerReportsTheRegistrationStage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	row := types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: federation.OrgURLSHA256("https://org.example"), State: types.RunnerUnclaimed, CreatedAt: time.Now()}
	hub := NewHub(ctx, HubConfig{OrgURL: "https://org.example", Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil },
		Admit: func(context.Context, types.Runner) (Admission, error) {
			return Admission{}, errors.New("an unclaimed runner is never admitted")
		}})
	defer hub.Close()
	org, run := runnerwire.Loopback()
	go func() { _ = hub.Serve(ctx, org, row.ID) }()
	online, resumed := make(chan *runnerwire.Peer, 1), make(chan bool, 1)
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: "https://org.example", RunnerID: row.ID.String(), PrivateKey: key,
		Hooks: sessionHooks(online, resumed), Dial: func(context.Context) (runnerwire.Conn, error) { return run, nil }})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Connect(ctx)
	if !errors.Is(err, runnerwire.ErrGoaway) {
		t.Fatalf("connect = %v, want the GOAWAY", err)
	}
	if got := runnerwire.StageOf(err); got != runnerwire.StageRegistration {
		t.Errorf("stage = %q, want registration (%v)", got, err)
	}
}
