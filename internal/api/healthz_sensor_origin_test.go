// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/groundtruth"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sensorAskStore records what the eBPF health read asks the store for, the
// time left on its deadline and whether its context had ended, and answers
// that no beat exists.
type sensorAskStore struct {
	store.Store
	action, actor string
	actorType     types.ActorType
	left          time.Duration
	hasDeadline   bool
	ctxErr        error
}

func (s *sensorAskStore) LatestOrganisationAuditEvent(ctx context.Context, action string, actorType types.ActorType, actor string) (types.AuditEvent, error) {
	s.action, s.actorType, s.actor = action, actorType, actor
	var deadline time.Time
	deadline, s.hasDeadline = ctx.Deadline()
	s.left = time.Until(deadline)
	s.ctxErr = ctx.Err()
	return types.AuditEvent{}, store.ErrNotFound
}

// The eBPF health read asks for the sensor's own beat: the heartbeat action, as
// the system sensor identity handleGroundtruthEvents stamps on every row it takes.
func TestEbpfGroundtruthStatusAsksForTheSensorsOwnBeat(t *testing.T) {
	h := newHarness(t)
	st := &sensorAskStore{}
	h.srv.cfg.Store = st
	if got := h.srv.ebpfGroundtruthStatus(context.Background()); got["state"] != "unavailable" || got["reason"] != nil {
		t.Fatalf("state = %v, reason = %v; want unavailable with no reason, as no beat exists", got["state"], got["reason"])
	}
	if st.action != groundtruth.ActionSensorHeartbeat || st.actorType != types.ActorSystem || st.actor != groundtruth.SensorActor {
		t.Errorf("asked for %q written by %s %q, want %q written by system %q",
			st.action, st.actorType, st.actor, groundtruth.ActionSensorHeartbeat, groundtruth.SensorActor)
	}
	if !st.hasDeadline || st.left <= 0 || st.left > sensorBeatReadTimeout {
		t.Errorf("the read had %v left on its deadline (deadline set: %v), want at most sensorBeatReadTimeout (%v)",
			st.left, st.hasDeadline, sensorBeatReadTimeout)
	}
	if got := h.srv.hostSensorCaveat(context.Background()); got != hostSensorUnavailableCaveat {
		t.Errorf("caveat = %q, want the none-on-record note", got)
	}
}

// Every caller on the replica shares the read's answer, so a caller that has gone away (a closed
// console tab) must not end it: only the read's own bound does.
func TestSensorBeatReadIsDetachedFromTheCaller(t *testing.T) {
	h := newHarness(t)
	st := &sensorAskStore{}
	h.srv.cfg.Store = st
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.srv.ebpfGroundtruthStatus(ctx)
	if st.action == "" || st.ctxErr != nil {
		t.Fatalf("read made: %v, its context ended with %v; want the read made under a context the caller cannot end", st.action != "", st.ctxErr)
	}
}

// The eBPF health read is bounded: forwarded beats are passed over row by row, and /healthz is the
// liveness probe, so a stalled read must give way at sensorBeatReadTimeout and read unavailable, saying
// the beat could not be read rather than that no beat is on record.
func TestEbpfGroundtruthStatusIsBoundedByAStalledStore(t *testing.T) {
	prev := sensorBeatReadTimeout
	sensorBeatReadTimeout = 100 * time.Millisecond
	t.Cleanup(func() { sensorBeatReadTimeout = prev })
	released := make(chan struct{})
	defer close(released)
	h := newHarness(t)
	h.srv.cfg.Store = blockingHeartbeatStore{released: released}

	status := make(chan map[string]any, 1)
	go func() { status <- h.srv.ebpfGroundtruthStatus(context.Background()) }()
	select {
	case got := <-status:
		if got["state"] != "unavailable" || got["reason"] != groundtruthBeatReadFailed {
			t.Errorf("state = %v, reason = %v against a stalled store, want unavailable, %q", got["state"], got["reason"], groundtruthBeatReadFailed)
		}
	case <-time.After(sensorBeatReadTimeout + 10*time.Second):
		t.Fatalf("the eBPF health read did not return within %s + slack against a stalled store", sensorBeatReadTimeout)
	}
	if got := h.srv.hostSensorCaveat(context.Background()); got != hostSensorUnreadableCaveat {
		t.Errorf("caveat = %q against a stalled store, want the could-not-be-read note", got)
	}
}

// stalledBeatRowStore stalls the beat read like blockingHeartbeatStore, and
// holds the sensor's own node rows.
type stalledBeatRowStore struct {
	blockingHeartbeatStore
	rows []store.GroundtruthSensor
}

func (stalledBeatRowStore) UpsertGroundtruthSensor(context.Context, store.GroundtruthSensor) error {
	return nil
}

func (s stalledBeatRowStore) ListGroundtruthSensors(context.Context, time.Time) ([]store.GroundtruthSensor, error) {
	return s.rows, nil
}

// The sensor's own node row decides first, and the audit read is made only when there is none: the
// row holds when the counts last rose, a laptop never writes it, and reading it scans no forwarded
// beats. Against an audit read that stalls for a minute, the row still answers at once.
func TestEbpfGroundtruthStatusReadsTheSensorsOwnRowFirst(t *testing.T) {
	prev := sensorBeatReadTimeout
	sensorBeatReadTimeout = time.Minute
	t.Cleanup(func() { sensorBeatReadTimeout = prev })
	released := make(chan struct{})
	defer close(released)
	h := newHarness(t)
	now := h.srv.cfg.Now()
	h.srv.cfg.Store = stalledBeatRowStore{
		blockingHeartbeatStore: blockingHeartbeatStore{released: released},
		rows:                   []store.GroundtruthSensor{{LastBeat: now, ObservedTotal: 7, EvidenceAt: &now}},
	}
	status := make(chan map[string]any, 1)
	go func() { status <- h.srv.ebpfGroundtruthStatus(context.Background()) }()
	select {
	case got := <-status:
		if got["state"] != "healthy" || got["reason"] != nil || got["observed_total"] != uint64(7) {
			t.Errorf("state = %v, reason = %v, observed_total = %v from a fresh node row whose counts just rose; want healthy, no reason, 7",
				got["state"], got["reason"], got["observed_total"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the health read waited on the audit read although the sensor's node row was there")
	}
}

// The bound stays inside both Helm probes that call /healthz: startup (the Kubernetes default, 1s) and
// liveness (3s). A read that ran to a probe's own timeout would fail it and restart the daemon.
func TestSensorBeatReadTimeoutStaysInsideTheProbes(t *testing.T) {
	if sensorBeatReadTimeout != 500*time.Millisecond {
		t.Fatalf("sensorBeatReadTimeout = %v, want 500ms, inside the startup probe's 1s", sensorBeatReadTimeout)
	}
}

// TestPG_HealthzSensorIgnoresADevicesForgedBeat: an enrolled laptop forwards a
// fresh kernel.sensor.ping claiming the sensor's identity and observed events.
// The ingest takes it, as it takes any action, but /healthz must not read it as
// this organisation's sensor: with no real beat the state is unavailable, with a
// stale one degraded, with a fresh one but no node row idle with no rise on
// record, and only the sensor's own node row whose counts rose reads healthy.
func TestPG_HealthzSensorIgnoresADevicesForgedBeat(t *testing.T) {
	pool := throwawayPGPool(t)
	ctx := context.Background()
	h := newHarness(t)
	cfg := baseTestConfig(h, store.NewPG(pool))
	clock := &testClock{now: time.Now().UTC()}
	cfg.Now = clock.Now
	h.srv = New(cfg)
	id, tok := enrolTestDevice(t, h.srv, "laptop")

	forged := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC().Truncate(time.Microsecond), ActorType: types.ActorSystem,
		Actor: groundtruth.SensorActor, Action: groundtruth.ActionSensorHeartbeat, Outcome: "success",
		Data: json.RawMessage(`{"observed_total":42,"dropped_total":0}`)}
	if err := pool.QueryRow(ctx, `SELECT audit_row_hash($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb)`,
		"", forged.ID, forged.Time, forged.RunID, string(forged.ActorType), forged.Actor,
		forged.Action, forged.Target, forged.Outcome, forged.SourceIP, []byte(forged.Data)).Scan(&forged.RowHash); err != nil {
		t.Fatal(err)
	}
	w := do(t, h.srv, http.MethodPost, "/api/v1/devices/"+id.String()+"/audit", tok,
		string(mustJSON([]types.FederatedAuditEvent{{AuditEvent: forged, Seq: 1}})))
	if w.Code != http.StatusOK || ackedSeq(t, w) != 1 {
		t.Fatalf("a laptop forwarding a sensor beat: %d %s, want 200 acked at 1", w.Code, w.Body.String())
	}

	if gt := healthzEbpf(t, h); gt["state"] != "unavailable" {
		t.Fatalf("with only the forwarded beat: state %v, want unavailable (%v)", gt["state"], gt)
	}
	beat := func(at time.Time) {
		t.Helper()
		ev := groundtruth.HeartbeatEventWithDropped(0, 7, 0, nil)
		ev.ID, ev.Time = uuid.New(), at
		if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
			t.Fatal(err)
		}
	}
	beat(time.Now().UTC().Add(-ebpfHeartbeatTTL - time.Minute))
	clock.advance(sensorBeatCacheFor) // past the kept answer
	if gt := healthzEbpf(t, h); gt["state"] != "degraded" {
		t.Fatalf("with the sensor's own beat stale: state %v, want degraded (%v)", gt["state"], gt)
	}
	beat(time.Now().UTC())
	clock.advance(sensorBeatCacheFor)
	if gt := healthzEbpf(t, h); gt["state"] != "idle" || gt["reason"] != groundtruthNotYetKnown {
		t.Fatalf("with the sensor's own beat fresh and no node row: %v, want idle with %q", gt, groundtruthNotYetKnown)
	}
	pg := store.NewPG(pool)
	for i, at := range []time.Time{clock.Now().Add(-30 * time.Second), clock.Now()} {
		if err := pg.UpsertGroundtruthSensor(ctx, store.GroundtruthSensor{LastBeat: at, ObservedTotal: 7 + uint64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if gt := healthzEbpf(t, h); gt["state"] != "healthy" {
		t.Fatalf("with the sensor's own node row, its counts risen: state %v, want healthy (%v)", gt["state"], gt)
	}
}
