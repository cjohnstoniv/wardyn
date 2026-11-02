// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// refuseStoreFake answers each event handler's store call with a fixed error and records every RefuseRunnerEvent.
type refuseStoreFake struct {
	store.Store
	store.RunnerStore
	resultErr, spoolErr, runErr, refuseErr error
	run                                    types.AgentRun

	mu    sync.Mutex
	calls []store.RunnerEventRefusal
}

func (f *refuseStoreFake) RecordRunnerActionResult(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, string, time.Time,
	func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (types.RunnerPendingAction, bool, error) {
	return types.RunnerPendingAction{}, false, f.resultErr
}

func (f *refuseStoreFake) IngestRunnerDecision(context.Context, uuid.UUID, runnerspool.Record, func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (bool, error) {
	return false, f.spoolErr
}

func (f *refuseStoreFake) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return f.run, f.runErr
}

func (f *refuseStoreFake) RefuseRunnerEvent(_ context.Context, _ uuid.UUID, r store.RunnerEventRefusal, _ func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r)
	return r.ActionID != nil, f.refuseErr
}

func (f *refuseStoreFake) recorded() []store.RunnerEventRefusal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.RunnerEventRefusal(nil), f.calls...)
}

func refuseTestServer(st store.Store, now *time.Time) *Server {
	return &Server{cfg: Config{Store: st, Now: func() time.Time { return *now },
		RunnerAuditPrepare: func(_ context.Context, ev types.AuditEvent) (types.AuditEvent, error) { return ev, nil }}}
}

// Every class DESIGN §2 names: only a sentinel that names the event itself is a refusal for good; a bare
// not-found, a database error or an organisation that cannot ingest stays transient.
func TestClassifyRunnerEvent(t *testing.T) {
	runID, runnerID, actionID := uuid.New(), uuid.New(), uuid.New()
	var recordRun uuid.UUID // a zero spool record's own run
	result := mustJSON(runnerwire.ActionResult{ID: actionID, Outcome: types.RunnerActionSuccess, ObservedAt: time.Now()})
	exit := mustJSON(runnerwire.AgentExit{Generation: uuid.New(), ExecID: "exec-1", Ref: "ref", ObservedAt: time.Now()})
	erase := mustJSON(runnerwire.ResidentErase{GrantID: uuid.New(), Outcome: runnerwire.ResidentErased, ReportID: uuid.New()})
	spool := mustJSON(runnerspool.Record{})
	unknownField := mustJSON(map[string]any{"id": actionID, "outcome": types.RunnerActionSuccess, "observed_at": time.Now(), "extra": 1})
	deadlock := &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}
	ev := func(kind string, run *uuid.UUID, data []byte) runnerwire.Event {
		return runnerwire.Event{Kind: kind, RunID: run, Data: data}
	}
	for name, c := range map[string]struct {
		ev       runnerwire.Event
		fake     *refuseStoreFake // nil: a fake that answers nil
		code     string           // "" = not an EventRefusal
		is       error            // the returned error still names this
		actionID bool             // the refusal names the action it answers
	}{
		"action_result unknown field":     {ev: ev(runnerwire.EventActionResult, &runID, unknownField), code: runnerwire.RefusalShape, is: store.ErrRunnerActionRefused, actionID: true},
		"action_result no run":            {ev: ev(runnerwire.EventActionResult, nil, result), code: runnerwire.RefusalShape, actionID: true},
		"action_result undecodable":       {ev: ev(runnerwire.EventActionResult, &runID, []byte(`{`)), code: runnerwire.RefusalShape},
		"agent_exit unknown field":        {ev: ev(runnerwire.EventAgentExit, &runID, []byte(`{"extra":1}`)), code: runnerwire.RefusalShape, is: store.ErrRunnerExecutionRefused},
		"agent_exit no run":               {ev: ev(runnerwire.EventAgentExit, nil, exit), code: runnerwire.RefusalShape},
		"spool trailing data":             {ev: ev(runnerwire.EventSpool, &runID, append(append([]byte(nil), spool...), []byte(` {}`)...)), code: runnerwire.RefusalShape, is: store.ErrRunnerSpoolRefused},
		"spool run mismatch":              {ev: ev(runnerwire.EventSpool, &runID, spool), code: runnerwire.RefusalShape},
		"posture with a run":              {ev: ev(runnerwire.EventPosture, &runID, mustJSON(types.RunnerPosture{OS: "linux", Evidence: types.PostureEvidenceOSProbe})), code: runnerwire.RefusalShape},
		"resident_erase no report id":     {ev: ev(runnerwire.EventResidentErase, &runID, mustJSON(runnerwire.ResidentErase{GrantID: uuid.New(), Outcome: runnerwire.ResidentErased})), code: runnerwire.RefusalShape},
		"unknown kind":                    {ev: ev("bogus", nil, nil), code: runnerwire.RefusalUnsupportedKind},
		"action refused by the store":     {ev: ev(runnerwire.EventActionResult, &runID, result), fake: &refuseStoreFake{resultErr: store.ErrRunnerActionRefused}, code: runnerwire.RefusalStoreRefused, actionID: true},
		"action gone":                     {ev: ev(runnerwire.EventActionResult, &runID, result), fake: &refuseStoreFake{resultErr: fmt.Errorf("%w: %w", store.ErrRunnerActionRefused, store.ErrNotFound)}, code: runnerwire.RefusalStoreRefused, is: store.ErrNotFound, actionID: true},
		"action changed repeat":           {ev: ev(runnerwire.EventActionResult, &runID, result), fake: &refuseStoreFake{resultErr: fmt.Errorf("%w: %w", store.ErrRunnerActionRefused, store.ErrConflict)}, code: runnerwire.RefusalStoreRefused, is: store.ErrConflict, actionID: true},
		"spool refused by the store":      {ev: ev(runnerwire.EventSpool, &recordRun, spool), fake: &refuseStoreFake{spoolErr: store.ErrRunnerSpoolRefused}, code: runnerwire.RefusalStoreRefused},
		"resident run of another runner":  {ev: ev(runnerwire.EventResidentErase, &runID, erase), fake: &refuseStoreFake{run: types.AgentRun{ID: runID, RunnerID: &actionID}}, code: runnerwire.RefusalStoreRefused},
		"resident run gone":               {ev: ev(runnerwire.EventResidentErase, &runID, erase), fake: &refuseStoreFake{runErr: store.ErrNotFound}, code: runnerwire.RefusalUnknownRun, is: store.ErrNotFound},
		"bare not-found from the action":  {ev: ev(runnerwire.EventActionResult, &runID, result), fake: &refuseStoreFake{resultErr: store.ErrNotFound}, is: store.ErrNotFound},
		"bare not-found from the spool":   {ev: ev(runnerwire.EventSpool, &recordRun, spool), fake: &refuseStoreFake{spoolErr: store.ErrNotFound}, is: store.ErrNotFound},
		"bare conflict from the action":   {ev: ev(runnerwire.EventActionResult, &runID, result), fake: &refuseStoreFake{resultErr: store.ErrConflict}, is: store.ErrConflict},
		"database error":                  {ev: ev(runnerwire.EventActionResult, &runID, result), fake: &refuseStoreFake{resultErr: deadlock}, is: deadlock},
		"refusal not recorded":            {ev: ev(runnerwire.EventSpool, &recordRun, spool), fake: &refuseStoreFake{spoolErr: store.ErrRunnerSpoolRefused, refuseErr: deadlock}, is: store.ErrRunnerSpoolRefused},
		"exit, organisation cannot stage": {ev: ev(runnerwire.EventAgentExit, &runID, exit), is: errRunnerIngestUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			fake := c.fake
			if fake == nil {
				fake = &refuseStoreFake{}
			}
			err := refuseTestServer(fake, &now).eventFromRunner(t.Context(), runnerID, "", c.ev)
			var refusal *runnerwire.EventRefusal
			got := errors.As(err, &refusal)
			if c.code == "" && got {
				t.Fatalf("classified as a refusal for good (%s): %v", refusal.Code, err)
			}
			if c.code != "" && (!got || refusal.Code != c.code) {
				t.Fatalf("err = %v, want an EventRefusal %q", err, c.code)
			}
			if c.is != nil && !errors.Is(err, c.is) {
				t.Fatalf("err = %v, no longer names %v", err, c.is)
			}
			calls := fake.recorded()
			if c.code != "" {
				if len(calls) != 1 || calls[0].Code != c.code || (calls[0].ActionID != nil) != c.actionID ||
					(c.actionID && *calls[0].ActionID != actionID) {
					t.Fatalf("RefuseRunnerEvent calls = %+v, want one %q naming the action: %v", calls, c.code, c.actionID)
				}
			}
		})
	}

	// The sentinel a store cannot be faked to return here (StageRunnerExit is store.PG only) maps the same way.
	now := time.Now()
	fake := &refuseStoreFake{}
	srv := refuseTestServer(fake, &now)
	var refusal *runnerwire.EventRefusal
	if err := srv.classifyRunnerEvent(t.Context(), runnerID, ev(runnerwire.EventAgentExit, &runID, exit), store.ErrRunnerExecutionRefused); !errors.As(err, &refusal) ||
		refusal.Code != runnerwire.RefusalStoreRefused {
		t.Fatalf("staged exit refusal = %v, want store_refused", err)
	}

	// An organisation that cannot record a refusal fails the link, loudly, whatever the event.
	shapeless := ev(runnerwire.EventSpool, &runID, []byte(`{`))
	if err := refuseTestServer(spoolIngestStub{}, &now).eventFromRunner(t.Context(), runnerID, "", shapeless); errors.As(err, &refusal) ||
		!errors.Is(err, errRunnerIngestUnavailable) {
		t.Fatalf("store without RefuseRunnerEvent = %v, want errRunnerIngestUnavailable", err)
	}
	noPolicy := refuseTestServer(fake, &now)
	noPolicy.cfg.RunnerAuditPrepare = nil
	if err := noPolicy.eventFromRunner(t.Context(), runnerID, "", ev(runnerwire.EventActionResult, &runID, result)); errors.As(err, &refusal) ||
		!errors.Is(err, errRunnerIngestUnavailable) {
		t.Fatalf("no RunnerAuditPrepare = %v, want errRunnerIngestUnavailable", err)
	}
}

// A refusal for good is acknowledged only once its row is recorded, and the row keeps codes, a hash and a size:
// nothing the runner wrote.
func TestRefusalRowBeforeAck(t *testing.T) {
	now := time.Now()
	runID := uuid.New()
	data := []byte(`{"secret":"a value the runner sent"}`)
	fake := &refuseStoreFake{}
	srv := refuseTestServer(fake, &now)
	err := srv.eventFromRunner(t.Context(), uuid.New(), "", runnerwire.Event{Kind: runnerwire.EventSpool, RunID: &runID, Data: data})
	var refusal *runnerwire.EventRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want an EventRefusal", err)
	}
	calls := fake.recorded()
	sum := sha256.Sum256(data)
	if len(calls) != 1 || !calls[0].Row || calls[0].Marker || calls[0].Summary != nil || calls[0].Kind != runnerwire.EventSpool ||
		calls[0].Code != runnerwire.RefusalShape || calls[0].Sentinel != errRunnerEventShape.Error() ||
		calls[0].PayloadSHA256 != hex.EncodeToString(sum[:]) || calls[0].PayloadBytes != len(data) || calls[0].RunID == nil || *calls[0].RunID != runID {
		t.Fatalf("RefuseRunnerEvent calls = %+v, want one row of codes, hash and size", calls)
	}

	failing := &refuseStoreFake{refuseErr: errors.New("audit chain lock timeout")}
	err = refuseTestServer(failing, &now).eventFromRunner(t.Context(), uuid.New(), "", runnerwire.Event{Kind: runnerwire.EventSpool, RunID: &runID, Data: data})
	if errors.As(err, &refusal) || !errors.Is(err, store.ErrRunnerSpoolRefused) {
		t.Fatalf("unrecorded refusal = %v, want the handler's own error, unacknowledged", err)
	}
}

// One runner writes at most 32 rows in an hour, the 33rd marked; the rest are counted and summarised by the first
// row of a later window. Each runner has its own count.
func TestRefusalRowsBounded(t *testing.T) {
	now := time.Now().UTC()
	start := now
	fake := &refuseStoreFake{}
	srv := refuseTestServer(fake, &now)
	runnerID, runID := uuid.New(), uuid.New()
	refuse := func(id uuid.UUID, i int) {
		t.Helper()
		data := []byte(fmt.Sprintf(`{"n":%d}`, i)) // distinct events: dedupe could not bound these
		err := srv.eventFromRunner(t.Context(), id, "", runnerwire.Event{Kind: runnerwire.EventSpool, RunID: &runID, Data: data})
		var refusal *runnerwire.EventRefusal
		if !errors.As(err, &refusal) {
			t.Fatalf("refusal %d = %v", i, err)
		}
	}
	for i := range 133 {
		now = start.Add(time.Duration(i) * time.Second)
		refuse(runnerID, i)
	}
	calls := fake.recorded()
	rows, markers := 0, 0
	for i, c := range calls {
		if c.Row {
			rows++
		}
		if c.Marker {
			markers++
			if i != runnerEventRefuseRows {
				t.Errorf("marker on refusal %d, want %d", i+1, runnerEventRefuseRows+1)
			}
		}
		if c.Summary != nil {
			t.Errorf("summary inside the first window: %+v", c.Summary)
		}
	}
	if len(calls) != 133 || rows != runnerEventRefuseRows+1 || markers != 1 {
		t.Fatalf("%d calls, %d rows, %d markers: want 133, %d, 1", len(calls), rows, markers, runnerEventRefuseRows+1)
	}
	refuse(uuid.New(), 0)
	if other := fake.recorded()[133]; !other.Row || other.Marker {
		t.Fatalf("a second runner shares the first one's bound: %+v", other)
	}
	now = start.Add(runnerEventRefuseWindow)
	refuse(runnerID, 133)
	last := fake.recorded()[134]
	if !last.Row || last.Marker || last.Summary == nil || last.Summary.Suppressed != 100 || !last.Summary.WindowStart.Equal(start) ||
		!last.Summary.WindowEnd.Equal(start.Add(runnerEventRefuseWindow)) {
		t.Fatalf("first refusal of the next window = %+v (summary %+v), want its row and a summary of 100", last, last.Summary)
	}
}

// A runner that suppressed rows and then goes quiet (revoked, say) owes its summary for a day; after that its window
// is dropped by the next refusal of any other runner, instead of staying in memory.
func TestRefusalWindowOwingASummaryIsDroppedAfterADay(t *testing.T) {
	start := time.Now().UTC()
	var r runnerEventRefusals
	quiet, other := uuid.New(), uuid.New()
	for range runnerEventRefuseRows + 6 {
		r.count(quiet, start)
	}
	r.count(other, start.Add(runnerEventRefuseWindow))
	if _, ok := r.byRunner[quiet]; !ok {
		t.Fatal("a window that suppressed rows was dropped before its summary was owed for a day")
	}
	if _, _, sum := r.plan(quiet, start.Add(runnerEventRefuseWindow)); sum == nil || sum.Suppressed != 5 {
		t.Fatalf("summary owed after an hour = %+v, want 5 suppressed", sum)
	}
	r.count(other, start.Add(runnerEventRefuseOwedKeep))
	if _, ok := r.byRunner[quiet]; ok {
		t.Fatal("a window that suppressed rows stayed in memory a day after it opened")
	}
}
