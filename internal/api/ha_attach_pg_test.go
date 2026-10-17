// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The attach lease and the run events across two Servers over one database (ha-l2.4). Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// leaseHolder is a web client for the registry, remembering why it was displaced.
type leaseHolderProbe struct {
	h *attachHolder

	mu        sync.Mutex
	displaced string
	notified  []bool
}

func newLeaseHolder(principal string) *leaseHolderProbe {
	p := &leaseHolderProbe{}
	p.h = &attachHolder{
		principal: principal, actorType: types.ActorHuman, since: time.Now().UTC(), source: attachSourceWeb,
		displace: func(reason string) {
			p.mu.Lock()
			p.displaced = reason
			p.mu.Unlock()
		},
		notify: func(readOnly bool, _ *attachHolder) {
			p.mu.Lock()
			p.notified = append(p.notified, readOnly)
			p.mu.Unlock()
		},
	}
	return p
}

func (p *leaseHolderProbe) displacedWith() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.displaced
}

func (p *leaseHolderProbe) promoted() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.notified) > 0 && !p.notified[len(p.notified)-1]
}

func (l *maskLab) takeover(rp replica, run uuid.UUID) (int, map[string]any) {
	l.t.Helper()
	w := do(l.t, rp.srv, http.MethodPost, "/api/v1/runs/"+run.String()+"/attach/takeover", adminToken, "")
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

func (l *maskLab) holderView(rp replica, run uuid.UUID) attachHolderView {
	l.t.Helper()
	w := do(l.t, rp.srv, http.MethodGet, "/api/v1/runs/"+run.String()+"/attach/holder", adminToken, "")
	var v attachHolderView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || w.Code != http.StatusOK {
		l.t.Fatalf("holder view = %d %s", w.Code, w.Body)
	}
	return v
}

// An attach whose lease was taken over by a take-over served on another replica cannot send
// input or resize, and its client is displaced; the slot is kept for the taker, so the bystander
// queued on the other replica is not promoted into it.
func TestHALivePG_ATakenOverAttachCannotSendInputOrResize(t *testing.T) {
	l := newMaskLab(t)
	a, b := l.liveReplica("replica-a"), l.liveReplica("replica-b")
	run := l.run()

	writer := newLeaseHolder("alice")
	readOnly, release := a.srv.registerAttachHolder(run.ID, writer.h)
	if readOnly || !writer.h.canWrite() {
		t.Fatalf("the first client on A: readOnly=%v canWrite=%v, want the writer", readOnly, writer.h.canWrite())
	}
	defer releaseAttach(release)
	sess := newCountingShellSession()
	if err := writer.h.writeGated(sess, []byte("typed")); err != nil || sess.writtenBytes() != 5 {
		t.Fatalf("the writer's input: err=%v bytes=%d, want 5", err, sess.writtenBytes())
	}

	// B reports who holds the terminal, and queues its own client behind the lease.
	if v := l.holderView(b, run.ID); !v.Held || v.Principal != "alice" || v.Source != attachSourceWeb {
		t.Fatalf("holder view on B = %+v, want alice over web, held through A's lease", v)
	}
	bystander := newLeaseHolder("carol")
	if ro, relB := b.srv.registerAttachHolder(run.ID, bystander.h); !ro || bystander.h.canWrite() {
		t.Fatalf("a client on B behind A's lease: readOnly=%v canWrite=%v, want an observer", ro, bystander.h.canWrite())
	} else {
		defer releaseAttach(relB)
	}

	if code, body := l.takeover(b, run.ID); code != http.StatusOK || body["previous_holder"] != "alice" {
		t.Fatalf("take-over on B = %d %v, want 200 naming alice", code, body)
	}
	haWait(t, 3*time.Second, "A's holder to lose its write authority", func() bool { return !writer.h.canWrite() })
	haWait(t, 3*time.Second, "A's client to be displaced", func() bool { return writer.displacedWith() != "" })
	if got := writer.displacedWith(); !strings.HasPrefix(got, attachTakeoverReasonPrefix) {
		t.Errorf("the displaced client was told %q, want a take-over reason", got)
	}
	if err := writer.h.writeGated(sess, []byte("more")); err != nil || sess.writtenBytes() != 5 {
		t.Fatalf("a taken-over holder's input reached the terminal: err=%v bytes=%d, want still 5", err, sess.writtenBytes())
	}
	if writer.h.canWrite() {
		t.Error("a taken-over holder may still resize")
	}
	var takerName string
	for _, ev := range l.rec.snapshot() {
		if ev.Action == "session.takeover" && strings.Contains(string(ev.Data), `"previous_holder":"alice"`) {
			takerName = ev.Actor
		}
	}
	if takerName == "" {
		t.Fatal("no session.takeover row names the displaced holder")
	}

	// The slot is the taker's: the keeper does not hand it to the bystander, and the taker's own
	// client, attaching on B, gets it.
	time.Sleep(2*attachLeaseRenewEvery + 200*time.Millisecond)
	if bystander.h.canWrite() || bystander.promoted() {
		t.Fatal("the slot a take-over freed went to a bystander")
	}
	taker := newLeaseHolder(takerName)
	ro, relC := b.srv.registerAttachHolder(run.ID, taker.h)
	defer releaseAttach(relC)
	if ro || !taker.h.canWrite() {
		t.Fatalf("the taker's client: readOnly=%v canWrite=%v, want the writer", ro, taker.h.canWrite())
	}
}

// With the notice lost (the servers have no bus), the fence alone ends a taken-over holder: it
// proves its lease against Postgres at least once a second, and fails closed.
func TestHALivePG_ATakenOverAttachIsFencedWithNoNotice(t *testing.T) {
	l := newMaskLab(t)
	a, b := l.replica(), l.replica()
	run := l.run()
	writer := newLeaseHolder("alice")
	if ro, release := a.srv.registerAttachHolder(run.ID, writer.h); ro {
		t.Fatal("the first client was not the writer")
	} else {
		defer releaseAttach(release)
	}
	if code, body := l.takeover(b, run.ID); code != http.StatusOK {
		t.Fatalf("take-over on B = %d %v", code, body)
	}
	haWait(t, 4*time.Second, "the fence to end A's holder with no notice", func() bool { return !writer.h.canWrite() })
	haWait(t, 4*time.Second, "A's client to be displaced", func() bool { return writer.displacedWith() != "" })
}

// A holder that cannot prove its lease because Postgres does not answer stops writing, and a
// lease whose replica died lapses to the next client.
func TestHALivePG_ALapsedLeaseGoesToTheWaitingObserverOnAnotherReplica(t *testing.T) {
	l := newMaskLab(t)
	a, b := l.liveReplica("replica-a"), l.liveReplica("replica-b")
	run := l.run()
	writer := newLeaseHolder("alice")
	_, releaseA := a.srv.registerAttachHolder(run.ID, writer.h)
	waiting := newLeaseHolder("alice")
	if ro, relB := b.srv.registerAttachHolder(run.ID, waiting.h); !ro {
		t.Fatal("B's client was not an observer behind A's lease")
	} else {
		defer releaseAttach(relB)
	}

	// A's writer leaves: the lease is released, B is told, and its observer is promoted in place.
	releaseAttach(releaseA)
	haWait(t, 5*time.Second, "B's observer to be promoted", func() bool { return waiting.h.canWrite() && waiting.promoted() })
	if l.count(`SELECT count(*) FROM run_attach_leases WHERE run_id=$1 AND holder_id=$2`, run.ID, waiting.h.id) != 1 {
		t.Fatal("the lease does not name the promoted client")
	}
	var promotedAudit bool
	for _, ev := range l.rec.snapshot() {
		if ev.Action == "session.promote" && ev.RunID != nil && *ev.RunID == run.ID {
			promotedAudit = true
		}
	}
	if !promotedAudit {
		t.Error("no session.promote row for the cross-replica promotion")
	}
}

// A lost event notice still ends a stream on B on the next beat: B's ring never saw the events A
// emitted, and the keepalive re-reads the store.
func TestHALivePG_ALostEventNoticeStillDeliversEndedOnTheNextBeat(t *testing.T) {
	l := newMaskLab(t)
	b := l.replica() // no bus: every notice from A is lost
	b.srv.runEvents.beat = 100 * time.Millisecond
	run := l.run()

	ts := httptest.NewServer(panicFails(t, b.srv.Handler()))
	defer ts.Close()
	stream := openRunEvents(t, t.Context(), ts.URL, run.ID, "")
	// The run ends on A (its CAS and emit), which B never hears of.
	if _, err := store.NewPG(l.pool).UpdateRunStateIf(context.Background(), run.ID, types.RunRunning, types.RunKilled); err != nil {
		t.Fatal(err)
	}
	ev, ok := stream.next()
	if !ok || ev.Type != client.RunEventEnded || ev.State != types.RunKilled {
		t.Fatalf("first event = %+v ok=%v, want ended KILLED on a following beat", ev, ok)
	}
}

// A notice from A puts its events in B's ring, so a stream on B follows a run A dispatches.
func TestHALivePG_ARunEventEmittedOnAReachesAStreamOnB(t *testing.T) {
	l := newMaskLab(t)
	a, b := l.liveReplica("replica-a"), l.liveReplica("replica-b")
	run := l.run()
	haWait(t, 10*time.Second, "B to hear A's event", func() bool {
		a.srv.runEvents.emit(run.ID, client.RunEvent{Type: client.RunEventProvisioning})
		evs, _, _ := b.srv.runEvents.since(run.ID, 0)
		return len(evs) > 0
	})
	evs, _, _ := b.srv.runEvents.since(run.ID, 0)
	if evs[0].Type != client.RunEventProvisioning {
		t.Fatalf("B's ring = %+v, want A's provisioning event", evs)
	}
}
