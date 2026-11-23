// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The copy-out door into a kept run, against a real Postgres: the run is a local run on a person's claimed
// Kubernetes runner, marked lost to an outage with the store's own writes.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// keptCopyOutPG is newContainmentEnv with a Kubernetes runner's caps and the run lost to an outage.
func keptCopyOutPG(t *testing.T) *containmentEnv {
	t.Helper()
	e := newContainmentEnv(t, false)
	seedRunnerCaps(t, e.srv, e.runnerID, runnerwire.Caps{EndsUnkept: true})
	if ok, err := e.pg.MarkRunLost(context.Background(), e.run.ID, types.LostOutage, time.Now(), 0); err != nil || !ok {
		t.Fatalf("MarkRunLost: %v %v", ok, err)
	}
	return e
}

func (e *containmentEnv) mint(c *http.Cookie) (int, string) {
	e.t.Helper()
	w := e.call(c, http.MethodPost, "/api/v1/runs/"+e.run.ID.String()+"/attach-ticket", "")
	return w.Code, w.Body.String()
}

func (e *containmentEnv) keptCopyOutField(c *http.Cookie) bool {
	e.t.Helper()
	w := e.call(c, http.MethodGet, "/api/v1/runs/"+e.run.ID.String(), "")
	if w.Code != http.StatusOK {
		e.t.Fatalf("GET run = %d %s", w.Code, w.Body)
	}
	var out struct {
		KeptCopyOut bool `json:"kept_copy_out"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		e.t.Fatal(err)
	}
	return out.KeptCopyOut
}

func TestPG_KeptCopyOut_TheOwnerMintsATicketUntilTheAgentStops(t *testing.T) {
	e := keptCopyOutPG(t)
	if code, body := e.mint(e.person); code != http.StatusOK {
		t.Fatalf("the owner's mint = %d %s, want 200", code, body)
	}
	if code, body := e.mint(e.other); code != http.StatusNotFound {
		t.Fatalf("another person's mint = %d %s, want 404", code, body)
	}
	if !e.keptCopyOutField(e.person) {
		t.Fatal("the owner reads kept_copy_out absent, want true")
	}
	got, err := e.pg.GetRun(context.Background(), e.run.ID)
	if err != nil || got.LostAt == nil {
		t.Fatalf("GetRun: %v %+v", err, got.LostAt)
	}
	// PG stores microseconds: the lost mark is read back, never the Go time written.
	if err := e.pg.MarkRunAgentStopped(context.Background(), e.run.ID, *got.LostAt); err != nil {
		t.Fatalf("MarkRunAgentStopped: %v", err)
	}
	code, body := e.mint(e.person)
	if code != http.StatusConflict || !strings.Contains(body, keptCopyOutAgentStopped) {
		t.Fatalf("the owner's mint after the stop = %d %s, want 409 %q", code, body, keptCopyOutAgentStopped)
	}
	if e.keptCopyOutField(e.person) {
		t.Fatal("kept_copy_out is still true after the agent stopped")
	}
}

func TestPG_KeptCopyOut_ContainmentPendingAndTheEndCloseTheDoor(t *testing.T) {
	e := keptCopyOutPG(t)
	e.mustPage(&runnerwire.ContainmentPending{Kind: runner.ContainmentAgentOnly, Since: time.Now().UTC(), Tuple: e.tuple()}, nil)
	code, body := e.mint(e.person)
	if code != http.StatusConflict || !strings.Contains(body, keptCopyOutRemoved) {
		t.Fatalf("the owner's mint with containment pending = %d %s, want 409 %q", code, body, keptCopyOutRemoved)
	}

	e = keptCopyOutPG(t)
	if _, err := e.pool.Exec(context.Background(), `UPDATE agent_runs SET ends_at = now() - interval '1 minute' WHERE id = $1`, e.run.ID); err != nil {
		t.Fatal(err)
	}
	code, body = e.mint(e.person)
	if code != http.StatusConflict || !strings.Contains(body, keptCopyOutPastEnd) {
		t.Fatalf("the owner's mint past the run's end = %d %s, want 409 %q", code, body, keptCopyOutPastEnd)
	}
}
