// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const legacyReleaseAction = "drive.legacy_docker.release"

// releaseDouble is the legacy Docker release's store and the shared audit table over memory: one value stands for
// the database every replica reads. Its recorder writes a release event into the table unless it is told to fail.
// It also answers the portal store's questions as a deployment with issuer portals on, so a monitor started over it
// is the one a deployment with portals active runs.
type releaseDouble struct {
	*activationStore
	mu       sync.Mutex
	recs     []store.LegacyDockerRelease
	settled  map[uuid.UUID]bool
	table    map[uuid.UUID]time.Time // the audit table: event id -> stored time
	rows     map[uuid.UUID]int       // rows written per id, a replay that duplicated would show 2
	marked   int                     // legacy Docker drives still held
	released int                     // calls of ReleaseLegacyDockerDrives
	down     bool                    // the recorder fails
	monitor  atomic.Int32            // calls of PortalIssuersActivated, one per monitor step
}

func newReleaseDouble() *releaseDouble {
	return &releaseDouble{activationStore: &activationStore{on: true}, settled: map[uuid.UUID]bool{}, table: map[uuid.UUID]time.Time{}, rows: map[uuid.UUID]int{}}
}

func (d *releaseDouble) PortalIssuersActivated(context.Context) (bool, error) {
	d.monitor.Add(1)
	return true, nil
}

// commit is a release another replica committed and then stopped before it sent the audit row.
func (d *releaseDouble) commit(n int) store.LegacyDockerRelease {
	d.mu.Lock()
	defer d.mu.Unlock()
	rec := store.LegacyDockerRelease{ID: uuid.New(), EventAt: time.Now().UTC().Truncate(time.Microsecond), ActorType: string(types.ActorHuman), Actor: "ops@example.com", Released: n}
	d.recs = append(d.recs, rec)
	return rec
}

func (d *releaseDouble) setDown(v bool) {
	d.mu.Lock()
	d.down = v
	d.mu.Unlock()
}

func (d *releaseDouble) auditRows(id uuid.UUID) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rows[id]
}

func (d *releaseDouble) isSettled(id uuid.UUID) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.settled[id]
}

func (d *releaseDouble) ReleaseLegacyDockerDrives(_ context.Context, actorType, actor string) (store.LegacyDockerRelease, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.released++
	if d.marked == 0 {
		return store.LegacyDockerRelease{}, nil
	}
	rec := store.LegacyDockerRelease{ID: uuid.New(), EventAt: time.Now().UTC().Truncate(time.Microsecond), ActorType: actorType, Actor: actor, Released: d.marked}
	d.marked = 0
	d.recs = append(d.recs, rec)
	return rec, nil
}

func (d *releaseDouble) PendingLegacyDockerReleases(context.Context) ([]store.LegacyDockerRelease, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []store.LegacyDockerRelease
	for _, r := range d.recs {
		if !d.settled[r.ID] {
			out = append(out, r)
		}
	}
	return out, nil
}

func (d *releaseDouble) SettleLegacyDockerRelease(_ context.Context, id uuid.UUID) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.settled[id] {
		return false, nil
	}
	if _, in := d.table[id]; !in {
		return false, nil
	}
	d.settled[id] = true
	return true, nil
}

func (d *releaseDouble) AuditEventsStored(_ context.Context, want []store.AuditEventKey) (map[store.AuditEventKey]bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[store.AuditEventKey]bool{}
	for _, k := range want {
		if at, ok := d.table[k.ID]; ok && at.Equal(k.At.UTC().Truncate(time.Microsecond)) {
			out[store.AuditEventKey{ID: k.ID, At: k.At.UTC().Truncate(time.Microsecond)}] = true
		}
	}
	return out, nil
}

// Record is the audit recorder: a row with an id already in the table is a second row, counted.
func (d *releaseDouble) Record(_ context.Context, ev types.AuditEvent) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.down {
		return errors.New("the audit store is down")
	}
	if ev.Action != legacyReleaseAction {
		return nil
	}
	d.rows[ev.ID]++
	d.table[ev.ID] = ev.Time.UTC().Truncate(time.Microsecond)
	return nil
}

func releaseServer(t *testing.T, d *releaseDouble, target string) *Server {
	t.Helper()
	cfg := baseTestConfig(newHarness(t), d)
	cfg.Audit = d
	cfg.RunnerTarget = target
	return New(cfg)
}

func legacyWait(t *testing.T, what string, within time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s", what, within)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The retry's cadence is the design's one a minute, and the loop is its own: a record committed by another replica,
// which then stopped, is delivered while the portal monitor of a deployment with portals on is asleep (it waits an
// hour once portals are on), with no request to wake anything, and the loop ends with its context.
func TestLegacyDockerReleaseRetry_RunsBesideASleepingPortalMonitor(t *testing.T) {
	if legacyDockerReleaseRetryEvery != time.Minute {
		t.Fatalf("legacyDockerReleaseRetryEvery = %s, want one minute", legacyDockerReleaseRetryEvery)
	}
	d := newReleaseDouble()
	srv := releaseServer(t, d, "docker")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	monitorDone := make(chan struct{})
	go func() { srv.RunPortalActivationMonitor(ctx); close(monitorDone) }()
	legacyWait(t, "the monitor's first step", 5*time.Second, func() bool { return d.monitor.Load() >= 1 })
	time.Sleep(50 * time.Millisecond) // the step ended: the monitor is in its hour-long wait

	// Another replica commits a release and stops before sending the audit row. Nothing here wakes the monitor,
	// and it does not deliver (that is not its job any more).
	rec := d.commit(3)
	time.Sleep(200 * time.Millisecond)
	if d.auditRows(rec.ID) != 0 || d.isSettled(rec.ID) {
		t.Fatalf("the sleeping monitor delivered the record: rows %d settled %v", d.auditRows(rec.ID), d.isSettled(rec.ID))
	}
	if n := d.monitor.Load(); n != 1 {
		t.Fatalf("the monitor stepped %d times, want it asleep after one", n)
	}

	loopDone := make(chan struct{})
	go func() { srv.runLegacyDockerReleaseRetry(ctx, 20*time.Millisecond); close(loopDone) }()
	legacyWait(t, "the record to be delivered and settled by the retry loop", 5*time.Second, func() bool { return d.isSettled(rec.ID) })
	if n := d.auditRows(rec.ID); n != 1 {
		t.Errorf("audit rows for the record = %d, want 1", n)
	}

	// A second record committed later is found by the next tick: the loop reads the owed rows each time.
	later := d.commit(1)
	legacyWait(t, "the later record to be settled", 5*time.Second, func() bool { return d.isSettled(later.ID) })

	cancel()
	for name, done := range map[string]chan struct{}{"the retry loop": loopDone, "the portal monitor": monitorDone} {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("%s outlived its context", name)
		}
	}
}

// A record whose row the recorder could not write stays owed and is sent again by a later tick, and the table ends
// with the one row for its id, however many ticks sent it.
func TestLegacyDockerReleaseRetry_SendsAgainUntilTheTableHoldsTheRow(t *testing.T) {
	d := newReleaseDouble()
	srv := releaseServer(t, d, "docker")
	rec := d.commit(2)
	d.setDown(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { srv.runLegacyDockerReleaseRetry(ctx, 10*time.Millisecond); close(done) }()
	time.Sleep(100 * time.Millisecond) // many ticks with the recorder down
	if d.isSettled(rec.ID) || d.auditRows(rec.ID) != 0 {
		t.Fatalf("with the recorder down: settled %v rows %d, want still owed and no row", d.isSettled(rec.ID), d.auditRows(rec.ID))
	}
	d.setDown(false)
	legacyWait(t, "the owed record to be settled once the recorder is back", 5*time.Second, func() bool { return d.isSettled(rec.ID) })
	time.Sleep(60 * time.Millisecond) // further ticks find nothing owed
	if n := d.auditRows(rec.ID); n != 1 {
		t.Errorf("audit rows for the record = %d, want 1", n)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the retry loop outlived its context")
	}
}

// A call of the route delivers and settles the earlier owed record on either runner target, and its zero-release
// answer keeps the replayed id: on the Docker target (nothing marked) and on every other target (nothing to
// release) the answer is {released: 0, replayed: [id]}; the next call answers {released: 0}.
// Values: target "docker", "k8s" and "local" (non-Docker targets; New turns an empty target into "docker"); marked drives 0 and 2; the Docker target
// with 2 marked releases them and answers its own event_id with the earlier id replayed.
func TestReleaseLegacyDockerDrives_AnswersTheReplayedIdsOnEveryTarget(t *testing.T) {
	type answer struct {
		Released int         `json:"released"`
		EventID  *uuid.UUID  `json:"event_id"`
		Replayed []uuid.UUID `json:"replayed"`
	}
	for _, tc := range []struct {
		target       string
		marked       int
		wantReleased int
		releaseCalls int // per call: the store's release statement is run only on the Docker target
	}{
		{"docker", 0, 0, 1},
		{"k8s", 2, 0, 0},
		{"local", 2, 0, 0},
		{"docker", 2, 2, 1},
	} {
		d := newReleaseDouble()
		d.marked = tc.marked
		srv := releaseServer(t, d, tc.target)
		owed := d.commit(4)
		call := func() answer {
			w := do(t, srv, http.MethodPost, "/api/v1/admin/drives/legacy-docker/release", adminToken, "")
			if w.Code != http.StatusOK {
				t.Fatalf("target %q marked %d: release = %d %s", tc.target, tc.marked, w.Code, w.Body)
			}
			var a answer
			if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
				t.Fatal(err)
			}
			return a
		}
		first := call()
		if first.Released != tc.wantReleased || len(first.Replayed) != 1 || first.Replayed[0] != owed.ID || (tc.wantReleased > 0) != (first.EventID != nil) {
			t.Errorf("target %q marked %d: first call = %+v, want released %d, replayed [%s], event_id only for a release", tc.target, tc.marked, first, tc.wantReleased, owed.ID)
		}
		if !d.isSettled(owed.ID) || d.auditRows(owed.ID) != 1 {
			t.Errorf("target %q: the earlier record settled %v with %d audit rows, want settled with 1", tc.target, d.isSettled(owed.ID), d.auditRows(owed.ID))
		}
		if first.EventID != nil && (!d.isSettled(*first.EventID) || d.auditRows(*first.EventID) != 1) {
			t.Errorf("target %q: this release's own record settled %v with %d rows, want settled with 1", tc.target, d.isSettled(*first.EventID), d.auditRows(*first.EventID))
		}
		if second := call(); second.Released != 0 || len(second.Replayed) != 0 || second.EventID != nil {
			t.Errorf("target %q marked %d: second call = %+v, want {released: 0} alone", tc.target, tc.marked, second)
		}
		if d.released != tc.releaseCalls*2 {
			t.Errorf("target %q: the release statement ran %d times, want %d (two calls)", tc.target, d.released, tc.releaseCalls*2)
		}
	}
}
