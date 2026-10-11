// Copyright 2025 The Wardyn Authors
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
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestHubLocalAdmissionPrecedesFreshAndResumedDispatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	row := types.Runner{ID: uuid.New(), PublicKey: pub, State: types.RunnerClaimed, OrgURLSHA256: federation.OrgURLSHA256("https://org.example")}
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
	entered, release := make(chan bool, 2), make(chan struct{})
	var admitted atomic.Bool
	var executions atomic.Int32
	hooks := sessionHooks(online, resumed)
	hooks.BeforeReady = func(ctx context.Context, _ *runnerwire.Peer, resume bool) error {
		entered <- resume
		select {
		case <-release:
			admitted.Store(true)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	hooks.Call = func(_ context.Context, _ *runnerwire.Peer, _ uint32, method string, _ json.RawMessage) (any, error) {
		if method == runnerwire.MethodExec || method == runnerwire.MethodStart {
			executions.Add(1)
		}
		if !admitted.Load() {
			return nil, errors.New("dispatch raced durable local admission")
		}
		return "admitted", nil
	}
	hooks.Offline = func(context.Context) { admitted.Store(false) }
	hubDone := make(chan error, 2)
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: "https://org.example", RunnerID: row.ID.String(), PrivateKey: key, Hooks: hooks,
		Dial: func(context.Context) (runnerwire.Conn, error) {
			org, run := runnerwire.Loopback()
			go func() { hubDone <- hub.Serve(ctx, org, row.ID) }()
			return run, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	for _, wantResume := range []bool{false, true} {
		go func() { done <- client.Connect(ctx) }()
		if got := awaitValue(t, ctx, entered); got != wantResume {
			t.Fatalf("admission resumed=%v want %v", got, wantResume)
		}
		if admitted.Load() {
			t.Fatal("previous connection retained admission")
		}
		if !wantResume && link.Online() {
			t.Fatal("fresh READY preceded local admission")
		}
		callDone := make(chan error, 1)
		if wantResume {
			waitOnline(t, ctx, link)
			// The org resumes immediately. Its first CALL can already be queued
			// while the runner is still establishing durable local admission.
			go func() { callDone <- link.Call(ctx, "probe", nil, nil) }()
		}
		select {
		case release <- struct{}{}:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		peer := awaitValue(t, ctx, online)
		if awaitValue(t, ctx, resumed) != wantResume {
			t.Fatal("wrong session phase")
		}
		waitOnline(t, ctx, link)
		ref := placement.RunnerRefPrefix(row.ID.String()) + "sandbox"
		if _, err := sub.Exec(ctx, ref, []string{"agent"}); err == nil {
			t.Fatal("authenticated substrate dispatched without a generation hook")
		}
		if err := sub.StartSandbox(ctx, ref); err == nil || executions.Load() != 0 {
			t.Fatal("missing generation hook reached runner", err)
		}
		if wantResume {
			if err := awaitValue(t, ctx, callDone); err != nil {
				t.Fatal(err)
			}
		}
		var reply string
		if err := link.Call(ctx, "probe", nil, &reply); err != nil || reply != "admitted" {
			t.Fatalf("dispatch=%q error=%v", reply, err)
		}
		peer.Close()
		awaitValue(t, ctx, done)
		awaitValue(t, ctx, hubDone)
		if admitted.Load() {
			t.Fatal("disconnect retained local admission")
		}
	}
}
