// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The open-stream half of an issuer portal's UI session: relayed streams, 101s and delayed headers end at
// their known end and within one re-check of an event that removes the session's authority, with a client
// that has stopped reading included. The gateway is served on a real server, so hijacks and deadlines are
// the server's own.

// relayEvery and relayBound scale the relay watch 30:1: one tick (30 s) and its lookup (5 s) become 1 s
// and about 167 ms. A stream is cut within relayEvery + relayBound of an event; the per-write fallback keeps
// its unscaled 30 s, so a cut inside the scaled bound is the concurrent interrupt's.
const (
	relayEvery = time.Second
	relayBound = 167 * time.Millisecond
	relaySlack = 400 * time.Millisecond
	relayAdmit = 2 * time.Second // the opening reads' own bound: a stream is admitted before it can be cut
)

// setRelayWatch scales the relay watch (setSupportWatch at relayBound) and gives the opening reads relayAdmit,
// so a cut test's admission does not race the scaled lookup bound, on Postgres under -race included.
func setRelayWatch(t *testing.T, every time.Duration) {
	t.Helper()
	setSupportWatch(t, every, relayBound)
	setIssuerAdmit(t, relayAdmit)
}

// inputLog records what the backend of a switched stream read: each line's send time, as the client stamped it.
type inputLog struct {
	mu    sync.Mutex
	sent  []time.Time
	bytes int
	fin   bool // the backend's reader returned
	out   int  // bytes the /stream backend wrote and flushed
}

func (l *inputLog) streamed() int { l.mu.Lock(); defer l.mu.Unlock(); return l.out }

func (l *inputLog) finish() { l.mu.Lock(); l.fin = true; l.mu.Unlock() }

func (l *inputLog) finished() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.fin }

func (l *inputLog) add(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.bytes += len(line)
	if ns, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64); err == nil {
		l.sent = append(l.sent, time.Unix(0, ns))
	}
}

// sentAfter counts the lines the backend received that the client sent after t.
func (l *inputLog) sentAfter(t time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.sent {
		if s.After(t) {
			n++
		}
	}
	return n
}

// streamBackend is the sandbox app: /stream an endless event stream, /hang headers that never come, /early a
// 103 at the time ?at names and then a hang, /rogue101 a 101 to a request that asked for none, /body a read
// of a streamed request body while an answer streams back, /upload a read of one with no answer until it ends,
// and any upgrade a 101 that then writes without end (nothing with X-Quiet), counting what it streamed, and logs
// its input.
func streamBackend(in *inputLog) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("Upgrade") != "":
			conn, brw, err := http.NewResponseController(w).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " + r.Header.Get("Upgrade") + "\r\nConnection: Upgrade\r\n\r\n")
			_ = brw.Flush()
			go func() {
				if r.Header.Get("X-Quiet") != "" {
					return // sends nothing
				}
				pad := " " + strings.Repeat("o", 32<<10) + "\n"
				for { // each frame starts with its send time (readLog.stampedAfter)
					n, err := io.WriteString(conn, strconv.FormatInt(time.Now().UnixNano(), 10)+pad)
					in.mu.Lock()
					in.out += n // what the backend streamed (inputLog.streamed)
					in.mu.Unlock()
					if err != nil {
						return
					}
				}
			}()
			sc := bufio.NewScanner(brw)
			for sc.Scan() {
				in.add(sc.Text())
			}
			in.finish()
		case strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Type", "text/event-stream")
			rc := http.NewResponseController(w)
			chunk := "data: " + strings.Repeat("e", 32<<10) + "\n\n"
			for r.Context().Err() == nil {
				if _, err := io.WriteString(w, chunk); err != nil || rc.Flush() != nil {
					return
				}
				in.mu.Lock()
				in.out += len(chunk)
				in.mu.Unlock()
			}
		case strings.HasSuffix(r.URL.Path, "/hang"):
			<-r.Context().Done()
		case strings.HasSuffix(r.URL.Path, "/early"):
			at, _ := strconv.ParseInt(r.URL.Query().Get("at"), 10, 64)
			time.Sleep(time.Until(time.Unix(0, at)))
			w.Header().Set("Link", "</a.css>; rel=preload")
			w.WriteHeader(http.StatusEarlyHints)
			<-r.Context().Done()
		case strings.HasSuffix(r.URL.Path, "/rogue101"):
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			time.Sleep(time.Second)
		case strings.HasSuffix(r.URL.Path, "/body"):
			rc := http.NewResponseController(w)
			_ = rc.EnableFullDuplex() // the answer streams while the body is still being read
			w.WriteHeader(http.StatusOK)
			_ = rc.Flush()
			wrote, read := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(wrote)
				for r.Context().Err() == nil {
					select {
					case <-read:
						return
					default:
					}
					if _, err := io.WriteString(w, strconv.FormatInt(time.Now().UnixNano(), 10)+"\n"); err != nil || rc.Flush() != nil {
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
			}()
			sc := bufio.NewScanner(r.Body)
			for sc.Scan() {
				in.add(sc.Text())
			}
			in.finish()
			close(read)
			<-wrote // the writer stops with the body, before the handler returns
		case strings.HasSuffix(r.URL.Path, "/upload"): // input only: nothing is answered until the body ends
			sc := bufio.NewScanner(r.Body)
			for sc.Scan() {
				in.add(sc.Text())
			}
			in.finish()
		default:
			_, _ = io.WriteString(w, "app")
		}
	})
}

// gatewayEnds wraps the gateway so each request's end (a return or the abort) is reported on ended.
func gatewayEnds(h http.Handler) (http.Handler, chan time.Time) {
	ended := make(chan time.Time, 16)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { ended <- time.Now() }()
		h.ServeHTTP(w, r)
	}), ended
}

// gatewayMode is how the test server serves the gateway: HTTP/1, HTTP/1 over TLS, or HTTP/2 over TLS.
type gatewayMode int

const (
	gwHTTP1 gatewayMode = iota
	gwTLS1
	gwHTTP2
)

func (m gatewayMode) String() string { return [...]string{"HTTP1", "HTTP1-TLS", "HTTP2"}[m] }

// serveGateway serves rig's gateway in mode, its connections on conns when conns has room, and reports
// each request's end.
func serveGateway(t *testing.T, rig *issuerUIRig, mode gatewayMode, conns chan *stallConn) (*httptest.Server, chan time.Time) {
	t.Helper()
	h, ended := gatewayEnds(rig.gateway)
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = &stallListener{Listener: srv.Listener, conns: conns}
	switch mode {
	case gwHTTP1:
		srv.Start()
	case gwTLS1:
		srv.TLS = &tls.Config{NextProtos: []string{"http/1.1"}}
		srv.StartTLS()
	case gwHTTP2:
		srv.EnableHTTP2 = true
		srv.StartTLS()
	}
	t.Cleanup(srv.Close)
	return srv, ended
}

// rawRequest opens a connection to srv (TLS unless mode is plain HTTP/1), sends one GET for path with cookie
// and extra headers, and returns the connection unread.
func rawRequest(t *testing.T, srv *httptest.Server, mode gatewayMode, path string, c *http.Cookie, extra string) net.Conn {
	t.Helper()
	return rawMethod(t, srv, mode, http.MethodGet, path, c, extra)
}

// rawMethod is rawRequest for any method; a body, if any, is the caller's to write.
func rawMethod(t *testing.T, srv *httptest.Server, mode gatewayMode, method, path string, c *http.Cookie, extra string) net.Conn {
	t.Helper()
	addr := srv.Listener.Addr().String()
	var conn net.Conn
	var err error
	if mode == gwHTTP1 {
		conn, err = net.Dial("tcp", addr)
	} else {
		conn, err = tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}}) // #nosec G402 -- the test server's own certificate
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	req := method + " " + path + " HTTP/1.1\r\nHost: " + addr + "\r\nCookie: " + c.Name + "=" + c.Value + "\r\n" + extra + "\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	return conn
}

// awaitHeaders reads conn's response through its header block, so the request was admitted before the test goes on.
func awaitHeaders(t *testing.T, conn net.Conn) { t.Helper(); awaitStatus(t, conn, " 200 ") }

// awaitStatus is awaitHeaders for a response whose status line holds code (" 101 " for a switch).
func awaitStatus(t *testing.T, conn net.Conn, code string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	if line, err := br.ReadString('\n'); err != nil || !strings.Contains(line, code) {
		t.Fatalf("the stream was not admitted: %q %v", line, err)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("the stream's headers did not arrive: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}
	_ = conn.SetReadDeadline(time.Time{})
}

// h2Unread starts an HTTP/2 GET for path with cookie, requires its 200, and never reads its body: flow control
// then blocks the server's writes. The request ends with the test.
func h2Unread(t *testing.T, srv *httptest.Server, path string, c *http.Cookie) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
	req.AddCookie(c)
	type answer struct {
		code, major int
		err         error
	}
	got := make(chan answer, 1)
	go func() {
		resp, err := srv.Client().Do(req)
		if err != nil {
			got <- answer{err: err}
			return
		}
		got <- answer{code: resp.StatusCode, major: resp.ProtoMajor}
		<-ctx.Done()
		_ = resp.Body.Close()
	}()
	select {
	case a := <-got:
		if a.err != nil || a.code != http.StatusOK || a.major != 2 {
			t.Fatalf("the HTTP/2 stream was not admitted: %d (HTTP/%d) %v", a.code, a.major, a.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the HTTP/2 stream never answered")
	}
}

// requireBlocked fails unless the backend has streamed through the gateway and then stopped, with the request
// still open: the gateway's write to its client is pending.
func requireBlocked(t *testing.T, in *inputLog, ended <-chan time.Time) {
	t.Helper()
	last, still := -1, 0
	for deadline := time.Now().Add(10 * time.Second); still < 2; {
		select {
		case <-ended:
			t.Fatal("the stream ended before anything cut it")
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("the backend never stopped streaming (%d bytes): the gateway's write never blocked", in.streamed())
		}
		n := in.streamed()
		if n > 0 && n == last {
			still++
		} else {
			still = 0
		}
		last = n
	}
}

// waitEnded waits for the gateway's request to end and returns when it did.
func waitEnded(t *testing.T, ended <-chan time.Time, within time.Duration, what string) time.Time {
	t.Helper()
	select {
	case at := <-ended:
		return at
	case <-time.After(within):
		t.Fatalf("%s: the stream was still open %v later", what, within)
		return time.Time{}
	}
}

// requireOpen fails when the gateway's request ended within d.
func requireOpen(t *testing.T, ended <-chan time.Time, d time.Duration) {
	t.Helper()
	select {
	case <-ended:
		t.Fatal("the stream ended before anything cut it")
	case <-time.After(d):
	}
}

// cutBy checks a cut landed after the event at from and within bound of it, and logs the measure. An end
// before the event is a refusal or a disconnect, never a measured cut.
func cutBy(t *testing.T, what string, from, at time.Time, bound time.Duration) {
	t.Helper()
	d := at.Sub(from)
	t.Logf("%s: the stream ended %v after the event (bound %v)", what, d.Round(time.Millisecond), bound)
	switch {
	case d < 0:
		t.Errorf("%s: ended %v before the event: not a cut by it", what, -d)
	case d > bound:
		t.Errorf("%s: ended %v after the event, past the bound %v", what, d, bound)
	}
}

// streamRig is a renewing issuer UI rig whose backend is streamBackend, with one session open under a grant
// that ends in 5 minutes.
func streamRig(t *testing.T) (*issuerUIRig, *http.Cookie) {
	t.Helper()
	rig, c, _ := streamRigLog(t)
	return rig, c
}

// streamRigLog is streamRig with the backend's log.
func streamRigLog(t *testing.T) (*issuerUIRig, *http.Cookie, *inputLog) {
	t.Helper()
	in := &inputLog{}
	rig := newIssuerUIRig(t, streamBackend(in))
	rig.is.renewal = true
	return rig, rig.session(rig.is.grant(rig.principal, uuid.Nil, rig.clock.now().Add(5*time.Minute))), in
}

// wall is the wall-clock time of t on the rig's harness clock, as the gateway converts a stream's end.
func (rig *issuerUIRig) wall(t time.Time) time.Time { return time.Now().Add(t.Sub(rig.clock.now())) }

func (rig *issuerUIRig) revoke() {
	rig.is.mu.Lock()
	rig.is.revoked = true
	rig.is.mu.Unlock()
}

const relayPath = "/stream"

func (rig *issuerUIRig) path(p string) string { return uiRelayPrefix(rig.run.ID, "code") + p }

// TestUIRenewal_OpenStreamStopsWithin35s: an open event stream, a request whose headers never come and a
// 101 are each cut within one re-check of a portal revoke, renewal on.
func TestUIRenewal_OpenStreamStopsWithin35s(t *testing.T) {
	setRelayWatch(t, relayEvery)
	cases := map[string]struct{ path, extra string }{
		"event stream":    {relayPath, ""},
		"delayed headers": {"/hang", ""},
		"101":             {"/ws", "Connection: Upgrade\r\nUpgrade: test\r\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rig, c := streamRig(t)
			srv, ended := serveGateway(t, rig, gwHTTP1, nil)
			rawRequest(t, srv, gwHTTP1, rig.path(tc.path), c, tc.extra)
			requireOpen(t, ended, 300*time.Millisecond)
			at := time.Now()
			rig.revoke()
			cutBy(t, name, at, waitEnded(t, ended, 10*time.Second, name), relayEvery+relayBound+relaySlack)
		})
	}
}

// TestUIRenewal_CheckOutageStopsWithin35s: a re-check whose store hangs is cut at its lookup bound: the
// stream ends within one tick and the bound.
func TestUIRenewal_CheckOutageStopsWithin35s(t *testing.T) {
	setRelayWatch(t, relayEvery)
	rig, c := streamRig(t)
	srv, ended := serveGateway(t, rig, gwHTTP1, nil)
	rawRequest(t, srv, gwHTTP1, rig.path(relayPath), c, "")
	requireOpen(t, ended, 300*time.Millisecond)
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	at := time.Now()
	rig.is.hold(hang)
	cutBy(t, "outage", at, waitEnded(t, ended, 10*time.Second, "outage"), relayEvery+relayBound+relaySlack)
}

// TestUIRenewal_OpenStreamCutAtAbsoluteExpiry: a renewable stream crosses its cookie's absolute end, issue
// time plus the session TTL, while the portal keeps exchanging, and is cut at it.
func TestUIRenewal_OpenStreamCutAtAbsoluteExpiry(t *testing.T) {
	setRelayWatch(t, relayEvery)
	rig := newIssuerUIRig(t, streamBackend(&inputLog{}))
	rig.is.renewal = true
	rig.srv.cfg.UISessionTTL = 3 * time.Second
	a := rig.is.grant(rig.principal, uuid.Nil, rig.clock.now().Add(time.Hour))
	c := rig.session(a)
	end := rig.wall(time.Unix(rig.clock.now().Unix(), 0).Add(3 * time.Second)) // IssuedAt is the redemption's second
	srv, ended := serveGateway(t, rig, gwHTTP1, nil)
	awaitHeaders(t, rawRequest(t, srv, gwHTTP1, rig.path(relayPath), c, "")) // admitted before the portal exchanges
	stop := make(chan struct{})
	go func() { // the portal keeps exchanging
		for {
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Millisecond):
				rig.is.grant(rig.principal, a.ChainID, rig.clock.now().Add(time.Hour))
			}
		}
	}()
	defer close(stop)
	got := waitEnded(t, ended, 10*time.Second, "absolute expiry")
	cutWithin(t, "the stream ended", got, end, relaySlack)
}

// TestUIRenewal_RevokeBetweenAdmissionReadsCutWithin35s: a revoke that commits just after the admission
// check's portal read, while the rest of that check runs, is seen by the first tick, which is timed from the
// check's start: the stream is cut within one tick and the bound of the commit.
func TestUIRenewal_RevokeBetweenAdmissionReadsCutWithin35s(t *testing.T) {
	setRelayWatch(t, relayEvery)
	rig, c := streamRig(t)
	srv, ended := serveGateway(t, rig, gwHTTP1, nil)
	committed := make(chan time.Time, 1)
	rig.is.mu.Lock()
	rig.is.afterChainRead = func() { // the admission check's portal read has answered live
		rig.revoke()
		committed <- time.Now()
	}
	rig.is.mu.Unlock()
	rawRequest(t, srv, gwHTTP1, rig.path(relayPath), c, "")
	at := <-committed
	cutBy(t, "revoke during admission", at, waitEnded(t, ended, 10*time.Second, "revoke during admission"), relayEvery+relayBound+relaySlack)
}

// endsAt opens a stream of the rig's session c and checks it ends at end.
func endsAt(t *testing.T, rig *issuerUIRig, c *http.Cookie, end time.Time, what string) {
	t.Helper()
	srv, ended := serveGateway(t, rig, gwHTTP1, nil)
	rawRequest(t, srv, gwHTTP1, rig.path(relayPath), c, "")
	cutWithin(t, what, waitEnded(t, ended, 10*time.Second, what), end, relaySlack)
}

// TestUIIssuerPortal_NonRenewableStreamEndsAtGrantExpiry: with renewal off, an open stream ends at its entry
// grant's end, though a later grant lives.
func TestUIIssuerPortal_NonRenewableStreamEndsAtGrantExpiry(t *testing.T) {
	setRelayWatch(t, time.Hour)
	rig := newIssuerUIRig(t, streamBackend(&inputLog{}))
	a := rig.is.grant(rig.principal, uuid.Nil, rig.clock.now().Add(2*time.Second))
	c := rig.session(a)
	rig.is.grant(rig.principal, a.ChainID, rig.clock.now().Add(time.Hour))
	endsAt(t, rig, c, rig.wall(cookieEnd(a.ExpiresAt)), "a non-renewable stream")
}

// cookieEnd is the expiry a non-renewable cookie of a grant ending at end is signed with: end in whole seconds
// (uiSession.Expires), which the stream's end (uiStreamEnd) takes when it is the earliest, as it is here.
func cookieEnd(end time.Time) time.Time { return time.Unix(end.Unix(), 0) }

// TestPortalDisable_OpenStreamsEndAtGrantExpiry: a person the identity provider disabled gets no further
// grant, so a renewable session's open stream ends at the end its chain had when it was admitted.
func TestPortalDisable_OpenStreamsEndAtGrantExpiry(t *testing.T) {
	setRelayWatch(t, time.Hour)
	rig := newIssuerUIRig(t, streamBackend(&inputLog{}))
	rig.is.renewal = true
	a := rig.is.grant(rig.principal, uuid.Nil, rig.clock.now().Add(2*time.Second))
	endsAt(t, rig, rig.session(a), rig.wall(a.ExpiresAt), "a disabled person's stream")
}

// TestUIIssuerPortal_LocalTTLShorterThanGrantCuts: a serving TTL shorter than the grant's remaining life
// cuts the stream at the TTL.
func TestUIIssuerPortal_LocalTTLShorterThanGrantCuts(t *testing.T) {
	setRelayWatch(t, time.Hour)
	rig := newIssuerUIRig(t, streamBackend(&inputLog{}))
	c := rig.session(rig.is.grant(rig.principal, uuid.Nil, rig.clock.now().Add(time.Hour)))
	rig.srv.cfg.UISessionTTL = 3 * time.Second // this process's own TTL, shorter than the one the cookie was minted under
	endsAt(t, rig, c, rig.wall(time.Unix(rig.clock.now().Unix(), 0).Add(3*time.Second)), "a stream past the local TTL")
}

// TestUIRenewal_WatchInstalledBeforePresenceWork: the presence work after admission stalls (the idle clock's
// store write hangs), and the watch still cuts the request within one tick and the bound of admission.
func TestUIRenewal_WatchInstalledBeforePresenceWork(t *testing.T) {
	setRelayWatch(t, relayEvery)
	rig, c := streamRig(t)
	stalled := make(chan time.Time, 1)
	rig.is.mu.Lock()
	rig.is.stallTouch = func(ctx context.Context) {
		stalled <- time.Now()
		<-ctx.Done()
	}
	rig.is.mu.Unlock()
	srv, ended := serveGateway(t, rig, gwHTTP1, nil)
	rawRequest(t, srv, gwHTTP1, rig.path(relayPath), c, "")
	at := <-stalled
	rig.revoke()
	cutBy(t, "stalled presence work", at, waitEnded(t, ended, 10*time.Second, "stalled presence work"), relayEvery+relayBound+relaySlack)
}

// TestUIRenewal_MixedReplicas: a 0.10.1 server checks a portal session's cookie by its entry grant alone
// (delegationLive on sess.Via), so it refuses a renewable cookie once that grant has ended, including the
// cookie behind a stream already open on a new server, which keeps serving to its admitted end. A console
// portal's session, default off, carries no chain and is checked the same way by both.
func TestUIRenewal_MixedReplicas(t *testing.T) {
	setRelayWatch(t, relayEvery)
	rig, c := streamRig(t)
	srv, ended := serveGateway(t, rig, gwHTTP1, nil)
	rawRequest(t, srv, gwHTTP1, rig.path(relayPath), c, "") // open on the new server
	requireOpen(t, ended, 300*time.Millisecond)
	sess := decodeCookie(t, rig.uiHarness, c)
	if sess.Chain == uuid.Nil || sess.Via == nil {
		t.Fatal("a renewable issuer portal's cookie carries no chain")
	}
	rig.is.grant(rig.principal, sess.Chain, rig.clock.now().Add(time.Hour))
	rig.clock.advance(6 * time.Minute) // past the entry grant's end; the successor lives
	if r := rig.relay(c); r.Code != http.StatusOK {
		t.Fatalf("the new server refused a renewed session: %d %s", r.Code, r.Body.String())
	}
	if _, v := rig.srv.delegationLive(context.Background(), *sess.Via); v == verdictOK {
		t.Fatal("the 0.10.1 check passed a renewable cookie past its entry grant's end")
	}
	requireOpen(t, ended, 2*relayEvery) // the open stream is not the old server's to end
	// A console portal's session.
	ch, _, _, via := portalHarness(t)
	cc := setUICookies(ch.enterWith(ch.portalTicket(via)))[0]
	csess := decodeCookie(t, ch, cc)
	if csess.Chain != uuid.Nil {
		t.Fatal("a console portal's cookie carries a chain")
	}
	if _, v := ch.srv.delegationLive(context.Background(), *csess.Via); v != verdictOK {
		t.Fatalf("the 0.10.1 check refused a console portal's live session: %v", v)
	}
	if r := uiGet(ch, uiRelayPrefix(ch.run.ID, "code")+"/", cc); r.Code != http.StatusOK {
		t.Fatalf("a console portal's session: %d", r.Code)
	}
}

// decodeCookie decodes c as h's gateway does.
func decodeCookie(t *testing.T, h *uiHarness, c *http.Cookie) uiSession {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, uiRelayPrefix(h.run.ID, "code")+"/", nil)
	req.AddCookie(c)
	sess, ok := h.srv.decodeUISession(req, h.clock.now())
	if !ok {
		t.Fatal("the cookie does not decode")
	}
	return sess
}

// TestUIRelayIssuerPortal_CutBeforeHijackRefusesSwitch: the cut lands between the backend's 101 and the
// hijack. The switch is refused and no byte reaches the client after the cut.
func TestUIRelayIssuerPortal_CutBeforeHijackRefusesSwitch(t *testing.T) {
	setRelayWatch(t, 50*time.Millisecond)
	rig, c := streamRig(t)
	hook := func(sc *streamCut) {
		rig.revoke()
		for !sc.isCut() {
			time.Sleep(5 * time.Millisecond)
		}
	}
	streamCutHijackHook.Store(&hook)
	t.Cleanup(func() { streamCutHijackHook.Store(nil) })
	srv, ended := serveGateway(t, rig, gwHTTP1, nil)
	conn := rawRequest(t, srv, gwHTTP1, rig.path("/ws"), c, "Connection: Upgrade\r\nUpgrade: test\r\n")
	waitEnded(t, ended, 10*time.Second, "cut before the hijack")
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if b, err := io.ReadAll(conn); len(b) != 0 || isTimeout(err) {
		t.Fatalf("after a cut before the hijack the client read %q (err %v), want nothing and a closed connection", b, err)
	}
}

// TestUIRelayIssuerPortal_CutAfterHijackClosesSocket: a 101 on HTTP/1, then a revoke; the socket closes.
func TestUIRelayIssuerPortal_CutAfterHijackClosesSocket(t *testing.T) {
	setRelayWatch(t, relayEvery)
	rig, c := streamRig(t)
	srv, ended := serveGateway(t, rig, gwHTTP1, nil)
	conn := rawRequest(t, srv, gwHTTP1, rig.path("/ws"), c, "Connection: Upgrade\r\nUpgrade: test\r\n")
	line, _ := bufio.NewReader(conn).ReadString('\n')
	if !strings.Contains(line, "101") {
		t.Fatalf("no 101: %q", line)
	}
	at := time.Now()
	rig.revoke()
	cutBy(t, "after the hijack", at, waitEnded(t, ended, 10*time.Second, "after the hijack"), relayEvery+relayBound+relaySlack)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.Copy(io.Discard, conn); isTimeout(err) {
		t.Fatal("the switched socket is still open after the cut")
	}
}

// TestUIRelayIssuerPortal_UpgradeOnHTTP2Refused: the backend answers 101 to an HTTP/2 request, which can
// carry no upgrade; the client gets the proxy's error, as today.
func TestUIRelayIssuerPortal_UpgradeOnHTTP2Refused(t *testing.T) {
	rig, c := streamRig(t)
	srv, _ := serveGateway(t, rig, gwHTTP2, nil)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+rig.path("/rogue101"), nil)
	req.AddCookie(c)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("a 101 relayed over %s: %d, want the proxy's 502", resp.Proto, resp.StatusCode)
	}
}

// TestUIRelayIssuerPortal_PerWriteDeadlineBoundsBlockedClient: with the concurrent interrupt off, a blocked
// streaming answer (HTTP/1 and HTTP/2) and a blocked 101 still end within the check bound plus one
// per-write deadline. Each leg is admitted (its 200 or 101 read) and its write shown pending before the revoke.
func TestUIRelayIssuerPortal_PerWriteDeadlineBoundsBlockedClient(t *testing.T) {
	setRelayWatch(t, relayEvery)
	streamCutNoInterrupt.Store(true)
	streamOpWriteNS.Store(int64(time.Second))
	t.Cleanup(func() { streamCutNoInterrupt.Store(false); streamOpWriteNS.Store(0) })
	bound := relayEvery + relayBound + time.Second + relaySlack
	for _, tc := range []struct {
		name  string
		mode  gatewayMode
		path  string
		extra string
	}{{"HTTP1 stream", gwHTTP1, relayPath, ""}, {"HTTP2 stream", gwHTTP2, relayPath, ""}, {"HTTP1 101", gwHTTP1, "/ws", "Connection: Upgrade\r\nUpgrade: test\r\n"}} {
		t.Run(tc.name, func(t *testing.T) {
			rig, c, in := streamRigLog(t)
			srv, ended := serveGateway(t, rig, tc.mode, nil)
			switch {
			case tc.mode == gwHTTP2:
				h2Unread(t, srv, rig.path(tc.path), c)
			case tc.path == relayPath:
				awaitHeaders(t, rawRequest(t, srv, tc.mode, rig.path(tc.path), c, tc.extra))
			default:
				awaitStatus(t, rawRequest(t, srv, tc.mode, rig.path(tc.path), c, tc.extra), " 101 ")
			}
			requireBlocked(t, in, ended)
			at := time.Now()
			rig.revoke()
			cutBy(t, tc.name+" (per-write fallback)", at, waitEnded(t, ended, 15*time.Second, tc.name), bound)
		})
	}
}

// TestUIRelayIssuerPortal_InterimResponseBoundedByEnd: the backend sends a 103 to a client that is not
// reading, 1 s before the stream's end. The interim write returns by the end, and nothing is written after it.
func TestUIRelayIssuerPortal_InterimResponseBoundedByEnd(t *testing.T) {
	setRelayWatch(t, time.Hour)
	rig := newIssuerUIRig(t, streamBackend(&inputLog{}))
	a := rig.is.grant(rig.principal, uuid.Nil, rig.clock.now().Add(3*time.Second))
	c := rig.session(a)
	end := rig.wall(cookieEnd(a.ExpiresAt)) // renewal off: the cookie's signed expiry, the grant's end in whole seconds
	conns := make(chan *stallConn, 4)
	srv, ended := serveGateway(t, rig, gwHTTP1, conns)
	rawRequest(t, srv, gwHTTP1, rig.path("/early?at="+strconv.FormatInt(end.Add(-time.Second).UnixNano(), 10)), c, "")
	sc := <-conns
	sc.stall(true)
	cutWithin(t, "the stream with a blocked 103 ended", waitEnded(t, ended, 10*time.Second, "blocked 103"), end, relaySlack)
	time.Sleep(200 * time.Millisecond)
	sc.stall(false)
	time.Sleep(100 * time.Millisecond)
	if n := sc.deliveredAfter(end.Add(time.Millisecond)); n != 0 {
		t.Errorf("%d bytes went to the client after the end", n)
	}
}

// TestUIRelayIssuerPortal_CutDuringStalledTLSShutdownStopsInput: a TLS 101 whose client keeps sending but has
// stopped reading, cut by a revoke and, in a subtest, by the stream's end. The backend gets no input sent
// after the cut, and Cut closes the socket without waiting for a close_notify the client would never read.
// One of the renewal gate's measures: a relayed 101 with a client that has stopped reading.
func TestUIRelayIssuerPortal_CutDuringStalledTLSShutdownStopsInput(t *testing.T) {
	setRelayWatch(t, relayEvery)
	for _, by := range []string{"revoke", "end"} {
		t.Run(by, func(t *testing.T) {
			in := &inputLog{}
			rig := newIssuerUIRig(t, streamBackend(in))
			rig.is.renewal = true
			life := time.Hour
			if by == "end" {
				life = 2 * time.Second
			}
			a := rig.is.grant(rig.principal, uuid.Nil, rig.clock.now().Add(life))
			c := rig.session(a)
			srv, ended := serveGateway(t, rig, gwTLS1, nil)
			conn := rawRequest(t, srv, gwTLS1, rig.path("/ws"), c, "Connection: Upgrade\r\nUpgrade: test\r\n")
			line, _ := bufio.NewReader(conn).ReadString('\n')
			if !strings.Contains(line, "101") {
				t.Fatalf("no 101: %q", line)
			}
			sendErr := make(chan time.Time, 1)
			go func() { // keeps sending, never reads again
				for {
					if _, err := fmt.Fprintf(conn, "%d\n", time.Now().UnixNano()); err != nil {
						sendErr <- time.Now()
						return
					}
					time.Sleep(2 * time.Millisecond)
				}
			}()
			event := rig.wall(a.ExpiresAt)
			if by == "revoke" {
				time.Sleep(300 * time.Millisecond)
				event = time.Now()
				rig.revoke()
			}
			cutAt := waitEnded(t, ended, 10*time.Second, by)
			if by == "end" {
				cutWithin(t, "TLS 101 (end): the stream ended", cutAt, event, relaySlack)
			} else {
				cutBy(t, "TLS 101 (revoke)", event, cutAt, relayEvery+relayBound+relaySlack)
			}
			select {
			case at := <-sendErr:
				if at.Sub(cutAt) > 2*time.Second {
					t.Errorf("the client's sends kept succeeding %v past the cut: the socket waited on a TLS close", at.Sub(cutAt))
				}
			case <-time.After(5 * time.Second):
				t.Error("the client could still send 5 s after the cut: the socket is open")
			}
			waitWithin(t, 5*time.Second, "the backend's reader returned", in.finished)
			limit := cutAt
			if by == "end" {
				limit = earlier(cutAt, event.Add(inputGrace))
			}
			if n := in.sentAfter(limit); n != 0 {
				t.Errorf("the backend received %d lines sent after the cut", n)
			}
		})
	}
}

// inputGrace is how long past a known cut point a line may still be stamped and reach the backend: the
// client stamps it before the write the cut then stops.
const inputGrace = 20 * time.Millisecond

// earlier is the earlier of a and b.
func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// TestUIRelayIssuerPortal_QuietOutputInputStopsAtEnd: a 101 whose backend sends nothing, with the viewer still
// sending, crosses its end: only the end stops the input, and no line sent after it reaches the backend.
func TestUIRelayIssuerPortal_QuietOutputInputStopsAtEnd(t *testing.T) {
	setRelayWatch(t, time.Hour) // no re-check due: the end alone must cut
	in := &inputLog{}
	rig := newIssuerUIRig(t, streamBackend(in))
	a := rig.is.grant(rig.principal, uuid.Nil, rig.clock.now().Add(2*time.Second))
	c := rig.session(a)
	srv, ended := serveGateway(t, rig, gwHTTP1, nil)
	conn := rawRequest(t, srv, gwHTTP1, rig.path("/ws"), c, "Connection: Upgrade\r\nUpgrade: test\r\nX-Quiet: 1\r\n")
	if line, _ := bufio.NewReader(conn).ReadString('\n'); !strings.Contains(line, "101") {
		t.Fatalf("no 101: %q", line)
	}
	end := rig.wall(a.ExpiresAt) // as the gateway converted it at admission
	go sendLines(conn)
	waitWithin(t, time.Second, "the viewer's input reached the backend", func() bool { return in.sentAfter(time.Now().Add(-time.Minute)) > 0 })
	at := waitEnded(t, ended, 10*time.Second, "quiet 101")
	cutWithin(t, "quiet 101 (end): the stream ended", at, end, relaySlack)
	waitWithin(t, 5*time.Second, "the backend's reader returned", in.finished)
	if n := in.sentAfter(earlier(at, end.Add(inputGrace))); n != 0 {
		t.Errorf("the backend received %d lines the viewer sent after the end", n)
	}
}
