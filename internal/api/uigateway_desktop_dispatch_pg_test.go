// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// dispatchDialer is a desktop runner whose dials and thaws are recorded: a dial waits on hold (when set,
// whatever its context) before it answers with a QEMU display that sends a Bell every 100ms; a thaw runs thaw.
type dispatchDialer struct {
	*sshFakeRunner
	mu       sync.Mutex
	dials    []time.Time
	thaws    []time.Time
	hold     chan struct{}
	thaw     func(ctx context.Context) error
	displays []net.Conn
}

func (d *dispatchDialer) DialDesktop(_ context.Context, _ string) (io.ReadWriteCloser, error) {
	d.mu.Lock()
	d.dials = append(d.dials, time.Now())
	hold := d.hold
	d.mu.Unlock()
	if hold != nil {
		<-hold
	}
	a, b := net.Pipe()
	d.mu.Lock()
	d.displays = append(d.displays, a)
	d.mu.Unlock()
	go func() {
		defer b.Close()
		qemuDisplay(b, func(c net.Conn) {
			go func() { _, _ = io.Copy(io.Discard, c) }()
			for {
				if _, err := c.Write([]byte{2}); err != nil { // Bell
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
		})
	}()
	return a, nil
}

func (d *dispatchDialer) FreezeSandbox(context.Context, string) error { return nil }

func (d *dispatchDialer) ThawSandbox(ctx context.Context, _ string) error {
	d.mu.Lock()
	d.thaws = append(d.thaws, time.Now())
	thaw := d.thaw
	d.mu.Unlock()
	if thaw != nil {
		return thaw(ctx)
	}
	return nil
}

func newDispatchDesktopRig(t *testing.T) (*dispatchRig, *dispatchDialer, string) {
	t.Helper()
	prig, srv := desktopPGRig(t)
	dd := &dispatchDialer{sshFakeRunner: prig.runner}
	prig.srv.cfg.Runner = dd
	setDispatchFresh(t, dispatchFresh)
	return &dispatchRig{issuerPGRig: prig, fresh: dispatchFresh}, dd, srv.URL
}

// viewerFrames counts the frames v receives until its socket fails or within ends.
func viewerFrames(v *desktopViewer, within time.Duration) int {
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	n := 0
	for {
		if _, _, err := v.ws.Read(ctx); err != nil {
			return n
		}
		n++
	}
}

func TestUIDesktopIssuerPortal_LateDialRefused_PG(t *testing.T) {
	t.Run("the dial", func(t *testing.T) {
		rig, dd, url := newDispatchDesktopRig(t)
		dd.hold = make(chan struct{})
		by := time.Now().Add(rig.fresh)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		v := dialDesktop(t, ctx, rig.uiHarness, url, rig.desktopCookie())
		time.Sleep(time.Until(by) + 200*time.Millisecond)
		close(dd.hold)
		if n := viewerFrames(v, 10*time.Second); n != 0 {
			t.Fatalf("%d frames relayed from a dial that returned after the dispatch time, want 0", n)
		}
		dd.mu.Lock()
		defer dd.mu.Unlock()
		if n := atOrAfter(dd.dials, by); n != 0 {
			t.Fatalf("%d dials started at or after the dispatch time", n)
		}
		for _, d := range dd.displays { // the pending dial is abandoned: its display is closed
			if _, err := d.Write([]byte{0}); err == nil {
				t.Fatal("a display returned after the dispatch time was left open")
			}
		}
	})
	t.Run("the thaw", func(t *testing.T) {
		rig, dd, url := newDispatchDesktopRig(t)
		rig.pausing(true, nil)
		dd.thaw = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
		by := time.Now().Add(rig.fresh)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		v := dialDesktop(t, ctx, rig.uiHarness, url, rig.desktopCookie())
		if n := viewerFrames(v, 10*time.Second); n != 0 {
			t.Fatalf("%d frames on a stream whose thaw stalled past the dispatch time, want 0", n)
		}
		dd.mu.Lock()
		defer dd.mu.Unlock()
		if n := atOrAfter(dd.thaws, by); n != 0 || len(dd.dials) != 0 {
			t.Fatalf("%d thaws at or after the dispatch time, %d dials; want 0, 0", n, len(dd.dials))
		}
	})
}

func TestUIDesktopIssuerPortal_ViewOnlyOutlivesDeadline_PG(t *testing.T) {
	rig, _, url := newDispatchDesktopRig(t)
	by := time.Now().Add(rig.fresh)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	v := dialDesktop(t, ctx, rig.uiHarness, url, rig.desktopCookie())
	v.handshake()
	time.Sleep(time.Until(by) + 2*rig.fresh) // it sends no input meanwhile
	if got := v.read(1); got[0] != 2 {
		t.Fatalf("the stream after the dispatch time: %v, want the display's Bell", got)
	}
}

func TestUIDesktopIssuerPortal_RevokedWhileKeepaliveSucceeds_PG(t *testing.T) {
	rig, _, url := newDispatchDesktopRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	v := dialDesktop(t, ctx, rig.uiHarness, url, rig.desktopCookie())
	v.handshake()
	time.Sleep(time.Until(time.Now().Add(rig.fresh)) + rig.fresh) // the keepalive's TouchRun keeps succeeding
	closed := viewerClosed(v)
	at := time.Now()
	rig.revokePortal(t)
	select {
	case got := <-closed:
		cutBy(t, "desktop, portal revoked while its keepalive succeeds", at, got, relayEvery+relayBound+relaySlack)
	case <-time.After(15 * time.Second):
		t.Fatal("the desktop outlived a portal revoke while its keepalive succeeded")
	}
}
