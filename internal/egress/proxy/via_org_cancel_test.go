// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A forwarded request whose stream the control plane ends (a run end, a release or an idle release all
// cancel the request's context) while its upload has stopped arriving returns at once, not when the
// body bound runs out: the handler returns, the inspection budget it charged is released, and the
// engine then closes. One case for each entry that reads a forwarded body.
func TestViaOrg_CancelledStreamEndsABlockedBodyRead(t *testing.T) {
	prev := mitmReadTimeout
	mitmReadTimeout = 10 * time.Minute // far past the waits below: only the cancel can end the read
	t.Cleanup(func() { mitmReadTimeout = prev })
	for _, tc := range []struct {
		name, path string
		start      func(f *viaOrgFixture) string
		mutate     func(*Config)
		serve      func(e *ViaOrgEngine, w http.ResponseWriter, r *http.Request)
	}{
		{"injected model upload", "/v1/messages", func(*viaOrgFixture) string { return `{"model":` },
			func(c *Config) {
				c.Policy.AllowedDomains = append(c.Policy.AllowedDomains, anthropicHost)
				c.AllowedModels = &[]string{"model-a"}
			},
			func(e *ViaOrgEngine, w http.ResponseWriter, r *http.Request) {
				r.Host = anthropicHost
				e.ServeInjected(w, r, anthropicHost)
			}},
		{"brokered push", "/wardyn/gh/octocat/hello-world.git/git-receive-pack",
			func(f *viaOrgFixture) string {
				return pkt(zeroOID+" "+someOID+" "+BranchNSPrefix(f.runID)+"work"+firstCaps) + "0000" + "PACK"
			},
			func(c *Config) { c.Policy.PushRules = &types.PushRulesSpec{DenyPaths: []string{"nonexistent/**"}} },
			func(e *ViaOrgEngine, w http.ResponseWriter, r *http.Request) { e.ServeBroker(w, r) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newViaOrgFixture(t, nil, tc.mutate)
			p := f.engine.srv.proxy
			stream, end := context.WithCancel(context.Background())
			defer end()
			served := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(served)
				tc.serve(f.engine, w, r.WithContext(stream))
			}))
			defer srv.Close()
			body, stalled := io.Pipe()
			defer stalled.Close()
			req, err := http.NewRequest(http.MethodPost, srv.URL+tc.path, body)
			if err != nil {
				t.Fatal(err)
			}
			go func() {
				if resp, err := srv.Client().Do(req); err == nil {
					_ = resp.Body.Close()
				}
			}()
			if _, err := stalled.Write([]byte(tc.start(f))); err != nil {
				t.Fatal(err)
			}
			b := p.scan.init()
			for deadline := time.Now().Add(10 * time.Second); b.retained.TryAcquire(b.limit); {
				b.retained.Release(b.limit)
				if time.Now().After(deadline) {
					t.Fatal("the request never charged the inspection budget: its body was not being read")
				}
				time.Sleep(10 * time.Millisecond)
			}
			end()
			select {
			case <-served:
			case <-time.After(5 * time.Second):
				t.Fatal("the stream ended and the handler was still reading a body that stopped arriving")
			}
			if !b.retained.TryAcquire(b.limit) {
				t.Fatal("the handler returned with its inspection charge still held")
			}
			b.retained.Release(b.limit)
			closed := make(chan error, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				closed <- f.engine.Close(ctx)
			}()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatalf("the engine's close: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the engine did not close after its stream ended")
			}
			if hits, _, _ := f.up.seen(); hits != 0 {
				t.Fatalf("a request whose body never finished reached the upstream %d times", hits)
			}
		})
	}
}

// Once a request's context has ended, a re-arm sets the deadline to now: a re-arm that runs after the
// cancel's interrupt (a hold that returned just as the stream ended) cannot push the deadline back out.
func TestRearmAfterTheContextEndedSetsTheDeadlineToNow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := trackNetworkBody(httptest.NewRequest(http.MethodPost, "/", strings.NewReader("unread")).WithContext(ctx))
	w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	rearmBodyDeadline(w, r)
	if !w.last.After(time.Now().Add(mitmReadTimeout / 2)) {
		t.Fatalf("a live request's re-arm set %v, want about %v from now", w.last, mitmReadTimeout)
	}
	cancel()
	rearmBodyDeadline(w, r)
	if w.last.After(time.Now()) {
		t.Fatalf("a re-arm after the context ended left the deadline at %v, in the future", w.last)
	}
}

// deadlineRecorder records the last read deadline a handler set on it.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	last time.Time
}

func (d *deadlineRecorder) SetReadDeadline(t time.Time) error {
	d.last = t
	return nil
}

// The stop a handler defers is fenced against an interrupt that has already started
// (context.AfterFunc's own stop does not wait for one), so the deadline is never set on a writer the
// handler has handed back. An interrupt that is setting the deadline holds stop until it has finished.
func TestInterruptStopWaitsForARunningInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := trackNetworkBody(httptest.NewRequest(http.MethodPost, "/", strings.NewReader("unread")).WithContext(ctx))
	w := &fencedDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	stop := interruptBodyReadOnCancel(w, r)
	cancel()
	select {
	case <-w.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the cancel never reached the deadline set")
	}
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("stop returned while the interrupt was still setting the deadline on the handler's writer")
	case <-time.After(200 * time.Millisecond):
	}
	close(w.release)
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("stop never returned after the interrupt finished")
	}
	if n := w.sets.Load(); n != 2 {
		t.Fatalf("the interrupt set the deadline %d times before stop returned, want 2 (the bound, then now)", n)
	}
}

// An interrupt that started and has not yet run when stop returns sets nothing. On one P the interrupt's
// goroutine, started by the cancel, runs only once this one yields, which is after stop. A round where
// the scheduler runs it earlier proves nothing and fails nothing.
func TestInterruptThatRunsAfterStopSetsNothing(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	for range 200 {
		ctx, cancel := context.WithCancel(context.Background())
		r := trackNetworkBody(httptest.NewRequest(http.MethodPost, "/", strings.NewReader("unread")).WithContext(ctx))
		w := &fencedDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
		stop := interruptBodyReadOnCancel(w, r)
		cancel()
		stop()
		w.stopped.Store(true)
		time.Sleep(time.Millisecond) // lets the interrupt run
		if n := w.late.Load(); n != 0 {
			t.Fatalf("an interrupt that started before stop set the deadline %d times after stop returned", n)
		}
	}
}

// fencedDeadlineWriter counts the read deadlines set on it, and those set after stopped. With entered
// and release set, its first set signals entered and waits for release.
type fencedDeadlineWriter struct {
	*httptest.ResponseRecorder
	entered, release chan struct{}
	stopped          atomic.Bool
	sets, late       atomic.Int32
}

func (f *fencedDeadlineWriter) SetReadDeadline(time.Time) error {
	if f.stopped.Load() {
		f.late.Add(1)
	}
	if f.sets.Add(1) == 1 && f.entered != nil {
		close(f.entered)
		<-f.release
	}
	return nil
}
