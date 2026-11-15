// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/cliprofile"
)

// countingRoundTripper counts the requests that get past the session transport to the network.
type countingRoundTripper struct{ sent atomic.Int32 }

func (c *countingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	c.sent.Add(1)
	return http.DefaultTransport.RoundTrip(r)
}

// TestRenewalLockWaitEndsWithTheRequestContext: a request whose access token is inside the renewal margin waits
// for another command's hold on the renewal lock. When its own context ends first (the deadline of
// `run --wait` or `run wait-ready --timeout`, or a cancel), it stops there with that error, sends nothing, and
// does not record a failed renewal; before, it waited out cliLockWait and then went out with no bearer. Once
// the other command lets go, a request on a live context renews and goes out.
func TestRenewalLockWaitEndsWithTheRequestContext(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want error
	}{
		{"deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 100*time.Millisecond)
		}, context.DeadlineExceeded},
		{"cancel", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(100*time.Millisecond, cancel)
			return ctx, cancel
		}, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSignInOrg(t)
			dir := cliEnv(t, approveInBrowser(t, f, nil))
			keychainReady = func() error { return errors.New("no session bus") }
			advance := skewClock(t)
			if _, _, err := runCLI(t, "", "login", f.srv.URL); err != nil {
				t.Fatalf("login: %v", err)
			}
			prevWait := cliLockWait
			cliLockWait = 10 * time.Second
			t.Cleanup(func() { cliLockWait = prevWait })
			origin, err := cliprofile.Normalize(f.srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			sess := &sessionState{}
			sess.resolve(context.Background(), f.srv.URL)
			pin, item := sess.pin, sess.s.item
			advance(intoMargin)
			// Another command is renewing: it holds the lock for the whole test.
			other, held, err := lockRefresh(context.Background(), dir)
			if err != nil || !held {
				t.Fatalf("the other command's lock: held %v, %v", held, err)
			}
			released := false
			release := func() {
				if !released {
					released = true
					other()
				}
			}
			defer release()

			base := &countingRoundTripper{}
			hc := &http.Client{Transport: sessionTransport{base: base, origin: origin, sess: sess}}
			get := func(ctx context.Context) (*http.Response, error) {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.srv.URL+"/api/v1/runs/"+fakeRunID.String(), nil)
				if err != nil {
					t.Fatal(err)
				}
				return hc.Do(req)
			}
			ctx, cancel := tc.ctx()
			defer cancel()
			start := time.Now()
			if resp, err := get(ctx); !errors.Is(err, tc.want) {
				if resp != nil {
					_ = resp.Body.Close()
				}
				t.Fatalf("request on a context that ended during the lock wait: err %v, want %v", err, tc.want)
			}
			if waited := time.Since(start); waited > 3*time.Second {
				t.Errorf("the request waited %v for the lock, want it to stop with its context", waited)
			}
			f.mu.Lock()
			refreshes := f.refreshes
			f.mu.Unlock()
			if base.sent.Load() != 0 || refreshes != 0 {
				t.Errorf("%d requests sent and %d refreshes, want none: nothing leaves when the context ends in the wait", base.sent.Load(), refreshes)
			}
			if sess.err != nil {
				t.Errorf("the cancelled wait was recorded as a renewal failure: %v", sess.err)
			}
			// A context that ends is neither a change of sign-in nor a refusal for every later request.
			if sess.refused != nil || sess.pin != pin || sess.s.item != item {
				t.Errorf("the cancelled wait changed the session state: refused %v, pin %v (want %v), item kept %v",
					sess.refused, sess.pin, pin, sess.s.item == item)
			}

			release()
			resp, err := get(context.Background())
			if err != nil {
				t.Fatalf("request after the lock was released: %v", err)
			}
			_ = resp.Body.Close()
			f.mu.Lock()
			defer f.mu.Unlock()
			if resp.StatusCode != http.StatusOK || f.refreshes != 1 || f.polls != 1 {
				t.Errorf("after the release: status %d, %d refreshes, %d polls, want 200, 1 and 1: the renewal is tried again", resp.StatusCode, f.refreshes, f.polls)
			}
			if sess.refused != nil || sess.pin != pin {
				t.Errorf("after the renewal: refused %v, pin %v, want nil and %v", sess.refused, sess.pin, pin)
			}
		})
	}
}

// TestEndedContextStillLatchesAChangedSignIn: a request whose context has already ended, and whose renewal
// read finds another sign-in stored, is refused as a changed sign-in and latches, as it would on a live
// context. An ended context never turns a changed sign-in into something a later request tries again.
func TestEndedContextStillLatchesAChangedSignIn(t *testing.T) {
	f := newSignInOrg(t)
	dir := cliEnv(t, approveInBrowser(t, f, nil))
	keychainReady = func() error { return errors.New("no session bus") }
	advance := skewClock(t)
	if _, _, err := runCLI(t, "", "login", f.srv.URL); err != nil {
		t.Fatalf("login: %v", err)
	}
	origin, err := cliprofile.Normalize(f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	sess := &sessionState{}
	sess.resolve(context.Background(), f.srv.URL)
	advance(intoMargin)
	f.mu.Lock()
	signInElsewhere(f, sess.s.item, func(it cliItem) error { return (fileStore{dir}).put(origin, it) })
	f.mu.Unlock()
	base := &countingRoundTripper{}
	hc := &http.Client{Transport: sessionTransport{base: base, origin: origin, sess: sess}}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	for _, ctx := range []context.Context{ended, context.Background()} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.srv.URL+"/api/v1/runs/"+fakeRunID.String(), nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := hc.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		wantChanged(t, err)
	}
	if sess.refused == nil || base.sent.Load() != 0 {
		t.Errorf("refused %v, %d requests sent, want the change latched and nothing sent", sess.refused, base.sent.Load())
	}
}

// TestWaitReadyTimeoutCoversTheFirstSignInRead: `run wait-ready --timeout` starts its budget before the command
// reads and renews its sign-in. While another command holds the renewal lock it gives up at the timeout
// (exit 124) with nothing sent; before, it first waited out the 10-second lock wait twice.
func TestWaitReadyTimeoutCoversTheFirstSignInRead(t *testing.T) {
	f := newSignInOrg(t)
	dir := cliEnv(t, approveInBrowser(t, f, nil))
	keychainReady = func() error { return errors.New("no session bus") }
	advance := skewClock(t)
	if _, _, err := runCLI(t, "", "login", f.srv.URL); err != nil {
		t.Fatalf("login: %v", err)
	}
	advance(intoMargin)
	other, held, err := lockRefresh(context.Background(), dir)
	if err != nil || !held {
		t.Fatalf("the other command's lock: held %v, %v", held, err)
	}
	defer other()
	start := time.Now()
	_, _, err = runCLI(t, "", "run", "wait-ready", "--timeout", "100ms", "--url", f.srv.URL, fakeRunID.String())
	if waited := time.Since(start); exitCodeFor(err) != 124 || waited > 3*time.Second {
		t.Errorf("err %v (exit %d) after %v, want exit 124 at the timeout, not after the lock wait", err, exitCodeFor(err), waited)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.polls != 0 || f.refreshes != 0 {
		t.Errorf("%d polls and %d refreshes, want none: nothing leaves while the sign-in cannot be renewed", f.polls, f.refreshes)
	}
}
