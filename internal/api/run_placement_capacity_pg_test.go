// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A runner at its runs at once takes no further run: in a pool with no cap the launch and the dry doors
// refuse runner_at_capacity, and a run waiting in a pool's queue is held with its own wait reason.
// Raising the limit lets the next run in.
func TestPG_RunnerAtItsMaxRunsRefusesOrWaits(t *testing.T) {
	e := newPlacementEnv(t)
	laptops := e.createPool("Laptops", types.RunnerPoolSelfHosted)
	awake := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	e.addToPool(e.person, laptops.ID, awake)
	e.stream(awake, types.CC1, types.CC2)
	e.personDefaults(e.person, fmt.Sprintf(`{"preferred_hosting":"self_hosted","self_hosted":%q}`, laptops.ID))
	if _, err := e.pool.Exec(context.Background(), `UPDATE runners SET max_runs = 1 WHERE id = $1`, awake); err != nil {
		t.Fatal(err)
	}

	first := e.localCreate(runBody(""))
	if first.RunnerID == nil || *first.RunnerID != awake {
		t.Fatalf("first run bound to %v, want %s", first.RunnerID, awake)
	}
	for _, door := range []string{"/api/v1/runs", "/api/v1/runs/preflight"} {
		w := e.call(e.person, http.MethodPost, door, runBody(""))
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `"reason":"`+string(placement.ReasonRunnerAtCapacity)+`"`) ||
			!strings.Contains(w.Body.String(), runnerpool.RunnerAtCapacityMsg("Laptops")) {
			t.Fatalf("%s on a full runner = %d %s, want 422 runner_at_capacity", door, w.Code, w.Body)
		}
	}

	room, err := e.srv.queueRunnerFree(withOIDCHuman(context.Background(), e.personP), laptops, queuedLaunch{Request: []byte(runBody(""))})
	if err != nil || room.wait != runnerpool.WaitRunnersAtCapacity || len(room.runners) != 0 {
		t.Fatalf("the queue's verdict on a full runner = %+v, %v; want it held for runners_at_capacity", room, err)
	}

	if w := e.call(e.person, http.MethodPut, "/api/v1/me/runners/"+awake.String(), `{"max_runs":2}`); w.Code != http.StatusOK {
		t.Fatalf("owner raise = %d %s", w.Code, w.Body)
	}
	if second := e.localCreate(runBody("")); second.RunnerID == nil || *second.RunnerID != awake {
		t.Fatalf("after the raise the run went to %v", second.RunnerID)
	}
}
