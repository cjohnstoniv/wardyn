// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"slices"
	"strings"
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

// kblFixture is one org hub and one runner identity whose sessions record, in order, what the runner saw: each
// PENDING (with its kinds), its STATE inventory, READY, and every LEASE.
type kblFixture struct {
	ctx     context.Context
	hub     *Hub
	sub     *Substrate
	link    *Link
	row     types.Runner
	key     ed25519.PrivateKey
	orgURL  string
	serving sync.WaitGroup
	online  chan *runnerwire.Peer
	resumed chan bool

	mu   sync.Mutex
	open []runnerwire.PendingAction
	seen []string
	// hang makes the runner never answer a kill call (a session that stops answering without a close).
	hang chan struct{}
}

func newKBL(t *testing.T) *kblFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &kblFixture{ctx: ctx, key: key, orgURL: "https://org.example", link: &Link{},
		online: make(chan *runnerwire.Peer, 4), resumed: make(chan bool, 4)}
	f.row = types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: federation.OrgURLSHA256(f.orgURL), State: types.RunnerClaimed}
	f.sub = New(f.row.ID.String(), f.link, Options{Queue: func(ctx context.Context, kind types.RunnerActionKind, ref string) error {
		if err := ctx.Err(); err != nil {
			return err // as production's queue write fails on a done context
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.open = append(f.open, runnerwire.PendingAction{ID: uuid.New(), Kind: kind, Ref: ref})
		return nil
	}})
	f.hub = NewHub(ctx, HubConfig{OrgURL: f.orgURL, Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return f.row, nil },
		Admit: func(context.Context, types.Runner) (Admission, error) {
			return Admission{Substrate: f.sub, Link: f.link,
				Pending: func(context.Context) ([]runnerwire.PendingAction, error) {
					f.mu.Lock()
					defer f.mu.Unlock()
					return slices.Clone(f.open), nil
				},
				BeginInventory: func(context.Context, string) error { return nil },
				StatePage:      func(context.Context, string, runnerwire.StatePage) error { return nil },
				Event: func(_ context.Context, ev runnerwire.Event) error {
					if ev.Kind != runnerwire.EventActionResult {
						return nil
					}
					var res runnerwire.ActionResult
					if err := json.Unmarshal(ev.Data, &res); err == nil {
						f.mu.Lock()
						f.open = slices.DeleteFunc(f.open, func(a runnerwire.PendingAction) bool { return a.ID == res.ID })
						f.mu.Unlock()
					}
					return nil
				},
			}, nil
		},
	})
	t.Cleanup(func() {
		if f.hang != nil {
			close(f.hang)
		}
		cancel()
		f.hub.Close()
		f.serving.Wait()
	})
	return f
}

func (f *kblFixture) note(s string) {
	f.mu.Lock()
	f.seen = append(f.seen, s)
	f.mu.Unlock()
}

// session is what the runner saw since the last call, and starts the next record.
func (f *kblFixture) session() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.seen
	f.seen = nil
	return out
}

// client is a new runner process for the fixture's identity: its first connect is always fresh.
func (f *kblFixture) client(t *testing.T) *runnerwire.Client {
	t.Helper()
	hooks := sessionHooks(f.online, f.resumed)
	hooks.Call = func(ctx context.Context, _ *runnerwire.Peer, _ uint32, method string, _ json.RawMessage) (any, error) {
		if method == runnerwire.MethodKill && f.hang != nil {
			select {
			case <-f.hang:
			case <-ctx.Done():
			}
			return nil, errors.New("never answered")
		}
		return runnerwire.TeardownResult{}, nil
	}
	hooks.Pending = func(ctx context.Context, p *runnerwire.Peer, actions []runnerwire.PendingAction) error {
		kinds := make([]string, 0, len(actions))
		for _, a := range actions {
			kinds = append(kinds, string(a.Kind))
			data, err := runnerwire.Marshal(runnerwire.ActionResult{ID: a.ID, Outcome: types.RunnerActionSuccess})
			if err == nil {
				err = p.Event(ctx, runnerwire.Event{Kind: runnerwire.EventActionResult, Data: data})
			}
			if err != nil {
				return err
			}
		}
		f.note("pending:" + strings.Join(kinds, ","))
		return nil
	}
	hooks.StatePages = func(ctx context.Context, send func(runnerwire.StatePage) error) error {
		f.note("state")
		return emptyInventory(ctx, send)
	}
	hooks.Lease = func(_ context.Context, l runnerwire.Lease) error {
		f.note("lease:" + l.RunID.String())
		return nil
	}
	online := hooks.Online
	hooks.Online = func(p *runnerwire.Peer, resumed bool) {
		f.note("ready")
		online(p, resumed)
	}
	c, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: f.orgURL, RunnerID: f.row.ID.String(), PrivateKey: f.key, Hooks: hooks,
		Dial: func(ctx context.Context) (runnerwire.Conn, error) {
			org, run := runnerwire.Loopback()
			f.serving.Go(func() { _ = f.hub.Serve(ctx, org, f.row.ID) })
			return run, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// connect runs one session of c until it is online, and answers its peer, whether it resumed, and its end.
func (f *kblFixture) connect(t *testing.T, c *runnerwire.Client) (*runnerwire.Peer, bool, <-chan error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.Connect(f.ctx) }()
	p := awaitValue(t, f.ctx, f.online)
	resumed := awaitValue(t, f.ctx, f.resumed)
	waitOnline(t, f.ctx, f.link)
	return p, resumed, done
}

// ended waits for a session's Connect to return and then for the hub's Serve of it: the runner hears its link close
// before that Serve lets go of the session slot, and a connection that finds the slot held is refused. A real runner
// waits its reconnect delay first; the test waits for the Serve.
func (f *kblFixture) ended(t *testing.T, done <-chan error) {
	t.Helper()
	_ = awaitValue(t, f.ctx, done)
	served := make(chan struct{})
	go func() {
		f.serving.Wait()
		close(served)
	}()
	awaitValue(t, f.ctx, served)
}

// requireKillBeforeReady fails unless a session's record is a fresh PENDING carrying kill, then STATE, then READY, with
// no LEASE.
func requireKillBeforeReady(t *testing.T, seen []string) {
	t.Helper()
	pending := slices.IndexFunc(seen, func(s string) bool { return strings.HasPrefix(s, "pending:") })
	state, ready := slices.Index(seen, "state"), slices.Index(seen, "ready")
	if pending < 0 || !strings.Contains(seen[pending], string(types.RunnerActionKill)) || state < pending || ready < state {
		t.Fatalf("session saw %v; want PENDING carrying the kill, then STATE, then READY", seen)
	}
	if slices.ContainsFunc(seen, func(s string) bool { return strings.HasPrefix(s, "lease:") }) {
		t.Fatalf("session saw %v; want no LEASE", seen)
	}
}

// A kill queued while the runner was offline is in its next fresh session's PENDING, applied before its STATE,
// READY follows, and no LEASE is sent on the session.
func TestKillsBeforeLeases_FreshSession(t *testing.T) {
	f := newKBL(t)
	p, _, done := f.connect(t, f.client(t))
	p.Close()
	f.ended(t, done)
	f.session()

	ref := placement.RunnerRefPrefix(f.row.ID.String()) + "sandbox"
	if err := f.sub.KillSandbox(f.ctx, ref); !errors.Is(err, runner.ErrPendingOnRunner) {
		t.Fatalf("kill while offline = %v, want it queued for the runner", err)
	}
	if _, resumed, _ := f.connect(t, f.client(t)); resumed {
		t.Fatal("a new runner process resumed")
	}
	requireKillBeforeReady(t, f.session())
}

// A kill queued during a short cut ends the resumed session: the reconnect inside the resume window is fresh,
// carries it, and no LEASE is sent between.
func TestKillsBeforeLeases_ResumedSession(t *testing.T) {
	f := newKBL(t)
	c := f.client(t)
	p, _, done := f.connect(t, c)
	p.Close()
	f.ended(t, done)
	f.session()

	ref := placement.RunnerRefPrefix(f.row.ID.String()) + "sandbox"
	if err := f.sub.KillSandbox(f.ctx, ref); !errors.Is(err, runner.ErrPendingOnRunner) {
		t.Fatalf("kill during the cut = %v, want it queued for the runner", err)
	}
	if _, resumed, _ := f.connect(t, c); resumed {
		t.Fatal("the reconnect resumed the session and skipped the queued kill")
	}
	requireKillBeforeReady(t, f.session())
}

// A kill call whose session stops answering, without a close, is delivered on the next fresh session.
func TestKillsBeforeLeases_HalfOpenLink(t *testing.T) {
	f := newKBL(t)
	f.hang = make(chan struct{})
	p, _, done := f.connect(t, f.client(t))
	f.session()

	ref := placement.RunnerRefPrefix(f.row.ID.String()) + "sandbox"
	callCtx, cancel := context.WithTimeout(f.ctx, 2*time.Second)
	err := f.sub.KillSandbox(callCtx, ref)
	cancel()
	if !errors.Is(err, runner.ErrPendingOnRunner) {
		t.Fatalf("a kill the runner never answered = %v, want it queued for the runner's next session", err)
	}
	p.Close() // the dead link is noticed and the session ends
	f.ended(t, done)
	f.session()

	if _, resumed, _ := f.connect(t, f.client(t)); resumed {
		t.Fatal("a new runner process resumed")
	}
	requireKillBeforeReady(t, f.session())
}
