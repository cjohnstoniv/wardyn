// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An issuer portal's desktop stream on Postgres: its re-check runs from the opening check, beside the stream,
// so neither a stalled setup (the start lock, the dial) nor a slow Closed-set read delays it. Re-checks are
// scaled to relayEvery.

// stallingDialer is a runner whose desktop dial waits for its context, as a display that never answers does.
type stallingDialer struct {
	*sshFakeRunner
	dialed chan time.Time
}

func (f *stallingDialer) DialDesktop(ctx context.Context, _ string) (io.ReadWriteCloser, error) {
	f.dialed <- time.Now()
	<-ctx.Done()
	return nil, ctx.Err()
}

// slowClosedStore answers the Closed set after delay, once slow is set, or fails when ctx ends first.
type slowClosedStore struct {
	issuerPGStore
	slow    *atomic.Bool
	delay   time.Duration
	entered *atomic.Int64 // counts the slow reads begun, when set
}

func (s slowClosedStore) UIDesktopClosed(ctx context.Context, key string, now time.Time) (bool, error) {
	if s.slow.Load() {
		if s.entered != nil {
			s.entered.Add(1)
		}
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return s.issuerPGStore.UIDesktopClosed(ctx, key, now)
}

// desktopPGRig is the issuer PG rig on a Linux VM run, its gateway on a real server.
func desktopPGRig(t *testing.T) (*issuerPGRig, *httptest.Server) {
	t.Helper()
	rig := newIssuerPGRig(t, okBackend())
	rig.run.MachineKind = types.MachineLinuxVM
	rig.store.putRun(rig.run)
	rig.srv.uiDesktop.recheckEvery = relayEvery
	srv := httptest.NewServer(rig.gateway)
	t.Cleanup(srv.Close)
	return rig, srv
}

// viewerClosed reads v's socket until it fails and returns when it did.
func viewerClosed(v *desktopViewer) <-chan time.Time {
	closed := make(chan time.Time, 1)
	go func() {
		for {
			if _, _, err := v.ws.Read(context.Background()); err != nil {
				closed <- time.Now()
				return
			}
		}
	}()
	return closed
}

// TestUIDesktopIssuerPortal_StalledSetupCutWithin40s_PG: the display's dial stalls; a suspend during it ends
// the stream within one re-check and its bound.
func TestUIDesktopIssuerPortal_StalledSetupCutWithin40s_PG(t *testing.T) {
	rig, srv := desktopPGRig(t)
	rig.srv.uiDesktop.recheckWait = relayBound
	dr := &stallingDialer{sshFakeRunner: rig.runner, dialed: make(chan time.Time, 1)}
	rig.srv.cfg.Runner = dr
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	v := dialDesktop(t, ctx, rig.uiHarness, srv.URL, rig.desktopCookie())
	closed := viewerClosed(v)
	select {
	case <-dr.dialed:
	case <-time.After(5 * time.Second):
		t.Fatal("the display was never dialed")
	}
	at := time.Now()
	rig.suspend(t)
	select {
	case got := <-closed:
		cutBy(t, "desktop, suspend during a stalled dial", at, got, relayEvery+relayBound+relaySlack)
	case <-time.After(15 * time.Second):
		t.Fatal("the desktop outlived a suspend during its setup")
	}
}

// TestUIDesktopIssuerPortal_SlowClosedSetLookupDoesNotDelayRecheck_PG: every Closed-set read takes most of its
// bound, read back to back; a suspend still ends the stream within one re-check and a fast lookup, not behind
// a Closed-set read.
func TestUIDesktopIssuerPortal_SlowClosedSetLookupDoesNotDelayRecheck_PG(t *testing.T) {
	rig, srv := desktopPGRig(t)
	const bound = 2 * time.Second
	rig.srv.uiDesktop.recheckWait = bound
	rig.srv.uiDesktopClosedEvery = 50 * time.Millisecond
	slow := &atomic.Bool{}
	rig.srv.cfg.Store = slowClosedStore{issuerPGStore: rig.srv.cfg.Store.(issuerPGStore), slow: slow, delay: bound * 9 / 10}
	dr := &desktopFakeRunner{sshFakeRunner: rig.runner, dialled: make(chan string, 8)}
	rig.srv.cfg.Runner = dr
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	v := dialDesktop(t, ctx, rig.uiHarness, srv.URL, rig.desktopCookie())
	v.handshake()
	slow.Store(true)
	time.Sleep(bound) // the stream's loop is inside a slow Closed-set read
	closed := viewerClosed(v)
	at := time.Now()
	rig.suspend(t)
	select {
	case got := <-closed:
		cutBy(t, "desktop, suspend beside a slow Closed-set read", at, got, relayEvery+relaySlack)
	case <-time.After(15 * time.Second):
		t.Fatal("the desktop outlived a suspend")
	}
}

// TestUIDesktopIssuerPortal_AdmissionClosedReadHeldPastBound_PG: the desktop's opening Closed-set read hangs;
// the handshake is answered 503, retryable, within the admission bound, after the read began, not held for as
// long as the store takes.
func TestUIDesktopIssuerPortal_AdmissionClosedReadHeldPastBound_PG(t *testing.T) {
	rig, srv := desktopPGRig(t)
	const bound = 200 * time.Millisecond
	setSupportWatch(t, time.Hour, bound)
	slow := &atomic.Bool{}
	slow.Store(true)
	entered := &atomic.Int64{}
	rig.srv.cfg.Store = slowClosedStore{issuerPGStore: rig.srv.cfg.Store.(issuerPGStore), slow: slow, delay: time.Minute, entered: entered}
	rig.srv.cfg.Runner = &desktopFakeRunner{sshFakeRunner: rig.runner, dialled: make(chan string, 8)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := rig.desktopCookie()
	start := time.Now()
	ws, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+desktopPath(rig.uiHarness, uiDesktopRFB),
		&websocket.DialOptions{HTTPHeader: http.Header{"Cookie": {c.Name + "=" + c.Value}}})
	took := time.Since(start)
	if err == nil {
		_ = ws.CloseNow()
		t.Fatal("a desktop whose Closed-set read hung was opened")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a hung Closed-set read: %v %v, want the retryable 503", resp, err)
	}
	if entered.Load() == 0 {
		t.Fatal("the handshake never reached the Closed-set read: the refusal came from elsewhere")
	}
	if took > bound+400*time.Millisecond {
		t.Fatalf("the open was held %v by a hung Closed-set read, want within %v", took, bound+400*time.Millisecond)
	}
}

// heldDesktopReadStore holds the desktop's opening reads until ctx ends: the rules read (the run's policy
// rows) once policy is set, and once signIn is set the run read after the rules read, which is the sign-in
// watch's (watchVMSignIn reads on, for a store that keeps take overs).
type heldDesktopReadStore struct {
	issuerPGStore
	policy, signIn, armed *atomic.Bool
}

func (s heldDesktopReadStore) QueryAuditEvents(ctx context.Context, runID uuid.UUID, limit int) ([]types.AuditEvent, error) {
	if s.policy.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	events, err := s.issuerPGStore.QueryAuditEvents(ctx, runID, limit)
	if s.signIn.Load() {
		s.armed.Store(true)
	}
	return events, err
}

func (s heldDesktopReadStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	if s.armed.Load() {
		<-ctx.Done()
		return types.AgentRun{}, ctx.Err()
	}
	return s.issuerPGStore.GetRun(ctx, id)
}

func (s heldDesktopReadStore) OpenTakeovers(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

// TestUIDesktopIssuerPortal_OpeningRulesAndSignInReadsHeld_PG: after admission, the desktop's rules read or its
// sign-in watch's run read hangs. Each is answered with its own 503 within the session's opening bound, never
// held for as long as the store takes.
func TestUIDesktopIssuerPortal_OpeningRulesAndSignInReadsHeld_PG(t *testing.T) {
	const bound = 200 * time.Millisecond
	for _, tc := range []struct {
		name, reason string
		policy       bool
	}{
		{"rules", reasonDesktopRulesUnreadable, true},
		{"sign-ins", reasonDesktopSignInsUnavailable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig, srv := desktopPGRig(t)
			setSupportWatch(t, time.Hour, bound)
			st := heldDesktopReadStore{issuerPGStore: rig.srv.cfg.Store.(issuerPGStore), policy: &atomic.Bool{}, signIn: &atomic.Bool{}, armed: &atomic.Bool{}}
			st.policy.Store(tc.policy)
			st.signIn.Store(!tc.policy)
			rig.srv.cfg.Store = st
			rig.srv.cfg.Runner = &desktopFakeRunner{sshFakeRunner: rig.runner, dialled: make(chan string, 8)}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			c := rig.desktopCookie()
			start := time.Now()
			ws, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+desktopPath(rig.uiHarness, uiDesktopRFB),
				&websocket.DialOptions{HTTPHeader: http.Header{"Cookie": {c.Name + "=" + c.Value}}})
			took := time.Since(start)
			if err == nil {
				_ = ws.CloseNow()
				t.Fatalf("%s: a desktop whose read hung was opened", tc.name)
			}
			if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("%s: %v %v, want 503", tc.name, resp, err)
			}
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), tc.reason) {
				t.Fatalf("%s: answered %s, want %s: the refusal came from another read", tc.name, body, tc.reason)
			}
			if took > bound+400*time.Millisecond {
				t.Fatalf("%s: the open was held %v, want within %v", tc.name, took, bound+400*time.Millisecond)
			}
		})
	}
}
