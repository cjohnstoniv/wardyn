// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A teardown queued while the link is down must reach the runner when it
// reconnects inside the resume window, not wait for a later fresh session.
func TestHubResumeDeliversTeardownQueuedWhileOffline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	var serving sync.WaitGroup
	defer func() { cancel(); serving.Wait() }()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	row := types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: federation.OrgURLSHA256("https://org.example"), State: types.RunnerClaimed}
	var mu sync.Mutex
	var open []runnerwire.PendingAction
	link := &Link{}
	sub := New(row.ID.String(), link, Options{Queue: func(_ context.Context, kind types.RunnerActionKind, ref string) error {
		mu.Lock()
		defer mu.Unlock()
		open = append(open, runnerwire.PendingAction{ID: uuid.New(), Kind: kind, Ref: ref})
		return nil
	}})
	config := HubConfig{OrgURL: "https://org.example", Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil },
		Admit: func(context.Context, types.Runner) (Admission, error) {
			return Admission{Substrate: sub, Link: link,
				Pending: func(context.Context) ([]runnerwire.PendingAction, error) {
					mu.Lock()
					defer mu.Unlock()
					return append([]runnerwire.PendingAction(nil), open...), nil
				},
				BeginInventory: func(context.Context, string) error { return nil },
				StatePage:      func(context.Context, string, runnerwire.StatePage) error { return nil },
				Event:          func(context.Context, runnerwire.Event) error { return nil },
			}, nil
		},
	}
	hub := NewHub(ctx, config)
	defer hub.Close()
	hubDone := make(chan error, 2)
	online, resumed := make(chan *runnerwire.Peer, 2), make(chan bool, 2)
	delivered := make(chan []runnerwire.PendingAction, 2)
	hooks := sessionHooks(online, resumed)
	hooks.Pending = func(ctx context.Context, p *runnerwire.Peer, actions []runnerwire.PendingAction) error {
		delivered <- actions
		for _, action := range actions {
			data, err := runnerwire.Marshal(runnerwire.ActionResult{ID: action.ID, Outcome: "applied"})
			if err == nil {
				err = p.Event(ctx, runnerwire.Event{Kind: runnerwire.EventActionResult, Data: data})
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: config.OrgURL, RunnerID: row.ID.String(), PrivateKey: key, Hooks: hooks,
		Dial: func(ctx context.Context) (runnerwire.Conn, error) {
			org, run := runnerwire.Loopback()
			serving.Go(func() { hubDone <- hub.Serve(ctx, org, row.ID) })
			return run, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() { done <- client.Connect(ctx) }()
	p := awaitValue(t, ctx, online)
	if awaitValue(t, ctx, resumed) || len(awaitValue(t, ctx, delivered)) != 0 {
		t.Fatal("first session was not a fresh session with an empty PENDING")
	}
	waitOnline(t, ctx, link)
	p.Close()
	_ = awaitValue(t, ctx, done)
	_ = awaitValue(t, ctx, hubDone)

	ref := placement.RunnerRefPrefix(row.ID.String()) + "sandbox"
	if err := sub.KillSandbox(ctx, ref); !errors.Is(err, runner.ErrPendingOnRunner) {
		t.Fatalf("kill while offline = %v, want it queued for the runner", err)
	}
	go func() { done <- client.Connect(ctx) }()
	_ = awaitValue(t, ctx, online)
	if awaitValue(t, ctx, resumed) {
		t.Fatal("reconnect resumed the session and skipped the queued kill")
	}
	select {
	case actions := <-delivered:
		if len(actions) != 1 || actions[0].Kind != types.RunnerActionKill || actions[0].Ref != ref {
			t.Fatalf("PENDING = %+v, want the queued kill", actions)
		}
	default:
		t.Fatal("queued kill did not reach the runner")
	}
	waitOnline(t, ctx, link)
}
