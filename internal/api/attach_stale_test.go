// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A writer whose socket is black-holed (its tab died without a close) is evicted
// through the audited take-over path and the same person's new tab is promoted
// in under 5 seconds.
func TestAttachStale_BlackHoledWriterIsEvictedAndNewcomerPromoted(t *testing.T) {
	srv, _, _, audit, run := holderTestServer(t)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	defer ts.Close()

	c1 := dialAttach(t, ts, srv, run.ID, holderOwner, "")
	readAttachMode(t, c1) // then c1 goes silent: no Read, so no pong ever
	waitFor(t, "the writer to be ready", func() bool {
		w := srv.attachHolderFor(run.ID)
		return w != nil && w.ready.isReady()
	})
	dead := srv.attachHolderFor(run.ID)

	start := time.Now()
	c2 := dialAttach(t, ts, srv, run.ID, holderOwner, "")
	if mode := readAttachMode(t, c2); !mode.ReadOnly {
		t.Fatal("the newcomer was admitted writable behind a live writer")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for {
		typ, data, err := c2.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for the promotion frame: %v", err)
		}
		if typ == websocket.MessageText && strings.Contains(string(data), `"read_only":false`) {
			break
		}
	}
	if took := time.Since(start); took >= 5*time.Second {
		t.Fatalf("promotion took %v, want under 5s", took)
	}
	if !dead.evicted.Load() || dead.canWrite() {
		t.Error("the stale writer kept write authority")
	}
	if w := srv.attachHolderFor(run.ID); w == nil || w == dead || !w.canWrite() {
		t.Errorf("writer after eviction = %+v, want the newcomer", w)
	}

	ev := waitForAudit(t, audit, run.ID, "session.takeover", "success")
	if ev == nil {
		t.Fatalf("no session.takeover audit; events = %s", auditDump(audit.snapshot(), run.ID))
	}
	if d := string(ev.Data); !strings.Contains(d, `"reason":"stale_writer"`) || !strings.Contains(d, `"previous_holder":"`+holderOwner+`"`) {
		t.Errorf("session.takeover data = %s, want reason stale_writer naming %s", d, holderOwner)
	}
	if ev.Actor != holderOwner {
		t.Errorf("session.takeover actor = %q, want the newcomer's principal", ev.Actor)
	}
}

// A writer that is slow but alive (its pong comes back at 1.5s) is never evicted.
func TestAttachStale_SlowButAliveWriterIsNotEvicted(t *testing.T) {
	srv, _, _, audit, run := holderTestServer(t)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	defer ts.Close()

	c1 := dialAttach(t, ts, srv, run.ID, holderOwner, "")
	readAttachMode(t, c1)
	waitFor(t, "the writer to be ready", func() bool {
		w := srv.attachHolderFor(run.ID)
		return w != nil && w.ready.isReady()
	})
	alive := srv.attachHolderFor(run.ID)

	go func() {
		time.Sleep(1500 * time.Millisecond) // the pong goes out when this reads
		for {
			if _, _, err := c1.Read(context.Background()); err != nil {
				return
			}
		}
	}()

	c2 := dialAttach(t, ts, srv, run.ID, holderOwner, "")
	if mode := readAttachMode(t, c2); !mode.ReadOnly {
		t.Fatal("the newcomer was admitted writable behind a live writer")
	}

	// Past two probe timeouts: an eviction would have happened by now.
	time.Sleep(attachStaleMisses*attachStaleProbeTimeout + 500*time.Millisecond)
	if w := srv.attachHolderFor(run.ID); w != alive || alive.evicted.Load() {
		t.Error("a slow but live writer was evicted")
	}
	if ev := findAudit(audit.snapshot(), run.ID, "session.takeover", "success"); ev != nil {
		t.Errorf("unexpected session.takeover: %s", ev.Data)
	}
}

// The probe's gates: an attaching writer is never pinged, and neither is another
// person's.
func TestAttachStale_NeverProbesAnAttachingOrForeignWriter(t *testing.T) {
	srv, _, _, run := holderTestServerWithAudit(t, &sshTestRecorder{})
	var pings atomic.Int32
	failPing := func(context.Context) error { pings.Add(1); return context.DeadlineExceeded }

	writer := &attachHolder{principal: holderOwner, actorType: types.ActorHuman, source: attachSourceWeb, ping: failPing, displace: func(string) {}}
	srv.registerAttachHolder(run.ID, writer)
	obs := &attachHolder{principal: holderOwner, actorType: types.ActorHuman, source: attachSourceWeb, displace: func(string) {}}
	srv.registerAttachHolder(run.ID, obs)

	// The writer is still attaching (never marked ready): left alone.
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	srv.probeStaleWriter(ctx, run.ID, obs)
	if n := pings.Load(); n != 0 {
		t.Errorf("an attaching writer was pinged %d times", n)
	}

	// Ready, but the observer is a different person: the probe ends at once.
	writer.ready.setReadyIf(func() bool { return true })
	other := &attachHolder{principal: holderSecond, actorType: types.ActorHuman, source: attachSourceWeb, displace: func(string) {}}
	srv.registerAttachHolder(run.ID, other)
	srv.probeStaleWriter(context.Background(), run.ID, other)
	if n := pings.Load(); n != 0 {
		t.Errorf("a different principal's observer made the probe ping the writer %d times", n)
	}
	if srv.attachHolderFor(run.ID) != writer {
		t.Error("the writer was evicted by a different principal's observer")
	}
}

func TestAttachStale_Verdict(t *testing.T) {
	for _, c := range []struct {
		misses int
		quiet  time.Duration
		want   bool
	}{
		{0, time.Hour, false},
		{1, time.Second, false},
		{1, attachStaleQuiet, true},
		{2, 0, true},
	} {
		if got := staleWriterVerdict(c.misses, c.quiet); got != c.want {
			t.Errorf("staleWriterVerdict(%d, %v) = %v, want %v", c.misses, c.quiet, got, c.want)
		}
	}
}
