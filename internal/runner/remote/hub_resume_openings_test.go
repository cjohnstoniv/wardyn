// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// unreadCall is the runner's end of a link that goes down instead of handing the read loop the first
// CALL of method: the org wrote the frame, and the runner never read it.
type unreadCall struct {
	runnerwire.Conn
	method  string
	armed   atomic.Bool
	dropped chan struct{}
}

func (c *unreadCall) ReadFrame(ctx context.Context) (runnerwire.Frame, error) {
	f, err := c.Conn.ReadFrame(ctx)
	if err != nil || f.Type != runnerwire.TypeCall || !c.armed.Load() {
		return f, err
	}
	var call runnerwire.Call
	if runnerwire.Unmarshal(f.Payload, &call) == nil && call.Method == c.method && c.armed.CompareAndSwap(true, false) {
		_ = c.Conn.Close()
		close(c.dropped)
		return runnerwire.Frame{}, runnerwire.ErrLinkDown
	}
	return f, err
}

// A start CALL written before its cutoff, on a link that fails before the runner reads it, never
// reaches the runner: the runner reconnects in time, and the hub starts a fresh session, which replays
// nothing, instead of resuming one whose replay holds the opening.
func TestHubResumeDropsUnreadOpenings(t *testing.T) {
	openings := map[string]func(context.Context, *Substrate, string) error{
		runnerwire.MethodExecStream: func(ctx context.Context, s *Substrate, ref string) error {
			_, err := s.ExecStream(ctx, ref, runner.ExecSpec{Argv: []string{"socat"}})
			return err
		},
		runnerwire.MethodAttach: func(ctx context.Context, s *Substrate, ref string) error {
			_, err := s.Attach(ctx, ref, runner.AttachOptions{Cols: 80, Rows: 24})
			return err
		},
		runnerwire.MethodDialDesktop: func(ctx context.Context, s *Substrate, ref string) error {
			_, err := s.DialDesktop(ctx, ref)
			return err
		},
	}
	for method, open := range openings {
		t.Run(method, func(t *testing.T) { testHubResumeDropsUnreadOpening(t, method, open) })
	}
}

func testHubResumeDropsUnreadOpening(t *testing.T, method string, open func(context.Context, *Substrate, string) error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	row := types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: federation.OrgURLSHA256("https://org.example"), State: types.RunnerClaimed}
	link := &Link{}
	sub := New(row.ID.String(), link, Options{})
	hub := NewHub(ctx, HubConfig{OrgURL: "https://org.example", Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil },
		Admit: func(context.Context, types.Runner) (Admission, error) {
			return Admission{Substrate: sub, Link: link,
				Pending:        func(context.Context) ([]runnerwire.PendingAction, error) { return nil, nil },
				BeginInventory: func(context.Context, string) error { return nil },
				StatePage:      func(context.Context, string, runnerwire.StatePage) error { return nil },
				Event:          func(context.Context, runnerwire.Event) error { return nil },
			}, nil
		},
	})
	defer hub.Close()
	online, resumed := make(chan *runnerwire.Peer, 2), make(chan bool, 2)
	hooks := sessionHooks(online, resumed)
	var served atomic.Int32
	hooks.Call = func(_ context.Context, _ *runnerwire.Peer, _ uint32, m string, _ json.RawMessage) (any, error) {
		if m == method {
			served.Add(1)
		}
		return "reply", nil
	}
	hubDone := make(chan error, 4)
	var dials atomic.Int32
	first := &unreadCall{method: method, dropped: make(chan struct{})}
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: "https://org.example", RunnerID: row.ID.String(), PrivateKey: key, Hooks: hooks,
		Dial: func(ctx context.Context) (runnerwire.Conn, error) {
			org, run := runnerwire.Loopback()
			go func() { hubDone <- hub.Serve(ctx, org, row.ID) }()
			if dials.Add(1) == 1 {
				first.Conn = run
				first.armed.Store(true)
				return first, nil
			}
			return run, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() { done <- client.Connect(ctx) }()
	_ = awaitValue(t, ctx, online)
	if awaitValue(t, ctx, resumed) {
		t.Fatal("first session resumed")
	}
	waitOnline(t, ctx, link)
	var reply string
	if err := link.Call(ctx, "probe", nil, &reply); err != nil || reply != "reply" {
		t.Fatalf("call %q: %v", reply, err)
	}
	sub.mu.Lock()
	sub.caps = &runnerwire.Caps{LinuxVM: &runnerwire.VMCaps{Desktop: true}} // DialDesktop needs an offer
	sub.mu.Unlock()

	// The opening is admitted well before its cutoff and written; the link fails before the runner reads it.
	opened := make(chan error, 1)
	start := runner.WithStartCutoff(ctx, time.Now().Add(time.Minute))
	go func() { opened <- open(start, sub, placement.RunnerRefPrefix(row.ID.String())+"box") }()
	select {
	case <-first.dropped:
	case <-ctx.Done():
		t.Fatal("the opening's CALL never reached the runner's end of the link")
	}
	if err := awaitValue(t, ctx, opened); !errors.Is(err, runner.ErrRunnerOffline) {
		t.Fatalf("opening = %v, want offline", err)
	}
	_ = awaitValue(t, ctx, done)
	_ = awaitValue(t, ctx, hubDone)

	go func() { done <- client.Connect(ctx) }()
	p := awaitValue(t, ctx, online)
	if awaitValue(t, ctx, resumed) {
		t.Fatal("the hub resumed a session whose replay holds the unread opening")
	}
	waitOnline(t, ctx, link)
	if err := link.Call(ctx, "probe", nil, &reply); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // a replayed CALL's handler runs on its own goroutine
	if n := served.Load(); n != 0 {
		t.Fatalf("the runner received the unread %s CALL %d times after reconnecting", method, n)
	}
	p.Close()
	_ = awaitValue(t, ctx, done)
}
