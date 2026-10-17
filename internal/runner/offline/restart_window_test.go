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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// orgQueue is the organisation's durable pending queue as a runner meets it: an action stays open
// until its action_result arrives, and every fresh session carries what is still open.
type orgQueue struct {
	mu      sync.Mutex
	open    []runnerwire.PendingAction
	results map[uuid.UUID]runnerwire.ActionResult
}

func (q *orgQueue) pending(context.Context) ([]runnerwire.PendingAction, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.open), nil
}

func (q *orgQueue) event(_ context.Context, ev runnerwire.Event) error {
	if ev.Kind != runnerwire.EventActionResult {
		return nil
	}
	var r runnerwire.ActionResult
	if err := runnerwire.Unmarshal(ev.Data, &r); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.results[r.ID] = r
	q.open = slices.DeleteFunc(q.open, func(a runnerwire.PendingAction) bool { return a.ID == r.ID })
	return nil
}

// TestRunnerComesBackAfterAControlPlaneRestart replaces the control plane under a connected runner
// with a live run: the old process drops the link, the WebSocket handshake answers 503 while the
// new one starts, and the new one's boot pass has queued teardown for a run whose local record the
// runner lost (a kill, then the end every boot queues for a terminal run that kept its sandbox ref).
// The runner must reach READY again on its own, keep the live run, and keep the lost run's record
// containment-only.
func TestRunnerComesBackAfterAControlPlaneRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	row := types.Runner{ID: uuid.New(), PublicKey: pub, State: types.RunnerClaimed, Owner: "runner-owner"}
	sub := &localSub{status: runner.Status{State: types.RunRunning}, agent: runner.Status{State: types.RunRunning}}
	// The substrate can end a sandbox, so an end that got through would run (as a kill here).
	m, err := New(t.TempDir(), row.ID, endingSub{sub}, uuid.UUID.String, noOutput, noErase)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	// phase 0: the old control plane; 1: its replacement is starting (503); 2: the new one.
	var phase atomic.Int32
	hubs := make([]*remote.Hub, 3)
	links := []*remote.Link{{}, nil, {}}
	queues := []*orgQueue{{results: map[uuid.UUID]runnerwire.ActionResult{}}, nil, {results: map[uuid.UUID]runnerwire.ActionResult{}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := phase.Load()
		if p == 1 {
			http.Error(w, "control plane restarting", http.StatusServiceUnavailable)
			return
		}
		if c, err := websocket.Accept(w, r, nil); err == nil {
			_ = hubs[p].Serve(ctx, runnerwire.NewWebSocket(c), row.ID)
		}
	}))
	t.Cleanup(server.Close)
	row.OrgURLSHA256 = federation.OrgURLSHA256(server.URL)
	for _, p := range []int{0, 2} {
		link, q := links[p], queues[p]
		orgSub := remote.New(row.ID.String(), link, remote.Options{})
		hubs[p] = remote.NewHub(ctx, remote.HubConfig{OrgURL: server.URL, Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil }, Admit: func(context.Context, types.Runner) (remote.Admission, error) {
			return remote.Admission{Substrate: orgSub, Link: link, Pending: q.pending, BeginInventory: func(context.Context, string) error { return nil },
				StatePage: func(context.Context, string, runnerwire.StatePage) error { return nil }, Event: q.event}, nil
		}})
		t.Cleanup(hubs[p].Close)
	}

	var orgTime atomic.Pointer[time.Time]
	online := make(chan struct{}, 4)
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: server.URL, RunnerID: row.ID.String(), PrivateKey: key, Hooks: runnerwire.RunnerHooks{
		Call: func(context.Context, *runnerwire.Peer, uint32, string, json.RawMessage) (any, error) {
			return true, nil
		},
		Open: func(*runnerwire.Peer, *runnerwire.Stream, runnerwire.Open) error { return runnerwire.ErrNotReady },
		Clock: func(h runnerwire.Hello, received time.Time) error {
			at := h.ServerTime
			orgTime.Store(&at)
			return m.Clock(h, received)
		},
		Pending: m.Pending, StatePages: m.StatePages, Replay: m.Replay, BeforeReady: m.BeforeReady, Lease: m.Lease, Revoked: m.Revoked, Offline: m.Offline,
		Online: func(p *runnerwire.Peer, _ bool) { m.Online(p); online <- struct{}{} },
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Client.Run's reconnect loop without its backoff: each attempt's error is reported.
	attempts := make(chan error, 64)
	go func() {
		for ctx.Err() == nil {
			err := client.Connect(ctx)
			select {
			case attempts <- err:
			case <-ctx.Done():
			}
		}
	}()
	select {
	case <-online:
	case err := <-attempts:
		t.Fatalf("first connection: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	live := uuid.New()
	lease := func(id uuid.UUID) error {
		at := *orgTime.Load()
		return m.Lease(ctx, runnerwire.Lease{RunID: id, TokenRenewedAt: at, EndsAt: at.Add(3 * time.Hour)})
	}
	if err := lease(live); err != nil {
		t.Fatal(err)
	}
	if err := m.BeforeCreate(ctx, runner.SandboxSpec{RunID: live}); err != nil {
		t.Fatal(err)
	}
	if err := m.Created(ctx, runner.SandboxSpec{RunID: live}, live.String()); err != nil {
		t.Fatal(err)
	}

	// The pod is replaced: the old process goes, and the handshake answers 503 while the new one starts.
	phase.Store(1)
	hubs[0].Close()
	for unavailable := 0; unavailable < 2; {
		select {
		case err := <-attempts:
			if err != nil && strings.Contains(err.Error(), "503") {
				unavailable++
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	lost := uuid.New()
	ref := placement.RunnerRefPrefix(row.ID.String()) + lost.String()
	kill := runnerwire.PendingAction{ID: uuid.New(), RunID: lost, Ref: ref, Kind: types.RunnerActionKill}
	end := runnerwire.PendingAction{ID: uuid.New(), RunID: lost, Ref: ref, Kind: types.RunnerActionEnd}
	queues[2].open = []runnerwire.PendingAction{kill, end}
	phase.Store(2)

	// Attempts already under way during the restart still meet the 503; three failures after it is
	// back is a runner that is not coming back.
	var failures []string
	for back := false; !back; {
		select {
		case <-online:
			back = true
		case err := <-attempts:
			if err != nil && !strings.Contains(err.Error(), "503") {
				if failures = append(failures, err.Error()); len(failures) == 3 {
					t.Fatalf("the runner never came back after the control plane did: %q", failures)
				}
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}

	q := queues[2]
	q.mu.Lock()
	open, killed, ended := len(q.open), q.results[kill.ID], q.results[end.ID]
	q.mu.Unlock()
	if open != 0 || killed.Outcome != types.RunnerActionSuccess || ended.Outcome != types.RunnerActionRefused || !strings.Contains(ended.Reason, "containment-only") {
		t.Fatalf("queue left open=%d kill=%+v end=%+v", open, killed, ended)
	}
	// The lost run stays containment-only: no lease, no execution, and the end never ran.
	m.mu.Lock()
	record := m.state.Runs[lost]
	m.mu.Unlock()
	if !record.ContainmentOnly || sub.kills != 1 {
		t.Fatalf("an end ran on the lost run, or its record changed: kills %d, record %+v", sub.kills, record)
	}
	if err := lease(lost); err == nil {
		t.Fatal("a containment-only record took a lease")
	}
	for _, method := range []string{runnerwire.MethodExec, runnerwire.MethodStart, runnerwire.MethodReplaceProxy} {
		if err := m.BeforeRefCall(ctx, method, lost.String(), uuid.New()); err == nil {
			t.Fatalf("a containment-only record admitted %s", method)
		}
	}
	// The live run is still held, its proxy untouched, and the new control plane can lease it again.
	m.mu.Lock()
	held := m.state.Runs[live]
	m.mu.Unlock()
	if !m.HoldsSandbox(live.String()) || held.ProxyStoppedAt != nil || held.Containment != nil {
		t.Fatalf("live run after the restart: %+v", held)
	}
	if err := lease(live); err != nil {
		t.Fatalf("the live run could not be leased again: %v", err)
	}
}
