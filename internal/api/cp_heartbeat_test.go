// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// outageWindowsFake is store.OutageWindows in memory: it counts beats and link-window closes, and fails them on
// demand.
type outageWindowsFake struct {
	mu       sync.Mutex
	beats    int
	closes   int
	beatErr  error
	closeErr error
}

func (f *outageWindowsFake) BeatControlPlane(context.Context, time.Duration) (*store.OutageWindow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beats++
	return nil, f.beatErr
}

func (f *outageWindowsFake) OpenRunnerLinkWindow(context.Context, uuid.UUID, string, time.Duration) (bool, error) {
	return false, nil
}

func (f *outageWindowsFake) CloseRunnerLinkWindow(context.Context, uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return f.closeErr
}

func (f *outageWindowsFake) HealRunnerLinkWindows(context.Context) (int, error) { return 0, nil }

func (f *outageWindowsFake) OpenServerOutageWindow(context.Context, time.Duration, map[string]any) error {
	return nil
}

func (f *outageWindowsFake) CloseServerOutageWindow(context.Context) error { return nil }

func (f *outageWindowsFake) PruneOutageWindows(context.Context, time.Duration) (int, error) {
	return 0, nil
}

func (f *outageWindowsFake) OutageWindowCovers(context.Context, *uuid.UUID, types.Placement, time.Time) (store.OutageWindow, bool, error) {
	return store.OutageWindow{}, false, nil
}

func (f *outageWindowsFake) beatCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.beats
}

func (f *outageWindowsFake) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closes
}

func (f *outageWindowsFake) failBeats(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beatErr = err
}

// outageWindowsStore is a Store whose only working part is its outage windows.
type outageWindowsStore struct {
	store.Store
	*outageWindowsFake
}

// A replica with no local runner beats too: the heartbeat starts before ReconcileOnBoot's no-runner return.
func TestReconcileOnBoot_HeartbeatStartsWithoutRunner(t *testing.T) {
	ow := &outageWindowsFake{}
	srv := testServer(t, baseTestConfig(newHarness(t), outageWindowsStore{outageWindowsFake: ow}))
	if srv.cfg.Runner != nil {
		t.Fatal("the server has a local runner")
	}
	if err := srv.ReconcileOnBoot(context.Background()); err != nil {
		t.Fatalf("ReconcileOnBoot: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for ow.beatCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if ow.beatCount() == 0 {
		t.Fatal("a replica with no local runner never beat")
	}
}

// The loop beats once at start, long before its first tick.
func TestRunCPHeartbeat_BeatsAtStart(t *testing.T) {
	ow := &outageWindowsFake{}
	srv := &Server{cfg: Config{Store: outageWindowsStore{outageWindowsFake: ow}}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.runCPHeartbeat(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(time.Second)
	for ow.beatCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := ow.beatCount(); n != 1 {
		t.Fatalf("beats in the first second = %d, want 1", n)
	}
}

// A failing beat logs one warning, then nothing until a beat succeeds, so an outage cannot flood the log.
func TestRunCPHeartbeat_LogsOnceWhileFailing(t *testing.T) {
	logs := captureSlog(t)
	const failed = "control-plane heartbeat failed"
	synctest.Test(t, func(t *testing.T) {
		ow := &outageWindowsFake{beatErr: errors.New("database unreachable")}
		srv := &Server{cfg: Config{Store: outageWindowsStore{outageWindowsFake: ow}}}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { srv.runCPHeartbeat(ctx); close(done) }()
		time.Sleep(4*cpHeartbeatEvery - time.Second) // passes at 0, 10, 20 and 30 s, all failing
		synctest.Wait()
		if n, warned := ow.beatCount(), strings.Count(logs.String(), failed); n != 4 || warned != 1 {
			t.Fatalf("after 4 failing passes: beats %d, warnings %d; want 4 and 1", n, warned)
		}
		ow.failBeats(nil)
		time.Sleep(cpHeartbeatEvery) // the pass at 40 s succeeds
		synctest.Wait()
		ow.failBeats(errors.New("database unreachable again"))
		time.Sleep(cpHeartbeatEvery) // the pass at 50 s fails after a success
		synctest.Wait()
		if warned := strings.Count(logs.String(), failed); warned != 2 {
			t.Fatalf("warnings after a success and a new failure = %d, want 2", warned)
		}
		cancel()
		<-done
	})
}

// A store with no outage windows runs no loop.
func TestRunCPHeartbeat_NoOutageWindowsStore(t *testing.T) {
	srv := &Server{cfg: Config{Store: struct{ store.Store }{}}}
	done := make(chan struct{})
	go func() { srv.runCPHeartbeat(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runCPHeartbeat kept running over a store with no outage windows")
	}
}

// failCloseRunnerStore is the registration fake whose link-window close fails.
type failCloseRunnerStore struct {
	*runnerRegistrationFake
	*outageWindowsFake
}

// A failed close of the runner's link window refuses the session in the production Connected callback, so no
// connected runner keeps a window opened before its session began.
func TestRunnerLink_FailedCloseRefusesSession(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	row := f.addRunner(srv, "alice", "laptop", types.RunnerClaimed, time.Hour)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	row.PublicKey = pub
	f.lock.Lock()
	f.rows[row.ID] = row
	f.lock.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.cfg.RunnerHub.Admit = func(context.Context, types.Runner) (remote.Admission, error) {
		link := &remote.Link{}
		return remote.Admission{Substrate: remote.New(row.ID.String(), link, remote.Options{}), Link: link,
			Pending:        func(context.Context) ([]runnerwire.PendingAction, error) { return nil, nil },
			BeginInventory: func(context.Context, string) error { return nil },
			StatePage:      func(context.Context, string, runnerwire.StatePage) error { return nil },
			Event:          func(context.Context, runnerwire.Event) error { return nil },
		}, nil
	}
	ow := &outageWindowsFake{closeErr: errors.New("database unreachable")}
	hub := srv.runnerHub(failCloseRunnerStore{runnerRegistrationFake: f, outageWindowsFake: ow})
	ready := make(chan struct{}, 1)
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: srv.cfg.RunnerOrgURL, RunnerID: row.ID.String(), PrivateKey: key,
		Hooks: runnerwire.RunnerHooks{
			Call:    func(context.Context, *runnerwire.Peer, uint32, string, json.RawMessage) (any, error) { return nil, nil },
			Open:    func(*runnerwire.Peer, *runnerwire.Stream, runnerwire.Open) error { return runnerwire.ErrNotReady },
			Clock:   func(runnerwire.Hello, time.Time) error { return nil },
			Pending: func(context.Context, *runnerwire.Peer, []runnerwire.PendingAction) error { return nil },
			StatePages: func(_ context.Context, send func(runnerwire.StatePage) error) error {
				return send(runnerwire.StatePage{InventoryID: uuid.New(), Final: true})
			},
			Replay:      func(context.Context, *runnerwire.Peer) error { return nil },
			BeforeReady: func(context.Context, *runnerwire.Peer, bool) error { return nil },
			Lease:       func(context.Context, runnerwire.Lease) error { return nil },
			Revoked:     func(context.Context) error { return nil },
			Offline:     func(context.Context) {},
			Online:      func(*runnerwire.Peer, bool) { ready <- struct{}{} },
		},
		Dial: func(context.Context) (runnerwire.Conn, error) {
			org, run := runnerwire.Loopback()
			go func() { _ = hub.Serve(ctx, org, row.ID) }()
			return run, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() { ended <- client.Connect(ctx) }()
	select {
	case <-ready:
		t.Fatal("a session whose link-window close failed reached READY")
	case err := <-ended:
		if err == nil {
			t.Fatal("the refused session ended with no error")
		}
	case <-ctx.Done():
		t.Fatal("the session neither reached READY nor ended")
	}
	if n := ow.closeCount(); n != 1 {
		t.Fatalf("link-window closes = %d, want 1", n)
	}
	if hub.Online(row.ID) {
		t.Fatal("the refused runner reads online")
	}
}
