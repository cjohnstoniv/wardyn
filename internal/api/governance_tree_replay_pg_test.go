// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// treeReplayFixture is a unitAdminEnv with a person and a live run in ua-a1 and in ua-b, and a held widening
// of ua-a1. Nothing the comparison of a change to ua-a1 reports may name ua-b's person or run.
type treeReplayFixture struct {
	*unitAdminEnv
	held              types.GovernanceChange
	a1Person, bPerson types.GovernanceMembership
	bRun              uuid.UUID
}

func newTreeReplayFixture(t *testing.T) *treeReplayFixture {
	t.Helper()
	f := &treeReplayFixture{unitAdminEnv: newUnitAdminEnv(t, true)}
	f.a1Person = treeMember(t, f.govEnv, f.a1, types.CapabilitySubjectUser, "sub-replay-a1", 1, false)
	f.bPerson = treeMember(t, f.govEnv, f.b, types.CapabilitySubjectUser, "sub-replay-b", 1, false)
	f.bRun = uuid.New()
	if _, err := f.pg.CreateRun(context.Background(), types.AgentRun{ID: f.bRun, CreatedBy: "sub-replay-b-runner", Agent: "claude-code",
		ConfinementClass: types.CC2, State: types.RunRunning, RunnerTarget: "docker", Task: "t", Title: "b ledger",
		SPIFFEID: "spiffe://wardyn.local/agent-run/" + f.bRun.String(), SandboxRef: "wardyn-agent-" + f.bRun.String(), UnitID: &f.b.ID}); err != nil {
		t.Fatal(err)
	}
	f.held = f.pending(f.call(f.adminA, http.MethodPut, "/api/v1/governance/units/"+f.a1.ID.String(), f.a1Widening()))
	return f
}

func (f *treeReplayFixture) a1Widening() string {
	return uaUnitBody("ua-a1", f.a.ID, uaDomains("pypi.org", "github.com", "npmjs.org"), nil)
}

// replayReaders are the two callers of the shared replay: a held change's impact and the tree dry run.
var replayReaders = []struct {
	name string
	read func(f *treeReplayFixture) *httptest.ResponseRecorder
}{
	{"impact", func(f *treeReplayFixture) *httptest.ResponseRecorder {
		return f.call(f.bob, http.MethodGet, impactPath(f.held.ID), "")
	}},
	{"dry run", func(f *treeReplayFixture) *httptest.ResponseRecorder {
		return f.admin(http.MethodPost, "/api/v1/governance/dry-run",
			`{"target":"unit","id":"`+f.a1.ID.String()+`",`+strings.TrimPrefix(f.a1Widening(), "{"))
	}},
}

// readReplay decodes either reader's answer into the people and live run ids it reports.
func readReplay(t *testing.T, w *httptest.ResponseRecorder) (people []treeDryRunPerson, runs []uuid.UUID) {
	t.Helper()
	var body struct {
		People   json.RawMessage `json:"people"`
		LiveRuns json.RawMessage `json:"live_runs"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil || json.Unmarshal(body.People, &people) != nil {
		t.Fatalf("answer = %d %s, want 200 with people", w.Code, w.Body)
	}
	var ids []uuid.UUID
	if json.Unmarshal(body.LiveRuns, &ids) != nil { // the impact lists rows, the dry run ids
		var rows []changeImpactRun
		if err := json.Unmarshal(body.LiveRuns, &rows); err != nil {
			t.Fatalf("live_runs %s: %v", body.LiveRuns, err)
		}
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
	}
	return people, ids
}

// checkNoUnitB fails on any trace of ua-b in an answer about ua-a1, and requires ua-a1's person, so the
// comparison is known to have run.
func (f *treeReplayFixture) checkNoUnitB(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	people, runs := readReplay(t, w)
	if !slices.ContainsFunc(people, func(p treeDryRunPerson) bool { return p.MembershipID == f.a1Person.ID }) {
		t.Errorf("people = %+v, want ua-a1's person", people)
	}
	if slices.ContainsFunc(people, func(p treeDryRunPerson) bool { return p.UnitID == f.b.ID || p.FromUnitID == f.b.ID }) || slices.Contains(runs, f.bRun) {
		t.Errorf("people %+v, live runs %v: name ua-b, which the change does not touch", people, runs)
	}
	for _, leak := range []string{f.bPerson.ID.String(), f.bPerson.Subject, f.b.ID.String(), f.bRun.String(), "sub-replay-b-runner"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("the answer about ua-a1 holds %q from ua-b: %s", leak, w.Body)
		}
	}
}

// atTreeStage makes the treeDryRunPause seam run write once, on its own goroutine, when the comparison
// reaches stage, and return once the write has finished or is waiting for the governance graph lock.
// The returned func waits for the write.
func atTreeStage(t *testing.T, e *govEnv, stage string, write func()) (wait func()) {
	t.Helper()
	prev := treeDryRunPause
	t.Cleanup(func() { treeDryRunPause = prev })
	done, fired := make(chan struct{}), false
	treeDryRunPause = func(at string) {
		if at != stage || fired {
			return
		}
		fired = true
		go func() { defer close(done); write() }()
		waitWriteOrGraphWaiter(t, e, done)
	}
	return func() {
		t.Helper()
		if !fired {
			t.Fatalf("the comparison never reached %q", stage)
		}
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("the concurrent write never finished")
		}
	}
}

// waitWriteOrGraphWaiter returns once done is closed or a session of this database waits for the
// governance graph lock (pg_advisory_xact_lock(class, 0): classid class, objid 0, objsubid 2).
func waitWriteOrGraphWaiter(t *testing.T, e *govEnv, done <-chan struct{}) {
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		select {
		case <-done:
			return
		default:
		}
		var waiting int
		if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory' AND NOT granted AND classid::bigint = $1 AND objid = 0 AND objsubid = 2
			AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`,
			int64(db.GovernanceGraphLockClass)).Scan(&waiting); err != nil {
			t.Errorf("read pg_locks: %v", err)
			return
		}
		if waiting > 0 {
			return
		}
	}
	t.Error("the concurrent write neither finished nor waited for the governance graph lock")
}

// TestPG_TreeReplay_UnrelatedUnitWrite: a write to ua-b arriving while a held change's impact or the
// tree dry run replays a change to ua-a1 (after the first read of the tree) waits for the graph lock the
// replay holds, so neither answer names ua-b's person, its policy or its live run. Before the lock was
// held from the first read, the write landed between the reads and ua-b's people and run were reported.
func TestPG_TreeReplay_UnrelatedUnitWrite(t *testing.T) {
	for _, reader := range replayReaders {
		t.Run(reader.name, func(t *testing.T) {
			f := newTreeReplayFixture(t)
			var written *httptest.ResponseRecorder
			wait := atTreeStage(t, f.govEnv, "replay", func() {
				written = f.admin(http.MethodPut, "/api/v1/governance/units/"+f.b.ID.String(),
					uaUnitBody("ua-b", store.DefaultUnitID, uaDomains("pypi.org", "github.com"), nil))
			})
			w := reader.read(f)
			wait()
			if written.Code != http.StatusOK {
				t.Fatalf("the ua-b write = %d %s", written.Code, written.Body)
			}
			if got := mustJSONString(t, f.profile(f.b.ID).Overlay); !strings.Contains(got, "github.com") {
				t.Fatalf("ua-b's overlay = %s, want the concurrent write's", got)
			}
			f.checkNoUnitB(t, w)
		})
	}
}

// TestPG_TreeReplay_BoundaryWriteBetweenSides: a boundary write landing between resolving the two
// sides of the comparison changes neither, since both resolve against the one boundary and depth read
// under the replay's lock; ua-b, untouched by the change, stays out of both answers. Before, each side
// read the boundary afresh, and every unit's walls looked changed by the change.
func TestPG_TreeReplay_BoundaryWriteBetweenSides(t *testing.T) {
	for _, reader := range replayReaders {
		t.Run(reader.name, func(t *testing.T) {
			f := newTreeReplayFixture(t)
			var putErr error
			wait := atTreeStage(t, f.govEnv, "walls", func() {
				_, putErr = f.pg.PutBoundary(context.Background(), testBoundary(types.BoundaryHardLimits), "seed")
			})
			w := reader.read(f)
			wait()
			if putErr != nil {
				t.Fatalf("the boundary write: %v", putErr)
			}
			if b, err := f.pg.GetBoundary(context.Background()); err != nil || !b.HardLimits() {
				t.Fatalf("boundary = %+v %v, want the concurrent hard_limits write", b, err)
			}
			f.checkNoUnitB(t, w)
		})
	}
}
