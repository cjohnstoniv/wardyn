// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/livebus"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// miscCovBusServer is a server whose live bus publishes through a pool that has been closed, so every
// notice it sends fails the same way, with no database.
func miscCovBusServer(t *testing.T, origin string) (*Server, error) {
	t.Helper()
	pool, closedErr := miscCovClosedPool(t)
	cfg := baseTestConfig(newHarness(t), nil)
	cfg.LiveBus = livebus.New(pool, origin)
	return New(cfg), closedErr
}

func TestMiscCovReplicaName(t *testing.T) {
	srv, _ := miscCovBusServer(t, "replica-a")
	if got := srv.replicaName(); got != "replica-a" {
		t.Errorf("replicaName with a bus = %q, want the bus origin", got)
	}

	plain := newHarness(t).srv
	first := plain.replicaName()
	if _, err := uuid.Parse(first); err != nil {
		t.Fatalf("replicaName with no bus = %q, want a generated id: %v", first, err)
	}
	if second := plain.replicaName(); second != first {
		t.Errorf("replicaName changed between calls: %q then %q", first, second)
	}
}

func TestMiscCovRegisterLiveBusWiresThePublisherOnlyWithABus(t *testing.T) {
	if newHarness(t).srv.runEvents.publish != nil {
		t.Error("a server with no bus publishes run events")
	}
	srv, _ := miscCovBusServer(t, "replica-a")
	if srv.runEvents.publish == nil {
		t.Error("a server with a bus does not publish the run events it emits")
	}
}

// An event this replica emits is announced to the others; a notice that cannot be sent costs only
// the hint, is logged with its kind and run, and the event is still in this replica's own ring.
func TestMiscCovEmittedRunEventsAreAnnouncedAndALostNoticeIsLogged(t *testing.T) {
	srv, closedErr := miscCovBusServer(t, "replica-a")
	logs := miscCovCaptureLogs(t)
	run := uuid.MustParse("00000000-0000-4000-8000-0000000000a1")
	ev := client.RunEvent{ID: 1, Type: client.RunEventReady}

	srv.runEvents.emit(run, ev)
	srv.WaitBackground()

	rec, ok := logs.find("a notice to the other replicas was not sent")
	if !ok {
		t.Fatal("the failed notice was not logged")
	}
	if v, _ := logs.attr(rec, "kind"); v.String() != livebus.KindRunEvent {
		t.Errorf("kind = %v, want %s", v, livebus.KindRunEvent)
	}
	if v, _ := logs.attr(rec, "run_id"); v.String() != run.String() {
		t.Errorf("run_id = %v, want %s", v, run)
	}
	if v, _ := logs.attr(rec, "err"); !errors.Is(v.Any().(error), closedErr) {
		t.Errorf("err = %v, want the publish error to wrap the pool's", v)
	}
	if got, _, _ := srv.runEvents.since(run, 0); len(got) != 1 || got[0].Type != client.RunEventReady {
		t.Errorf("the emitting replica's ring holds %+v, want its own event", got)
	}
}

func TestMiscCovPublishNoticeWithNoBusDoesNothing(t *testing.T) {
	srv := newHarness(t).srv
	logs := miscCovCaptureLogs(t)
	srv.publishNotice(livebus.KindRunKill, uuid.New(), nil)
	srv.WaitBackground()
	if _, ok := logs.find("a notice to the other replicas was not sent"); ok {
		t.Error("a server with no bus tried to publish a notice")
	}
}

// A kill cancels the run's in-flight create here and announces it to the replica that may hold it.
func TestMiscCovCancelCreateStopsTheLocalCreateAndAnnouncesTheKill(t *testing.T) {
	srv, _ := miscCovBusServer(t, "replica-a")
	logs := miscCovCaptureLogs(t)
	run := uuid.MustParse("00000000-0000-4000-8000-0000000000a2")
	ctx, done := srv.creates.track(t.Context(), run)
	defer done()

	srv.cancelCreate(run)
	srv.WaitBackground()

	if ctx.Err() == nil {
		t.Error("the run's in-flight create was not cancelled")
	}
	rec, ok := logs.find("a notice to the other replicas was not sent")
	if !ok {
		t.Fatal("the kill was not announced (no publish attempted)")
	}
	if v, _ := logs.attr(rec, "kind"); v.String() != livebus.KindRunKill {
		t.Errorf("kind = %v, want %s", v, livebus.KindRunKill)
	}
}

func TestMiscCovOnRunEventNotice(t *testing.T) {
	srv, _ := miscCovBusServer(t, "replica-a")
	run := uuid.MustParse("00000000-0000-4000-8000-0000000000a3")
	evs := []client.RunEvent{{ID: 1, Type: client.RunEventProvisioning}, {ID: 2, Type: client.RunEventReady}}
	raw, err := json.Marshal(evs)
	if err != nil {
		t.Fatal(err)
	}
	types := func() []string {
		got, _, _ := srv.runEvents.since(run, 0)
		var out []string
		for _, e := range got {
			out = append(out, e.Type)
		}
		return out
	}

	// Its own notices come back over the bus and must not be appended twice.
	srv.onRunEventNotice(livebus.Message{Kind: livebus.KindRunEvent, Run: run, Origin: "replica-a", Data: raw})
	if got := types(); len(got) != 0 {
		t.Fatalf("a notice from this replica was appended: %v", got)
	}
	// Garbage and empty lists from another replica are dropped, not appended.
	srv.onRunEventNotice(livebus.Message{Kind: livebus.KindRunEvent, Run: run, Origin: "replica-b", Data: json.RawMessage(`{not json`)})
	srv.onRunEventNotice(livebus.Message{Kind: livebus.KindRunEvent, Run: run, Origin: "replica-b", Data: json.RawMessage(`[]`)})
	if got := types(); len(got) != 0 {
		t.Fatalf("an undecodable or empty notice was appended: %v", got)
	}
	srv.onRunEventNotice(livebus.Message{Kind: livebus.KindRunEvent, Run: run, Origin: "replica-b", Data: raw})
	if got := types(); !slices.Equal(got, []string{client.RunEventProvisioning, client.RunEventReady}) {
		t.Fatalf("ring = %v, want the other replica's events in order", got)
	}
}

func TestMiscCovUnmarshalNotice(t *testing.T) {
	var got struct{ N int }
	if err := unmarshalNotice(livebus.Message{Data: json.RawMessage(`{"N":7}`)}, &got); err != nil || got.N != 7 {
		t.Fatalf("decoded %+v, %v; want N=7", got, err)
	}
	if err := unmarshalNotice(livebus.Message{Data: json.RawMessage(`[`)}, &got); err == nil {
		t.Error("a truncated notice decoded")
	}
}
