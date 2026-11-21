// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// An issuer portal's run-events stream on Postgres: its authority runs beside it every beat, each lookup
// bounded, and an event that removes the authority ends it within one beat and the bound (20 s unscaled),
// a client that has stopped reading and a lookup that hangs included. The beat is scaled to eventsBeat and the
// bound to eventsBound; the batch write deadline keeps its unscaled 30 s.

const (
	eventsBeat  = 300 * time.Millisecond
	eventsBound = 300 * time.Millisecond
)

// issuerEventsEnv is an issuer portal pinned to the platform unit, one of its people with a live grant, and
// that person's running run in the platform team, the API served on a real server.
type issuerEventsEnv struct {
	*authorityEnv
	d         types.Delegate
	principal string
	raw       string
	run       types.AgentRun
}

func newIssuerEventsEnv(t *testing.T) *issuerEventsEnv {
	t.Helper()
	e := newAuthorityEnv(t)
	d := e.portal(t, e.bind(t), &e.plat.ID)
	principal, raw := e.person(t, d, "person-1")
	run := e.run(t, principal, &e.team.ID, d.ID)
	e.h.srv.runEvents.beat = eventsBeat
	setSupportWatch(t, time.Hour, eventsBound)
	return &issuerEventsEnv{authorityEnv: e, d: d, principal: principal, raw: raw, run: run}
}

func (e *issuerEventsEnv) exec(t *testing.T, what, sql string, args ...any) {
	t.Helper()
	if _, err := e.st.Pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// issuerEventsEvents are the events the keepalive check must see, by name.
func issuerEventsEvents() map[string]func(*issuerEventsEnv, *testing.T) {
	return map[string]func(*issuerEventsEnv, *testing.T){
		"grant expires": func(e *issuerEventsEnv, t *testing.T) {
			e.exec(t, "expire the grant", `UPDATE portal_grants SET expires_at = clock_timestamp() WHERE principal = $1`, e.principal)
		},
		"portal revoked": func(e *issuerEventsEnv, t *testing.T) {
			if _, _, err := e.st.RevokeDelegate(context.Background(), e.d.ID, time.Now(), false); err != nil {
				t.Fatalf("revoke the portal: %v", err)
			}
		},
		"person suspended": func(e *issuerEventsEnv, t *testing.T) {
			e.exec(t, "suspend", `UPDATE principal_identities SET deactivated_at = now() WHERE principal = $1`, e.principal)
		},
		"person purged": func(e *issuerEventsEnv, t *testing.T) {
			tombstonePerson(t, e.st, e.principal)
			if _, err := e.st.GetPortalGrantByRaw(context.Background(), e.raw, time.Now()); err != nil {
				t.Fatalf("the grant did not stay live across the tombstone: %v", err)
			}
		},
		"unit moves out of the pin": func(e *issuerEventsEnv, t *testing.T) {
			team := e.team
			team.ParentID = &e.other.ID
			if _, err := e.st.UpsertGovernanceUnit(context.Background(), team); err != nil {
				t.Fatalf("move the run's unit: %v", err)
			}
		},
	}
}

// openIssuerEvents opens the run's event stream with the person's grant and waits for one keepalive.
func (e *issuerEventsEnv) openIssuerEvents(t *testing.T) *bufio.Reader {
	t.Helper()
	ts := httptest.NewServer(panicFails(t, e.h.srv.Handler()))
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	resp := openEventsAs(t, ctx, ts.URL, e.run.ID, e.raw)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("open the stream: %d", resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("the stream ended before its first keepalive: %v", err)
		}
		if strings.HasPrefix(line, ": keepalive") {
			return br // one successful heartbeat
		}
	}
}

// endedBy reads br until the stream ends and returns when it did.
func endedBy(t *testing.T, br *bufio.Reader, within time.Duration) time.Time {
	t.Helper()
	done := make(chan time.Time, 1)
	go func() {
		for {
			if _, err := br.ReadString('\n'); err != nil {
				done <- time.Now()
				return
			}
		}
	}()
	select {
	case at := <-done:
		return at
	case <-time.After(within):
		t.Fatalf("the stream was still open %v later", within)
		return time.Time{}
	}
}

// TestRunEventsIssuerPortal_KeepaliveChecks_PG: one successful heartbeat, then, each in a subtest, the grant
// expires, the portal is revoked, the person is suspended or purged, or the run's unit moves out of the pin;
// the stream ends within one beat and the bound each time.
func TestRunEventsIssuerPortal_KeepaliveChecks_PG(t *testing.T) {
	for name, event := range issuerEventsEvents() {
		t.Run(name, func(t *testing.T) {
			e := newIssuerEventsEnv(t)
			br := e.openIssuerEvents(t)
			at := time.Now()
			event(e, t)
			cutBy(t, "event stream, "+name, at, endedBy(t, br, 10*time.Second), eventsBeat+eventsBound+relaySlack)
		})
	}
}

// streamSlotFreed waits for srv's last event stream to end and returns when it did.
func streamSlotFreed(t *testing.T, srv *Server, within time.Duration) time.Time {
	t.Helper()
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if !streamSlotHeld(srv) {
			return time.Now()
		}
	}
	t.Fatalf("the stream still held its slot %v later", within)
	return time.Time{}
}

// streamSlotHeld reports whether srv holds an event stream open.
func streamSlotHeld(srv *Server) bool {
	srv.runEvents.mu.Lock()
	defer srv.runEvents.mu.Unlock()
	return len(srv.runEvents.streams) > 0
}

// openZeroWindow opens the run's event stream over HTTP/2 with bearer and a client that grants it no window,
// and returns the client connection once the stream's HEADERS answered 200.
func openZeroWindow(t *testing.T, ts *httptest.Server, runID string, bearer string) *tls.Conn {
	t.Helper()
	addr := ts.Listener.Addr().String()
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, NextProtos: []string{"h2"}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatal(err)
	}
	fr := http2.NewFramer(conn, conn)
	if err := fr.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 0}); err != nil {
		t.Fatal(err)
	}
	var block bytes.Buffer
	enc := hpack.NewEncoder(&block)
	for _, f := range [][2]string{
		{":method", http.MethodGet}, {":scheme", "https"}, {":authority", addr},
		{":path", "/api/v1/runs/" + runID + "/events"}, {"authorization", "Bearer " + bearer},
	} {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: block.Bytes(), EndStream: true, EndHeaders: true}); err != nil {
		t.Fatal(err)
	}
	status := make(chan string, 1)
	go func() { // read and drop every frame; never grant a window
		dec := hpack.NewDecoder(4096, nil)
		for once := false; ; {
			f, err := fr.ReadFrame()
			if err != nil {
				return
			}
			if h, ok := f.(*http2.HeadersFrame); ok && h.StreamID == 1 && !once {
				once = true
				fields, _ := dec.DecodeFull(h.HeaderBlockFragment())
				st := ""
				for _, hf := range fields {
					if hf.Name == ":status" {
						st = hf.Value
					}
				}
				status <- st
			}
		}
	}()
	select {
	case st := <-status:
		if st != "200" {
			t.Fatalf("the stream answered %q, want 200: not admitted", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never answered")
	}
	return conn
}

// TestRunEventsIssuerPortal_BlockedWriterCutWithin20s_PG: an HTTP/2 client that grants the stream no window
// blocks the handler's first flush, under the unscaled 30 s batch deadline. The stream answered 200 and still
// holds its slot just before a portal revoke, which ends the handler within one beat and the bound. Negative
// control: a client that disconnects early frees the slot before any event, which the pre-event check refuses
// to count as a cut.
func TestRunEventsIssuerPortal_BlockedWriterCutWithin20s_PG(t *testing.T) {
	t.Run("revoke", func(t *testing.T) {
		e := newIssuerEventsEnv(t)
		ts := newH2EventsServer(t, e.h.srv)
		openZeroWindow(t, ts, e.run.ID.String(), e.raw)
		time.Sleep(2 * eventsBeat) // past a clean tick, the handler blocked in its flush
		if !streamSlotHeld(e.h.srv) {
			t.Fatal("the stream was not open and blocked just before the event")
		}
		at := time.Now()
		if _, _, err := e.st.RevokeDelegate(context.Background(), e.d.ID, time.Now(), false); err != nil {
			t.Fatalf("revoke the portal: %v", err)
		}
		cutBy(t, "HTTP2 event stream, client not reading", at, streamSlotFreed(t, e.h.srv, 20*time.Second), eventsBeat+eventsBound+relaySlack)
	})
	t.Run("early disconnect is no cut", func(t *testing.T) {
		e := newIssuerEventsEnv(t)
		ts := newH2EventsServer(t, e.h.srv)
		conn := openZeroWindow(t, ts, e.run.ID.String(), e.raw)
		_ = conn.Close()
		streamSlotFreed(t, e.h.srv, 5*time.Second)
		if streamSlotHeld(e.h.srv) { // the revoke case's pre-event check refuses this state
			t.Fatal("the pre-event check would count a disconnected stream as open")
		}
	})
}

// flakyEventsStore is the env's Postgres store with the failures a test switches on: the person's identity that
// cannot be read, and the run that is gone, cannot be read, or whose read hangs until ctx ends.
type flakyEventsStore struct {
	store.PG
	identityFails, runGone, runFails, runStall *atomic.Bool
}

func newFlakyEventsStore(pg store.PG) flakyEventsStore {
	return flakyEventsStore{PG: pg, identityFails: &atomic.Bool{}, runGone: &atomic.Bool{}, runFails: &atomic.Bool{}, runStall: &atomic.Bool{}}
}

func (f flakyEventsStore) IdentityBlocked(ctx context.Context, principal string, epoch int64) (bool, error) {
	if f.identityFails.Load() {
		return false, errors.New("identity read failed")
	}
	return f.PG.IdentityBlocked(ctx, principal, epoch)
}

func (f flakyEventsStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	switch {
	case f.runGone.Load():
		return types.AgentRun{}, store.ErrNotFound
	case f.runFails.Load():
		return types.AgentRun{}, errors.New("run read failed")
	case f.runStall.Load():
		<-ctx.Done()
		return types.AgentRun{}, ctx.Err()
	}
	return f.PG.GetRun(ctx, id)
}

// TestRunEventsIssuerPortal_PinChangeDuringDelayedSetupReleasesNoBacklog_PG: while the stream's setup is delayed
// past a beat, after its authorization read, the run's unit moves out of the pin (403), the person's identity
// cannot be read (503, retryable), or the run goes (404, as the read gate answers a missing run); none releases
// the backlog. Control: the same delay with nothing changed serves the backlog.
func TestRunEventsIssuerPortal_PinChangeDuringDelayedSetupReleasesNoBacklog_PG(t *testing.T) {
	for _, tc := range []struct {
		name   string
		during func(e *issuerEventsEnv, f flakyEventsStore, t *testing.T)
		want   int
	}{
		{"unit moves out", func(e *issuerEventsEnv, _ flakyEventsStore, t *testing.T) {
			team := e.team
			team.ParentID = &e.other.ID
			if _, err := e.st.UpsertGovernanceUnit(context.Background(), team); err != nil {
				t.Errorf("move the run's unit: %v", err)
			}
		}, http.StatusForbidden},
		{"identity unreadable", func(_ *issuerEventsEnv, f flakyEventsStore, _ *testing.T) { f.identityFails.Store(true) }, http.StatusServiceUnavailable},
		{"run gone", func(_ *issuerEventsEnv, f flakyEventsStore, _ *testing.T) { f.runGone.Store(true) }, http.StatusNotFound},
		{"control", func(*issuerEventsEnv, flakyEventsStore, *testing.T) {}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newIssuerEventsEnv(t)
			f := newFlakyEventsStore(e.st)
			e.h.srv.cfg.Store = f
			e.h.srv.runEvents.emit(e.run.ID, client.RunEvent{Type: client.RunEventPulling, At: time.Now()}) // the backlog
			e.h.srv.runEvents.onSetup = func() {
				tc.during(e, f, t)
				time.Sleep(2 * eventsBeat)
			}
			ts := httptest.NewServer(panicFails(t, e.h.srv.Handler()))
			t.Cleanup(ts.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			resp := openEventsAs(t, ctx, ts.URL, e.run.ID, e.raw)
			if tc.want == http.StatusOK {
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("control: %d", resp.StatusCode)
				}
				br := bufio.NewReader(resp.Body)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						t.Fatalf("control: the backlog never came: %v", err)
					}
					if strings.Contains(line, client.RunEventPulling) {
						return
					}
				}
			}
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.want || strings.Contains(string(body), client.RunEventPulling) {
				t.Fatalf("%s during setup: %d %q, want %d and no backlog", tc.name, resp.StatusCode, body, tc.want)
			}
		})
	}
}

// TestRunEventsIssuerPortal_FirstRunLookupAnswers_PG: the stream's first run read, at its opening, finds no run
// (404, as a missing run is answered), cannot be read (503, retryable), or hangs (the same 503 within the opening
// bound); none opens the stream.
func TestRunEventsIssuerPortal_FirstRunLookupAnswers_PG(t *testing.T) {
	for _, tc := range []struct {
		name   string
		set    func(f flakyEventsStore)
		want   int
		reason string
	}{
		{"missing", func(f flakyEventsStore) { f.runGone.Store(true) }, http.StatusNotFound, reasonRunNotFound},
		{"unreadable", func(f flakyEventsStore) { f.runFails.Store(true) }, http.StatusServiceUnavailable, uiDeniedReasonRevocationUnavailable},
		{"stall", func(f flakyEventsStore) { f.runStall.Store(true) }, http.StatusServiceUnavailable, uiDeniedReasonRevocationUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newIssuerEventsEnv(t)
			f := newFlakyEventsStore(e.st)
			tc.set(f)
			e.h.srv.cfg.Store = f
			ts := httptest.NewServer(panicFails(t, e.h.srv.Handler()))
			t.Cleanup(ts.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			start := time.Now()
			resp := openEventsAs(t, ctx, ts.URL, e.run.ID, e.raw)
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.want || !strings.Contains(string(body), tc.reason) {
				t.Fatalf("%s: %d %q, want %d %s", tc.name, resp.StatusCode, body, tc.want, tc.reason)
			}
			if d := time.Since(start); d > eventsBound+400*time.Millisecond {
				t.Errorf("%s: answered %v after the open, want within the %v bound", tc.name, d, eventsBound)
			}
		})
	}
}

// TestRunEventsIssuerPortal_BlockedLookupStillEnds_PG: the grant table is locked, so the next tick's lookup
// hangs; the lookup's bound ends it and the stream with it, within one beat and the bound.
func TestRunEventsIssuerPortal_BlockedLookupStillEnds_PG(t *testing.T) {
	e := newIssuerEventsEnv(t)
	br := e.openIssuerEvents(t)
	tx, err := e.st.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	at := time.Now()
	if _, err := tx.Exec(context.Background(), `LOCK TABLE portal_grants IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock the grants: %v", err)
	}
	cutBy(t, "event stream, hung lookup", at, endedBy(t, br, 10*time.Second), eventsBeat+eventsBound+relaySlack)
}
