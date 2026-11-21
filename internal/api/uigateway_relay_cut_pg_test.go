// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The open-stream half of an issuer portal's UI session on Postgres: the events a tick must discover (a portal
// revoke, a suspend, a purge's tombstone, the run's unit leaving the pin) and the known ends (the chain's end
// at admission), on real servers. Ticks are scaled as in uigateway_relay_cut_test.go; the per-write fallback
// keeps its unscaled 30 s, so a cut inside relayEvery + relayBound is the concurrent interrupt's.

// pgEvents are the events a tick must discover, by name.
var pgEvents = map[string]func(*issuerPGRig, *testing.T){
	"portal revoke": (*issuerPGRig).revokePortal,
	"suspend":       (*issuerPGRig).suspend,
}

// openBlocked opens the relay stream at path in mode with a client that has stopped reading.
func openBlocked(t *testing.T, rig *issuerPGRig, mode gatewayMode, path string, c *http.Cookie) chan time.Time {
	t.Helper()
	srv, ended := serveGateway(t, rig.issuerUIRig, mode, nil)
	if mode == gwHTTP2 {
		h2Unread(t, srv, rig.path(path), c)
	} else {
		rawRequest(t, srv, mode, rig.path(path), c, "")
	}
	return ended
}

// TestUIRelayIssuerPortal_BlockedClientWriteCutOnRevoke_PG: a streaming 200 on HTTP/1 and on HTTP/2, its
// client not reading, is cut within one re-check of a portal revoke and of a suspend. One of the renewal
// gate's measures: each subtest logs the stream's end after the event.
func TestUIRelayIssuerPortal_BlockedClientWriteCutOnRevoke_PG(t *testing.T) {
	setSupportWatch(t, relayEvery, relayBound)
	for _, mode := range []gatewayMode{gwHTTP1, gwHTTP2} {
		for name, event := range pgEvents {
			t.Run(mode.String()+"/"+name, func(t *testing.T) {
				rig := newIssuerPGRig(t, streamBackend(&inputLog{}), renews)
				ended := openBlocked(t, rig, mode, relayPath, rig.session(rig.grant))
				requireOpen(t, ended, 500*time.Millisecond) // the client's buffers fill and the gateway's write blocks
				at := time.Now()
				event(rig, t)
				cutBy(t, mode.String()+" stream, client not reading, "+name, at, waitEnded(t, ended, 20*time.Second, name), relayEvery+relayBound+relaySlack)
			})
		}
	}
}

// readLog is what a viewer read: each read's time and size. done closes when its reader has returned.
type readLog struct {
	mu    sync.Mutex
	reads []stallWrite
	done  chan struct{}
}

func newReadLog() *readLog { return &readLog{done: make(chan struct{})} }

func (l *readLog) after(t time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, r := range l.reads {
		if r.at.After(t) {
			n += r.n
		}
	}
	return n
}

// readAll reads r until it fails, logging each read.
func (l *readLog) readAll(r io.Reader) {
	defer close(l.done)
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		l.mu.Lock()
		l.reads = append(l.reads, stallWrite{time.Now(), n})
		l.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// sendLines writes a timestamp line to w every 2 ms until a write fails.
func sendLines(w io.Writer) {
	for {
		if _, err := fmt.Fprintf(w, "%d\n", time.Now().UnixNano()); err != nil {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// chunkLines is a chunked request body whose every chunk is one timestamp line.
type chunkLines struct{ w io.Writer }

func (c chunkLines) Write(p []byte) (int, error) {
	if _, err := fmt.Fprintf(c.w, "%x\r\n%s\r\n", len(p), p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// waitWithin polls cond until it holds, failing after within.
func waitWithin(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(within); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %v", what, within)
		}
	}
}

// TestUIRelayIssuerPortal_BothDirectionsStopAtEnd_PG: a renewable 101 and a streamed request body cross their
// admitted end while the portal keeps minting the person successors. Traffic is shown flowing in each direction
// the case claims (both for the 101, the viewer's input for the body) before the end; once the stream has ended
// and the observers have returned, no viewer input sent after the end reached the backend, and no backend output
// reached the viewer after it. The same with a revoke in place of the end.
func TestUIRelayIssuerPortal_BothDirectionsStopAtEnd_PG(t *testing.T) {
	setSupportWatch(t, relayEvery, relayBound)
	for _, shape := range []string{"101", "request body"} {
		for _, by := range []string{"end", "revoke"} {
			t.Run(shape+"/"+by, func(t *testing.T) {
				in := &inputLog{}
				rig := newIssuerPGRig(t, streamBackend(in), renews)
				life := 3 * time.Second
				if by == "revoke" {
					life = time.Hour
				}
				end := rig.endIn(t, rig.grant, life)
				c := rig.session(rig.grant)
				srv, ended := serveGateway(t, rig.issuerUIRig, gwHTTP1, nil)
				var out *readLog
				if shape == "101" {
					conn := rawRequest(t, srv, gwHTTP1, rig.path("/ws"), c, "Connection: Upgrade\r\nUpgrade: test\r\n")
					br := bufio.NewReader(conn)
					if line, _ := br.ReadString('\n'); !strings.Contains(line, "101") {
						t.Fatalf("no 101: %q", line)
					}
					skipHeaders(br)
					out = newReadLog()
					go out.readAll(br)
					go sendLines(conn)
				} else {
					conn := rawMethod(t, srv, gwHTTP1, http.MethodPost, rig.path("/body"), c, "Transfer-Encoding: chunked\r\n")
					go sendLines(chunkLines{conn})
				}
				// Admitted and flowing in every claimed direction, before any successor or event.
				opened := time.Now()
				waitWithin(t, time.Second, "the viewer's input reached the backend", func() bool { return in.sentAfter(opened.Add(-time.Minute)) > 0 })
				if out != nil {
					waitWithin(t, time.Second, "the backend's output reached the viewer", func() bool { return out.after(opened.Add(-time.Minute)) > 0 })
				}
				stop := make(chan struct{})
				go func() { // the portal keeps exchanging
					for {
						select {
						case <-stop:
							return
						case <-time.After(300 * time.Millisecond):
							_ = rig.mintQuiet(time.Now().Add(time.Hour)) // refused once the portal is revoked
						}
					}
				}()
				t.Cleanup(func() { close(stop) })
				if by == "revoke" {
					time.Sleep(500 * time.Millisecond)
					end = time.Now()
					rig.revokePortal(t)
				}
				at := waitEnded(t, ended, 20*time.Second, shape+" "+by)
				if by == "end" {
					cutWithin(t, shape+" (end): the stream ended", at, end, relaySlack)
				} else {
					cutBy(t, shape+" (revoke)", end, at, relayEvery+relayBound+relaySlack)
				}
				if in.sentAfter(end.Add(-time.Minute)) == 0 {
					t.Fatal("no viewer input crossed the stream at all")
				}
				waitWithin(t, 5*time.Second, "the backend's reader returned", in.finished)
				limit := at // a revoke's cut lands somewhere inside its window: the handler's end bounds it
				if by == "end" {
					limit = earlier(at, end.Add(inputGrace)) // the end is the cut point
				}
				if n := in.sentAfter(limit); n != 0 {
					t.Errorf("the backend received %d lines the viewer sent after the stream ended", n)
				}
				if out != nil {
					select {
					case <-out.done:
					case <-time.After(5 * time.Second):
						t.Fatal("the viewer's reader never returned")
					}
					if n := out.after(at.Add(relaySlack)); n != 0 {
						t.Errorf("the viewer read %d bytes after the stream ended", n)
					}
				}
			})
		}
	}
}

// TestUIRenewal_ChainEndJustAfterCleanTickCutsAtEnd_PG: the chain's end falls just after a clean tick, with no
// successor; the stream is cut at the end, not at the next tick.
func TestUIRenewal_ChainEndJustAfterCleanTickCutsAtEnd_PG(t *testing.T) {
	setSupportWatch(t, relayEvery, relayBound)
	t.Run("no successor", func(t *testing.T) {
		rig := newIssuerPGRig(t, streamBackend(&inputLog{}), renews)
		end := rig.endIn(t, rig.grant, relayEvery+relayEvery/10) // a tick at relayEvery passes clean
		ended := openBlocked(t, rig, gwHTTP1, relayPath, rig.session(rig.grant))
		at := waitEnded(t, ended, 10*time.Second, "chain end")
		cutWithin(t, "the stream ended", at, end, relaySlack)
	})
}

// TestUIRenewal_BlockedClientCutAtChainEnd_PG: a renewable stream whose client has stopped reading, on HTTP/1
// and on HTTP/2, is cut at the end its chain had at admission.
func TestUIRenewal_BlockedClientCutAtChainEnd_PG(t *testing.T) {
	setSupportWatch(t, time.Hour, relayBound)
	for _, mode := range []gatewayMode{gwHTTP1, gwHTTP2} {
		t.Run(mode.String(), func(t *testing.T) {
			rig := newIssuerPGRig(t, streamBackend(&inputLog{}), renews)
			end := rig.endIn(t, rig.grant, 2*time.Second)
			ended := openBlocked(t, rig, mode, relayPath, rig.session(rig.grant))
			cutWithin(t, mode.String()+": the blocked stream ended", waitEnded(t, ended, 10*time.Second, "chain end"), end, relaySlack)
		})
	}
}

// skipHeaders reads br past the end of a response's header block.
func skipHeaders(br *bufio.Reader) {
	for {
		line, err := br.ReadString('\n')
		if err != nil || line == "\r\n" {
			return
		}
	}
}

// mintQuiet mints the person a further grant capped by signedEnd and returns the mint's error: for a goroutine
// that keeps the portal exchanging.
func (rig *issuerPGRig) mintQuiet(signedEnd time.Time) error {
	_, _, err := rig.env.st.MintPortalGrant(context.Background(), types.PortalGrant{DelegateID: rig.d.ID, Principal: rig.principal,
		UserType: rig.d.UserType, SubjectIssuedAt: time.Now().UTC()}, delegatedTokenPrefix+fmt.Sprint(time.Now().UnixNano()), signedEnd, 10*time.Minute, rig.d.AdmissionRev)
	return err
}

// successorTo mints the person a further grant capped by signedEnd, as an exchange of one subject token whose
// iat + max age (or exp) is signedEnd does.
func (rig *issuerPGRig) successorTo(t *testing.T, signedEnd time.Time) {
	t.Helper()
	if err := rig.mintQuiet(signedEnd); err != nil {
		t.Fatalf("mint a successor: %v", err)
	}
}

// TestUIRenewal_OpenStreamEndsAtAdmittedEndWhilePortalExchanges_PG: successors commit throughout, the last
// from the same last subject token. The open stream ends at its admitted end; the app's next request is served
// and opens a new stream, which ends at that token's signed end: no stream outlives it.
func TestUIRenewal_OpenStreamEndsAtAdmittedEndWhilePortalExchanges_PG(t *testing.T) {
	setSupportWatch(t, relayEvery, relayBound)
	rig := newIssuerPGRig(t, streamBackend(&inputLog{}), renews)
	admitted := rig.endIn(t, rig.grant, 2*time.Second)
	c := rig.session(rig.grant)
	signedEnd := time.Now().Add(8 * time.Second)
	srv, ended := serveGateway(t, rig.issuerUIRig, gwHTTP1, nil)
	// Admitted before any successor; then successors while each still has five seconds to live.
	awaitHeaders(t, rawRequest(t, srv, gwHTTP1, rig.path(relayPath), c, ""))
	for time.Until(signedEnd) > 6*time.Second {
		rig.successorTo(t, signedEnd)
		time.Sleep(300 * time.Millisecond)
	}
	cutWithin(t, "the first stream ended", waitEnded(t, ended, 10*time.Second, "admitted end"), admitted, relaySlack)
	if r := rig.relay(c); r.Code != http.StatusOK {
		t.Fatalf("the app's next request after the first stream's end: %d %s", r.Code, r.Body.String())
	}
	srv2, ended2 := serveGateway(t, rig.issuerUIRig, gwHTTP1, nil)
	rawRequest(t, srv2, gwHTTP1, rig.path(relayPath), c, "")
	requireOpen(t, ended2, 300*time.Millisecond)
	at := waitEnded(t, ended2, 15*time.Second, "signed end")
	if at.After(signedEnd.Add(relaySlack)) {
		t.Fatalf("the second stream ended %v past the token's signed end", at.Sub(signedEnd))
	}
}

// eventRefusesEverything opens a stream, commits event and checks the stream is cut within one re-check and
// that a ticket minted before the event, a new relay request and a desktop open are all refused.
func eventRefusesEverything(t *testing.T, event func(*issuerPGRig, *testing.T), what string) {
	t.Helper()
	setSupportWatch(t, relayEvery, relayBound)
	rig := newIssuerPGRig(t, streamBackend(&inputLog{}))
	c := rig.session(rig.grant)
	ticket := rig.ticket(rig.grant)
	desk := rig.desktopCookie()
	ended := openBlocked(t, rig, gwHTTP1, relayPath, c)
	requireOpen(t, ended, 300*time.Millisecond)
	at := time.Now()
	event(rig, t)
	if rec := rig.enterWith(ticket); rec.Code != http.StatusForbidden || len(setUICookies(rec)) != 0 {
		t.Errorf("%s: a ticket redemption: %d, want 403", what, rec.Code)
	}
	if r := rig.relay(c); r.Code != http.StatusForbidden {
		t.Errorf("%s: a new relay request: %d, want 403", what, r.Code)
	}
	if code := rig.desktopOpen(desk); code != http.StatusForbidden {
		t.Errorf("%s: a desktop open: %d, want 403", what, code)
	}
	cutBy(t, what+": the open stream", at, waitEnded(t, ended, 10*time.Second, what), relayEvery+relayBound+relaySlack)
}

// TestPortalPurge_TombstoneWithLiveGrantRefusedOnTicketAndRelay_PG: the person's tombstone is committed, the
// grant left live and the rest of the purge held; a ticket redemption, a new relay request and a desktop open
// are refused, and an open stream is cut.
func TestPortalPurge_TombstoneWithLiveGrantRefusedOnTicketAndRelay_PG(t *testing.T) {
	eventRefusesEverything(t, (*issuerPGRig).tombstone, "tombstone")
}

// TestPortalPin_ReparentOutCutsTicketRelayAndStream_PG: the run's unit is re-parented out of the portal's pin;
// a ticket redemption, a new relay request and a desktop open are refused, and an open stream is cut.
func TestPortalPin_ReparentOutCutsTicketRelayAndStream_PG(t *testing.T) {
	eventRefusesEverything(t, (*issuerPGRig).moveOut, "unit move")
}
