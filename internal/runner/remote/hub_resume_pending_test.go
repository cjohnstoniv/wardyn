// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"slices"
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

// linkReaches waits until the link's session state satisfies is (or ctx ends).
func linkReaches(ctx context.Context, link *Link, is func(*Link) bool) {
	for ctx.Err() == nil {
		link.mu.Lock()
		ok := is(link)
		link.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// A teardown queued during a fresh session's admission, after the PENDING the session started with and
// after the org's last look at the queue (its judging of the final STATE page), is carried by no frame
// of that session: the session is ended at once, and the runner's next, fresh session carries it.
func TestHubQueuedDuringAdmissionReachesTheRunnerOnAFreshSession(t *testing.T) {
	// notReady closes once the kill's call has found the link not ready and turned to the queue.
	notReady := make(chan struct{})
	for _, tc := range []struct {
		name string
		// inPage issues the kill from the org's judging of the final STATE page, after its look at the queue.
		inPage bool
		// before issues it from the runner's side, just before its READY.
		before func(ctx context.Context, link *Link, kill func())
		// queue delays the kill's queue write: the org's database round trip.
		queue func(ctx context.Context, link *Link)
	}{
		{name: "after the final page was accepted", before: func(ctx context.Context, link *Link, kill func()) {
			linkReaches(ctx, link, func(l *Link) bool { return l.stated })
			kill()
		}},
		{name: "while the final page is judged", inPage: true},
		// The kill's call finds the link not ready; its queue write lands once the session is READY. READY
		// waits for that call: one made after it reaches the runner live on this session and queues nothing.
		{name: "as the session goes ready",
			before: func(ctx context.Context, _ *Link, kill func()) {
				go kill()
				select {
				case <-notReady:
				case <-ctx.Done():
				}
			},
			queue: func(ctx context.Context, link *Link) {
				close(notReady)
				linkReaches(ctx, link, func(l *Link) bool { return l.ready })
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			sub := New(row.ID.String(), link, Options{Queue: func(ctx context.Context, kind types.RunnerActionKind, ref string) error {
				if tc.queue != nil {
					tc.queue(ctx, link)
				}
				mu.Lock()
				defer mu.Unlock()
				open = append(open, runnerwire.PendingAction{ID: uuid.New(), Kind: kind, Ref: ref})
				return nil
			}})
			ref := placement.RunnerRefPrefix(row.ID.String()) + "sandbox"
			queued := make(chan error, 1)
			var once sync.Once
			kill := func() { once.Do(func() { queued <- sub.KillSandbox(ctx, ref) }) }
			pending := func(context.Context) ([]runnerwire.PendingAction, error) {
				mu.Lock()
				defer mu.Unlock()
				return append([]runnerwire.PendingAction(nil), open...), nil
			}
			config := HubConfig{OrgURL: "https://org.example", Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil },
				Admit: func(context.Context, types.Runner) (Admission, error) {
					return Admission{Substrate: sub, Link: link, Pending: pending,
						BeginInventory: func(context.Context, string) error { return nil },
						// As the org judges a final page: it is refused while an action is open.
						StatePage: func(ctx context.Context, _ string, _ runnerwire.StatePage) error {
							if open, err := pending(ctx); err != nil || len(open) != 0 {
								return errors.New("open action: the page is refused")
							}
							if tc.inPage {
								kill()
							}
							return nil
						},
						Event: func(_ context.Context, ev runnerwire.Event) error {
							var result runnerwire.ActionResult
							if ev.Kind == runnerwire.EventActionResult && runnerwire.Unmarshal(ev.Data, &result) == nil {
								mu.Lock()
								open = slices.DeleteFunc(open, func(a runnerwire.PendingAction) bool { return a.ID == result.ID })
								mu.Unlock()
							}
							return nil
						},
					}, nil
				},
			}
			hub := NewHub(ctx, config)
			defer hub.Close()
			hubDone := make(chan error, 2)
			online, resumed := make(chan *runnerwire.Peer, 2), make(chan bool, 2)
			delivered := make(chan []runnerwire.PendingAction, 2)
			hooks := sessionHooks(online, resumed)
			hooks.BeforeReady = func(ctx context.Context, _ *runnerwire.Peer, _ bool) error {
				if tc.before != nil {
					tc.before(ctx, link, kill)
				}
				return nil
			}
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
			if len(awaitValue(t, ctx, delivered)) != 0 {
				t.Fatal("the first session's PENDING was not empty")
			}
			if err := awaitValue(t, ctx, queued); !errors.Is(err, runner.ErrPendingOnRunner) {
				t.Fatalf("kill during admission = %v, want it queued for the runner", err)
			}
			// The session that could not carry the kill ends; one that stayed online would never deliver it.
			_ = awaitValue(t, ctx, done)
			_ = awaitValue(t, ctx, hubDone)
			go func() { done <- client.Connect(ctx) }()
			if actions := awaitValue(t, ctx, delivered); len(actions) != 1 || actions[0].Kind != types.RunnerActionKill || actions[0].Ref != ref {
				t.Fatalf("the next session's PENDING = %+v, want the queued kill", actions)
			}
			waitOnline(t, ctx, link)
		})
	}
}

// The same on a resumed session: an action queued just after the session read the queue is not carried
// by the resume, so the resumed session ends, and the runner's next one is fresh and carries it.
func TestHubResumeEndsWhenAnActionIsQueuedAfterItsPendingRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	var serving sync.WaitGroup
	defer func() { cancel(); serving.Wait() }()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	row := types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: federation.OrgURLSHA256("https://org.example"), State: types.RunnerClaimed}
	ref := placement.RunnerRefPrefix(row.ID.String()) + "sandbox"
	var mu sync.Mutex
	var open []runnerwire.PendingAction
	queueAfterRead := false
	link := &Link{}
	sub := New(row.ID.String(), link, Options{})
	config := HubConfig{OrgURL: "https://org.example", Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil },
		Admit: func(context.Context, types.Runner) (Admission, error) {
			return Admission{Substrate: sub, Link: link,
				// The resume's read finds the queue empty, and a kill lands right after it.
				Pending: func(context.Context) ([]runnerwire.PendingAction, error) {
					mu.Lock()
					defer mu.Unlock()
					out := append([]runnerwire.PendingAction(nil), open...)
					if queueAfterRead {
						queueAfterRead = false
						open = append(open, runnerwire.PendingAction{ID: uuid.New(), Kind: types.RunnerActionKill, Ref: ref})
					}
					return out, nil
				},
				BeginInventory: func(context.Context, string) error { return nil },
				StatePage:      func(context.Context, string, runnerwire.StatePage) error { return nil },
				Event:          func(context.Context, runnerwire.Event) error { return nil },
			}, nil
		},
	}
	hub := NewHub(ctx, config)
	defer hub.Close()
	hubDone := make(chan error, 3)
	online, resumed := make(chan *runnerwire.Peer, 3), make(chan bool, 3)
	delivered := make(chan []runnerwire.PendingAction, 3)
	hooks := sessionHooks(online, resumed)
	hooks.Pending = func(_ context.Context, _ *runnerwire.Peer, actions []runnerwire.PendingAction) error {
		delivered <- actions
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
	done := make(chan error, 3)
	go func() { done <- client.Connect(ctx) }()
	p := awaitValue(t, ctx, online)
	if awaitValue(t, ctx, resumed) || len(awaitValue(t, ctx, delivered)) != 0 {
		t.Fatal("first session was not a fresh session with an empty PENDING")
	}
	waitOnline(t, ctx, link)
	p.Close()
	_ = awaitValue(t, ctx, done)
	_ = awaitValue(t, ctx, hubDone)

	mu.Lock()
	queueAfterRead = true
	mu.Unlock()
	go func() { done <- client.Connect(ctx) }()
	// The resume cannot stay up: it never carried the kill.
	_ = awaitValue(t, ctx, done)
	_ = awaitValue(t, ctx, hubDone)
	go func() { done <- client.Connect(ctx) }()
	for {
		actions := awaitValue(t, ctx, delivered)
		if len(actions) == 1 && actions[0].Kind == types.RunnerActionKill && actions[0].Ref == ref {
			return
		}
		if len(actions) != 0 {
			t.Fatalf("PENDING = %+v, want the kill queued after the resume's read", actions)
		}
	}
}

// A teardown called while the runner is away, whose queue write lands only after the resumed session
// made both of its looks at the queue, is carried by nothing in that session: the session is ended, and
// the runner's next, fresh session carries it.
func TestHubResumeEndsWhenAnActionIsQueuedAfterBothItsPendingReads(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	var serving sync.WaitGroup
	defer func() { cancel(); serving.Wait() }()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	row := types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: federation.OrgURLSHA256("https://org.example"), State: types.RunnerClaimed}
	ref := placement.RunnerRefPrefix(row.ID.String()) + "sandbox"
	var mu sync.Mutex
	var open []runnerwire.PendingAction
	reads, writeAfter := 0, 0
	link := &Link{}
	sub := New(row.ID.String(), link, Options{Queue: func(ctx context.Context, kind types.RunnerActionKind, ref string) error {
		for ctx.Err() == nil {
			mu.Lock()
			if reads >= writeAfter {
				open = append(open, runnerwire.PendingAction{ID: uuid.New(), Kind: kind, Ref: ref})
				mu.Unlock()
				return nil
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
		}
		return ctx.Err()
	}})
	config := HubConfig{OrgURL: "https://org.example", Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil },
		Admit: func(context.Context, types.Runner) (Admission, error) {
			return Admission{Substrate: sub, Link: link,
				Pending: func(context.Context) ([]runnerwire.PendingAction, error) {
					mu.Lock()
					defer mu.Unlock()
					reads++
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
	hubDone := make(chan error, 3)
	online, resumed := make(chan *runnerwire.Peer, 3), make(chan bool, 3)
	delivered := make(chan []runnerwire.PendingAction, 3)
	hooks := sessionHooks(online, resumed)
	hooks.Pending = func(_ context.Context, _ *runnerwire.Peer, actions []runnerwire.PendingAction) error {
		delivered <- actions
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
	done := make(chan error, 3)
	go func() { done <- client.Connect(ctx) }()
	p := awaitValue(t, ctx, online)
	if awaitValue(t, ctx, resumed) || len(awaitValue(t, ctx, delivered)) != 0 {
		t.Fatal("first session was not a fresh session with an empty PENDING")
	}
	waitOnline(t, ctx, link)
	p.Close()
	_ = awaitValue(t, ctx, done)
	_ = awaitValue(t, ctx, hubDone)

	// The kill finds the runner away; its queue write waits out the resume's two looks at the queue.
	mu.Lock()
	writeAfter = reads + 2
	mu.Unlock()
	queued := make(chan error, 1)
	go func() { queued <- sub.KillSandbox(ctx, ref) }()
	go func() { done <- client.Connect(ctx) }()
	if err := awaitValue(t, ctx, queued); !errors.Is(err, runner.ErrPendingOnRunner) {
		t.Fatalf("kill while the runner was away = %v, want it queued for the runner", err)
	}
	// The resume cannot stay up: it never carried the kill.
	_ = awaitValue(t, ctx, done)
	_ = awaitValue(t, ctx, hubDone)
	go func() { done <- client.Connect(ctx) }()
	for {
		actions := awaitValue(t, ctx, delivered)
		if len(actions) == 1 && actions[0].Kind == types.RunnerActionKill && actions[0].Ref == ref {
			return
		}
		if len(actions) != 0 {
			t.Fatalf("PENDING = %+v, want the kill queued after the resume's reads", actions)
		}
	}
}
