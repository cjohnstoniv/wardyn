// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The queue's surface against a real Postgres: slot counts on the pool list and the dry doors, and a
// waiting run's place. Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// bothTypesPool is a capped pool that takes Background tasks and Interactive environments.
func (e *queueEnv) bothTypesPool(name string, max int) types.RunnerPool {
	e.t.Helper()
	p := e.cappedPool(name, max)
	limits := *p.Limits
	limits.Interactive = &types.RunnerPoolInteractiveLimits{CPUMillis: limits.Background.CPUMillis, MemoryMiB: limits.Background.MemoryMiB,
		Lifetime: limits.Background.Lifetime, Idle: &types.RunnerPoolDuration{DefaultSec: 1800, MaxSec: 3600}}
	out, err := e.pg.UpdateRunnerPool(context.Background(), p.ID, e.revision(p.ID), nil, nil, &limits)
	if err != nil {
		e.t.Fatalf("allow interactive: %v", err)
	}
	return out
}

// The list counts what holds a slot as used and what waits as waiting; a waiting run is not a slot.
// ?run_type= adds the wait that type gets.
func TestPoolListSlotsCountSlotHoldersAndWaiters(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.bothTypesPool("Capped", 2)
	uncapped := e.limitedPool("Open", types.CC1, types.CC2, types.CC3)

	list, _ := e.list(nil, "")
	got := entryOf(t, list, pool.ID).Slots
	if got == nil || got.Used != 0 || got.Waiting != 0 || got.Max == nil || *got.Max != 2 || got.Verdict != client.SlotsStartsNow || got.MaxWaitSec != 0 {
		t.Fatalf("an idle pool answers %+v", got)
	}
	if open := entryOf(t, list, uncapped.ID).Slots; open == nil || open.Max != nil || open.Verdict != client.SlotsStartsNow {
		t.Fatalf("a pool with no cap answers %+v", open)
	}

	a, b := e.occupy(pool), e.occupy(pool)
	list, _ = e.list(nil, "")
	if got := entryOf(t, list, pool.ID).Slots; got.Used != 2 || got.Waiting != 0 || got.Verdict != client.SlotsWaits {
		t.Fatalf("a pool at its limit with nobody waiting answers %+v", got)
	}

	e.queuedLaunch(nil, pool, "")
	list, _ = e.list(nil, "?run_type=interactive")
	got = entryOf(t, list, pool.ID).Slots
	if got.Used != 2 || got.Waiting != 1 || got.Verdict != client.SlotsWaits || got.MaxWaitSec != 3600 {
		t.Fatalf("a waiting run is counted as waiting and not as used: %+v", got)
	}
	list, _ = e.list(nil, "?run_type=background")
	if got := entryOf(t, list, pool.ID).Slots; got.MaxWaitSec != 86400 {
		t.Fatalf("background max_wait_sec = %d, want 86400", got.MaxWaitSec)
	}

	e.free(a)
	e.free(b)
}

// Both dry doors answer the resolved pool's slots, with the wait this run would get, and an unpooled
// run none.
func TestDryDoorsCarryRunnerPoolSlots(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.bothTypesPool("Capped", 1)
	e.occupy(pool)
	pin := `"runner_pool_id":"` + pool.ID.String() + `"`

	for _, tc := range []struct {
		experience string
		wantWait   int
	}{{"background", 86400}, {"interactive", 3600}} {
		body := endBody(tc.experience, pin)
		pf := decodeJSON[preflightResponse](t, e.admin(http.MethodPost, "/api/v1/runs/preflight", body))
		pv := e.admin(http.MethodPost, policyPreviewPath, body)
		if pv.Code != http.StatusOK {
			t.Fatalf("preview = %d %s", pv.Code, pv.Body)
		}
		preview := decodeJSON[policyPreviewResponse](t, pv)
		for door, s := range map[string]*client.PoolSlots{"preflight": pf.RunnerPoolSlots, "preview": preview.RunnerPoolSlots} {
			if s == nil || s.Used != 1 || s.Max == nil || *s.Max != 1 || s.Verdict != client.SlotsWaits || s.MaxWaitSec != tc.wantWait {
				t.Fatalf("%s %s runner_pool_slots = %+v, want used 1 of 1, waits, max_wait_sec %d", door, tc.experience, s, tc.wantWait)
			}
		}
	}

	asked := decodeJSON[preflightResponse](t, e.admin(http.MethodPost, "/api/v1/runs/preflight", endBody("background", pin+`,"max_wait_sec":600`)))
	if asked.RunnerPoolSlots == nil || asked.RunnerPoolSlots.MaxWaitSec != 600 {
		t.Fatalf("a request's own max_wait_sec is the one shown: %+v", asked.RunnerPoolSlots)
	}
}

// A waiting run says where it stands, on the 201, the single read and the list; it has no queue once it
// starts, and the place is a count of runs ahead.
func TestQueuedRunCarriesItsPlace(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Capped", 1)
	occupant := e.occupy(pool)

	first := e.queuedLaunch(nil, pool, "")
	second := e.queuedLaunch(nil, pool, "")
	for i, run := range []queuedLaunchAnswer{first, second} {
		q := run.Queue
		if q == nil || q.PoolID != pool.ID || q.Position != i || q.Reason != "runner_pool_at_capacity" {
			t.Fatalf("run %d answered queue %+v, want pool %s position %d runner_pool_at_capacity", i, q, pool.ID, i)
		}
		if got := q.DeadlineAt.Sub(q.QueuedAt); got != 24*time.Hour {
			t.Fatalf("run %d waits %s, want the Background default of 24h", i, got)
		}
	}

	w := e.admin(http.MethodGet, "/api/v1/runs/"+second.ID.String(), "")
	if got := decodeJSON[types.AgentRun](t, w); got.Queue == nil || got.Queue.Position != 1 || got.AdmittedAt != nil {
		t.Fatalf("GET /runs/{id} queue %+v admitted_at %v, want position 1 and no admission", got.Queue, got.AdmittedAt)
	}
	var listed []types.AgentRun
	if err := json.Unmarshal(e.admin(http.MethodGet, "/api/v1/runs", "").Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	positions := map[string]int{}
	for _, r := range listed {
		if r.Queue != nil {
			positions[r.ID.String()] = r.Queue.Position
		}
	}
	if positions[first.ID.String()] != 0 || positions[second.ID.String()] != 1 || len(positions) != 2 {
		t.Fatalf("GET /runs queue positions = %v", positions)
	}

	e.free(occupant)
	started := e.waitStarted(first.ID)
	if started.Queue != nil || started.AdmittedAt == nil {
		t.Fatalf("a promoted run has queue %+v admitted_at %v", started.Queue, started.AdmittedAt)
	}
	w = e.admin(http.MethodGet, "/api/v1/runs/"+first.ID.String(), "")
	if got := decodeJSON[types.AgentRun](t, w); got.Queue != nil || got.AdmittedAt == nil {
		t.Fatalf("GET after promotion: queue %+v admitted_at %v", got.Queue, got.AdmittedAt)
	}
}

// Privacy: slot answers on every surface carry counts only. A person who owns none of a pool's runs
// reads it busy, and nothing in the answer names a run, an owner or an executor.
func TestSlotAnswersNameNoRunOwnerOrExecutor(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Capped", 1)
	occupant := e.occupy(pool)
	waiter := e.queuedLaunch(nil, pool, "")

	body := endBody("background", `"runner_pool_id":"`+pool.ID.String()+`"`)
	w := e.call(e.person, http.MethodPost, "/api/v1/runs/preflight", body)
	if w.Code != http.StatusOK {
		t.Fatalf("preflight = %d %s", w.Code, w.Body)
	}
	_, listBody := e.list(e.person, "?run_type=background")
	for door, raw := range map[string][]byte{
		"preflight": rawField(t, w.Body.Bytes(), "runner_pool_slots"),
		"list":      slotsOfPool(t, []byte(listBody), pool.ID.String()),
	} {
		var s client.PoolSlots
		if err := json.Unmarshal(raw, &s); err != nil || s.Used != 1 || s.Waiting != 1 || s.Verdict != client.SlotsWaits {
			t.Fatalf("%s slots = %s (%v), want one running and one waiting", door, raw, err)
		}
		for _, secret := range []string{occupant.ID.String(), waiter.ID.String(), occupant.CreatedBy, waiter.CreatedBy, "alpha"} {
			if secret != "" && strings.Contains(string(raw), secret) {
				t.Fatalf("%s slots %s name %q", door, raw, secret)
			}
		}
	}
}

func rawField(t *testing.T, doc []byte, key string) json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(doc, &m); err != nil || m[key] == nil {
		t.Fatalf("no %q in %s (%v)", key, doc, err)
	}
	return m[key]
}

func slotsOfPool(t *testing.T, listBody []byte, poolID string) json.RawMessage {
	t.Helper()
	var l struct {
		Pools []map[string]json.RawMessage `json:"pools"`
	}
	if err := json.Unmarshal(listBody, &l); err != nil {
		t.Fatal(err)
	}
	for _, p := range l.Pools {
		if string(p["id"]) == `"`+poolID+`"` {
			return p["slots"]
		}
	}
	t.Fatalf("pool %s not listed", poolID)
	return nil
}

// failingSlotCounts is a store whose slot count fails.
type failingSlotCounts struct{ store.PG }

func (failingSlotCounts) RunnerPoolSlotCounts(context.Context, []uuid.UUID) (map[uuid.UUID]store.PoolCounts, error) {
	return nil, errors.New("slot count unavailable")
}

// The counts are advisory: when they cannot be read, the pool list and both dry doors still answer,
// without slots, and a launch is untouched.
func TestSlotCountFailureNeverRefusesTheAnswer(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.bothTypesPool("Capped", 1)
	e.srv.cfg.Store = failingSlotCounts{e.pg}
	pin := `"runner_pool_id":"` + pool.ID.String() + `"`

	list, raw := e.list(nil, "")
	if got := entryOf(t, list, pool.ID); got.Slots != nil || strings.Contains(raw, `"slots"`) {
		t.Fatalf("the list carries slots %+v although the count failed: %s", got.Slots, raw)
	}
	body := endBody("background", pin)
	for door, path := range map[string]string{"preflight": "/api/v1/runs/preflight", "preview": policyPreviewPath} {
		w := e.admin(http.MethodPost, path, body)
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "runner_pool_slots") {
			t.Fatalf("%s = %d %s, want 200 without runner_pool_slots", door, w.Code, w.Body)
		}
	}
}
