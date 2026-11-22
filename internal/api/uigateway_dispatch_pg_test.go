// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// dispatchBackend answers "/hold" with a 200 whose body never ends, "/ws" with a 101 that echoes lines, and
// anything else with a short 200.
func dispatchBackend() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hold":
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/ws":
			conn, brw, err := http.NewResponseController(w).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
			for {
				line, err := brw.ReadString('\n')
				if err != nil {
					return
				}
				if _, err := io.WriteString(conn, line); err != nil {
					return
				}
			}
		default:
			_, _ = io.WriteString(w, "sandbox app")
		}
	})
}

// relayAsync sends one relay GET for path with c and returns its answer.
func (rig *dispatchRig) relayAsync(ctx context.Context, path string, c *http.Cookie) chan *httptest.ResponseRecorder {
	ch := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, rig.path(path), nil).WithContext(ctx)
		req.AddCookie(c)
		rec := httptest.NewRecorder()
		rig.gateway.ServeHTTP(rec, req)
		ch <- rec
	}()
	return ch
}

func answerWithin(t *testing.T, ch chan *httptest.ResponseRecorder, d time.Duration) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(d):
		t.Fatalf("no answer within %s", d)
		return nil
	}
}

// dispatchStall is one place an admitted request is held: arm installs the hold on rig, which signals entered
// and waits for release; typed is whether the refusal is the dispatch 503's reason.
type dispatchStall struct {
	name  string
	arm   func(t *testing.T, rig *dispatchRig, c *http.Cookie, entered chan struct{}, release chan struct{})
	typed bool
	// noStart: no relay or probe exec is started at all.
	noStart bool
}

func dispatchStalls() []dispatchStall {
	return []dispatchStall{
		{name: "a before the hand-off", typed: true, arm: func(_ *testing.T, rig *dispatchRig, _ *http.Cookie, entered, release chan struct{}) {
			rig.pausing(false, nil)
			ps := rig.srv.cfg.Store.(pausingStore)
			ps.touch = func(ctx context.Context) { _ = stall(entered, release)(ctx) }
			rig.srv.cfg.Store = ps
		}},
		{name: "b inside the connection wait", typed: true, arm: func(t *testing.T, rig *dispatchRig, c *http.Cookie, entered, release chan struct{}) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			for i := 0; i < maxUIConnsPerRun; i++ { // the port's connections are all held: the next request waits in the transport
				rig.relayAsync(ctx, "/hold", c)
			}
			waitWithin(t, 5*time.Second, "the port's connections held", func() bool { return slots(rig.srv, rig.run.ID) == maxUIConnsPerRun })
			waitWithin(t, 5*time.Second, "the held requests written", func() bool { // one driver write each, before this request
				rig.dr.mu.Lock()
				defer rig.dr.mu.Unlock()
				return len(rig.dr.hs) >= maxUIConnsPerRun
			})
			close(entered)
		}},
		{name: "c a stalled thaw", typed: true, arm: func(_ *testing.T, rig *dispatchRig, _ *http.Cookie, entered, release chan struct{}) {
			rig.pausing(true, nil)
			rig.dr.setHook("thaw", stall(entered, release))
		}},
		{name: "c a stalled start lock", arm: func(t *testing.T, rig *dispatchRig, _ *http.Cookie, entered, release chan struct{}) {
			h, err := rig.srv.lockStart(context.Background(), rig.run.ID)
			if err != nil {
				t.Fatalf("hold the start lock: %v", err)
			}
			go func() { <-release; h.release() }()
			close(entered)
		}},
		{name: "c a stalled last read under the start lock", typed: true, noStart: true, arm: func(_ *testing.T, rig *dispatchRig, _ *http.Cookie, entered, release chan struct{}) {
			hold := stall(entered, release)
			rig.pausing(false, func(ctx context.Context) {
				if inStack("startRead") {
					_ = hold(context.Background()) // the read stalls past the dispatch time, whatever its own bound
				}
			})
		}},
		{name: "c a stalled app probe", typed: true, arm: func(_ *testing.T, rig *dispatchRig, _ *http.Cookie, entered, release chan struct{}) {
			rig.dr.setHook("sh", stall(entered, release))
		}},
		{name: "c a relay exec started late", typed: true, arm: func(_ *testing.T, rig *dispatchRig, _ *http.Cookie, entered, release chan struct{}) {
			rig.dr.setHook("socat", func(context.Context) error {
				close(entered)
				<-release // returns after the dispatch time with a session
				return nil
			})
		}},
	}
}

func TestUIRelayIssuerPortal_ZeroBytesAfterDeadline_PG(t *testing.T) {
	for _, st := range dispatchStalls() {
		for _, after := range []bool{true, false} {
			name := st.name + "/released before"
			if after {
				name = st.name + "/released after"
			}
			t.Run(name, func(t *testing.T) {
				rig := newDispatchRig(t, dispatchBackend())
				c := rig.session(rig.grant)
				entered, release := make(chan struct{}), make(chan struct{})
				st.arm(t, rig, c, entered, release)
				_, held := rig.dr.bytesAtOrAfter(time.Now()) // the stall's own requests' bytes (b's held connections), not this request's
				t0 := time.Now()
				by := t0.Add(rig.fresh) // at or before the request's dispatch time
				ans := rig.relayAsync(context.Background(), "/", c)
				<-entered
				rig.revokePortal(t)
				if after {
					time.Sleep(time.Until(by) + 200*time.Millisecond)
				}
				close(release)
				r := answerWithin(t, ans, 10*time.Second)
				late, all := rig.dr.bytesAtOrAfter(by)
				if late != 0 {
					t.Fatalf("%d bytes passed the driver at or after the dispatch time (%d in all)", late, all)
				}
				if s := rig.dr.startsAtOrAfter("", by); len(s) != 0 {
					t.Fatalf("starts at or after the dispatch time: %+v", s)
				}
				if !after {
					return
				}
				if all-held != 0 || r.Code != http.StatusServiceUnavailable {
					t.Fatalf("released after: %d bytes, answer %d %s; want 0 and the 503", all-held, r.Code, r.Body.String())
				}
				if st.typed && dispatchReason(r) != uiDeniedReasonDelegationUnavailable {
					t.Fatalf("released after: reason %q, want the dispatch 503's %q", dispatchReason(r), uiDeniedReasonDelegationUnavailable)
				}
				if st.noStart && (rig.dr.count("sh") != 0 || rig.dr.count("socat") != 0) {
					t.Fatalf("a start was called with no time left: %d probes, %d relays", rig.dr.count("sh"), rig.dr.count("socat"))
				}
				if st.name == "c a relay exec started late" {
					waitWithin(t, 5*time.Second, "the late relay exec ended", func() bool {
						rig.dr.mu.Lock()
						defer rig.dr.mu.Unlock()
						return rig.dr.closes >= 1
					})
				}
			})
		}
	}

	t.Run("f a refusal before the budget timer", func(t *testing.T) {
		for _, startErr := range []error{runner.ErrStartCutoff, errors.New("a driver error no answer names")} {
			rig := newDispatchRig(t, dispatchBackend())
			c := rig.session(rig.grant)
			old := startAfterFunc
			startAfterFunc = func(time.Duration, func()) *time.Timer { return time.AfterFunc(time.Hour, func() {}) }
			by := time.Now().Add(rig.fresh)
			rig.dr.setHook("socat", func(context.Context) error {
				time.Sleep(time.Until(by) + 100*time.Millisecond)
				return startErr
			})
			r := rig.relay(c)
			startAfterFunc = old
			if r.Code != http.StatusServiceUnavailable || dispatchReason(r) != uiDeniedReasonDelegationUnavailable {
				t.Fatalf("%v after the dispatch time, budget timer held: %d %s; want the dispatch 503, never 502", startErr, r.Code, r.Body.String())
			}
		}
	})

	t.Run("queued past the deadline", func(t *testing.T) {
		for _, holdRecord := range []bool{false, true} {
			rig := newDispatchRig(t, dispatchBackend())
			c := rig.session(rig.grant)
			by := time.Now().Add(rig.fresh)
			queued := make(chan struct{})
			var once sync.Once
			rig.srv.uiDispatch.beforeAdmit = func() {
				once.Do(func() { close(queued) })
				time.Sleep(time.Until(by) + 100*time.Millisecond)
			}
			recordHeld, recordFree := make(chan struct{}), make(chan struct{})
			if holdRecord {
				var ronce sync.Once
				rig.srv.uiDispatch.refuseHook = func() { ronce.Do(func() { close(recordHeld) }); <-recordFree }
			}
			ans := rig.relayAsync(context.Background(), "/", c)
			<-queued
			if holdRecord {
				<-recordHeld
				select {
				case r := <-ans:
					t.Fatalf("an answer (%d) while the gate's 503 record was held", r.Code)
				case <-time.After(200 * time.Millisecond):
				}
				close(recordFree)
			}
			r := answerWithin(t, ans, 10*time.Second)
			if _, all := rig.dr.bytesAtOrAfter(by); all != 0 || r.Code != http.StatusServiceUnavailable || dispatchReason(r) != uiDeniedReasonDelegationUnavailable {
				t.Fatalf("queued past the deadline (record held %v): %d bytes, %d %s; want 0 and the dispatch 503", holdRecord, all, r.Code, r.Body.String())
			}
		}
	})

	t.Run("coverage", func(t *testing.T) {
		for _, mode := range []gatewayMode{gwHTTP1, gwHTTP2} {
			rig := newDispatchRig(t, dispatchBackend())
			c := rig.session(rig.grant)
			by := time.Now().Add(rig.fresh)
			rig.srv.uiDispatch.beforeAdmit = func() { time.Sleep(time.Until(by) + 100*time.Millisecond) }
			srv, _ := serveGateway(t, rig.issuerUIRig, mode, nil)
			req, _ := http.NewRequest(http.MethodGet, srv.URL+rig.path("/"), nil)
			req.AddCookie(c)
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("%s: %v", mode, err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable || (mode == gwHTTP2 && resp.ProtoMajor != 2) {
				t.Fatalf("%s: %d over HTTP/%d, want the 503", mode, resp.StatusCode, resp.ProtoMajor)
			}
		}
		rig := newDispatchRig(t, dispatchBackend())
		c := rig.session(rig.grant)
		by := time.Now().Add(rig.fresh)
		rig.srv.uiDispatch.beforeAdmit = func() { time.Sleep(time.Until(by) + 100*time.Millisecond) }
		srv, _ := serveGateway(t, rig.issuerUIRig, gwHTTP1, nil)
		awaitStatus(t, rawRequest(t, srv, gwHTTP1, rig.path("/ws"), c, "Connection: Upgrade\r\nUpgrade: test\r\n"), " 503 ")
	})

	t.Run("the relay's life is not cut", func(t *testing.T) {
		rig := newDispatchRig(t, dispatchBackend())
		c := rig.session(rig.grant)
		by := time.Now().Add(rig.fresh)
		srv, _ := serveGateway(t, rig.issuerUIRig, gwHTTP1, nil)
		conn := rawRequest(t, srv, gwHTTP1, rig.path("/ws"), c, "Connection: Upgrade\r\nUpgrade: test\r\n")
		awaitStatus(t, conn, " 101 ")
		time.Sleep(time.Until(by) + 3*rig.fresh)
		if _, err := io.WriteString(conn, "still here\n"); err != nil {
			t.Fatalf("write after the dispatch time: %v", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if line, err := bufio.NewReader(conn).ReadString('\n'); err != nil || line != "still here\n" {
			t.Fatalf("the upgraded relay after the dispatch time: %q %v", line, err)
		}
	})
}

func TestUIRelayIssuerPortal_ExpiryLeavesOtherSessionsUsable_PG(t *testing.T) {
	rig := newDispatchRig(t, dispatchBackend())
	c := rig.session(rig.grant)
	by := time.Now().Add(rig.fresh)
	expiring := atomic.Bool{}
	expiring.Store(true)
	queued := make(chan struct{})
	var once sync.Once
	rig.srv.uiDispatch.beforeAdmit = func() {
		if expiring.Load() {
			once.Do(func() { close(queued) })
			time.Sleep(time.Until(by) + 100*time.Millisecond)
		}
	}
	ans := rig.relayAsync(context.Background(), "/", c)
	<-queued
	expiring.Store(false)
	// While it expires: a session with no portal on it is served on the shared pool, twice on one connection,
	// and a second portal session on its own fresh dial.
	plain := plainCookie(rig.uiHarness, rig.run.ID, rig.principal)
	var got, reused atomic.Int32
	for i := 0; i < 2; i++ {
		if r := traced(rig.uiHarness, rig.run.ID, plain, &got, &reused); r.Code != http.StatusOK {
			t.Fatalf("the plain session while a portal request expires: %d %s", r.Code, r.Body.String())
		}
	}
	if reused.Load() != 1 {
		t.Fatalf("the plain session's second request reused %d connections, want 1", reused.Load())
	}
	second := rig.session(rig.grant)
	if r := rig.relay(second); r.Code != http.StatusOK {
		t.Fatalf("a second portal session while one expires: %d %s", r.Code, r.Body.String())
	}
	if r := answerWithin(t, ans, 10*time.Second); r.Code != http.StatusServiceUnavailable {
		t.Fatalf("the expired request: %d, want 503", r.Code)
	}
	// Its slot is freed on the cleanup path after Close, so it is polled for.
	waitWithin(t, 5*time.Second, "the expired request's slot freed", func() bool { return slots(rig.srv, rig.run.ID) <= 1 })
	var held []func()
	for i := 0; ; i++ {
		release, ok := rig.srv.acquireUIConn(rig.run.ID)
		if !ok {
			break
		}
		held = append(held, release)
	}
	for _, r := range held {
		r()
	}
	if n := len(held) + slots(rig.srv, rig.run.ID); n != maxUIConnsPerRun {
		t.Fatalf("slots taken up to the cap: %d, want %d", n, maxUIConnsPerRun)
	}
}

// dispatchPct is the p-th percentile of ds.
func dispatchPct(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[int(float64(len(s)-1)*p)]
}

// TestUIRelayIssuerPortal_SingleUseCostMeasured_PG measures the single-use proxy against the shared one; its one
// gate is zero slot refusals for a page with more than maxUIConnsPerRun requests in flight.
func TestUIRelayIssuerPortal_SingleUseCostMeasured_PG(t *testing.T) {
	load := func(rig *dispatchRig, c *http.Cookie, n int) (lat []time.Duration, refused int) {
		var mu sync.Mutex
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				start := time.Now()
				r := rig.relay(c)
				mu.Lock()
				defer mu.Unlock()
				lat = append(lat, time.Since(start))
				if r.Code == http.StatusServiceUnavailable {
					refused++
				}
			}()
		}
		wg.Wait()
		return lat, refused
	}
	for _, kase := range []string{"cold", "warm", "at the slot cap"} {
		for _, portal := range []bool{true, false} {
			rig := newDispatchRig(t, dispatchBackend())
			setDispatchFresh(t, 25*time.Second) // the production value
			c := plainCookie(rig.uiHarness, rig.run.ID, rig.principal)
			if portal {
				c = rig.session(rig.grant)
			}
			if kase != "cold" {
				rig.relay(c) // warms the app-ready entry
			}
			if kase == "at the slot cap" {
				for i := 0; i < maxUIConnsPerRun-1; i++ {
					release, _ := rig.srv.acquireUIConn(rig.run.ID)
					t.Cleanup(release)
				}
			}
			lat, refused := load(rig, c, 20)
			t.Logf("20-asset page, %s, portal=%v: p50 %s p95 %s, %d slot refusals, %d relay execs, %d probes",
				kase, portal, dispatchPct(lat, 0.5), dispatchPct(lat, 0.95), refused, rig.dr.count("socat"), rig.dr.count("sh"))
			if kase != "at the slot cap" && refused != 0 {
				t.Errorf("%s portal=%v: %d slot refusals for a page with more than %d requests in flight, want 0", kase, portal, refused, maxUIConnsPerRun)
			}
		}
	}
	// Cold starts refused with no revoke: an app whose cold start runs past the dispatch time.
	rig := newDispatchRig(t, dispatchBackend())
	c := rig.session(rig.grant)
	rig.dr.setHook("sh", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	r := rig.relay(c)
	rig.dr.setHook("sh", nil)
	retry := rig.relay(c)
	t.Logf("a cold start past the dispatch time: %d; the retry: %d (probes %d)", r.Code, retry.Code, rig.dr.count("sh"))
	if r.Code != http.StatusServiceUnavailable {
		t.Errorf("a cold start past the dispatch time: %d, want 503", r.Code)
	}
}
