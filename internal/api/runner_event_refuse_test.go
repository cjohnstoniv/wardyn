// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
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

func (f *refuseStoreFake) RefuseRunnerEvent(_ context.Context, _ uuid.UUID, r store.RunnerEventRefusal, _ func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r)
	return r.ActionID != nil, true, f.refuseErr
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
	if len(calls) != 1 || calls[0].Kind != runnerwire.EventSpool ||
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

// The bound on refusal rows is the store's, counted in the transaction that writes them: no replica drops, plans or
// counts a refusal itself, so two replicas hand every refusal of one runner to the store, each with its own hash, and
// acknowledge each one it records.
func TestRefusalBoundIsTheStores(t *testing.T) {
	now := time.Now()
	fake := &refuseStoreFake{}
	replicas := []*Server{refuseTestServer(fake, &now), refuseTestServer(fake, &now)}
	runnerID, runID := uuid.New(), uuid.New()
	// Past the 33 rows a window writes, and distinct events: dedupe could not bound these.
	const n = 70
	payload := func(i int) []byte { return []byte(fmt.Sprintf(`{"n":%d}`, i)) }
	for i := range n {
		err := replicas[i%2].eventFromRunner(t.Context(), runnerID, "", runnerwire.Event{Kind: runnerwire.EventSpool, RunID: &runID, Data: payload(i)})
		var refusal *runnerwire.EventRefusal
		if !errors.As(err, &refusal) {
			t.Fatalf("refusal %d = %v, want an EventRefusal", i, err)
		}
	}
	calls := fake.recorded()
	if len(calls) != n {
		t.Fatalf("%d RefuseRunnerEvent calls, want %d: a replica dropped a refusal the store must count", len(calls), n)
	}
	for i, c := range calls {
		if sum := sha256.Sum256(payload(i)); c.PayloadSHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("call %d carries %s, want the hash of refusal %d", i, c.PayloadSHA256, i)
		}
	}
}

// A refusal reads an overflow summary's interval for the store, and a sequenced record's place never depends on it.
func TestRefusalReadsTheSpoolPlace(t *testing.T) {
	now := time.Now()
	runID, spoolID := uuid.New(), uuid.New()
	hash := strings.Repeat("a", 64)
	for name, tc := range map[string]struct {
		data string
		want *store.RunnerEventRefusalSpool
	}{
		"summary": {`{"spool_id":"` + spoolID.String() + `","seq":0,"hash":"` + hash + `","count":2,"drop_end":5,"later":1}`,
			&store.RunnerEventRefusalSpool{RunID: runID, SpoolID: spoolID, Hash: hash, Count: 2, DropEnd: 5}},
		"summary with an unreadable interval": {`{"spool_id":"` + spoolID.String() + `","seq":0,"hash":"` + hash + `","count":"x","drop_end":5,"later":1}`,
			&store.RunnerEventRefusalSpool{RunID: runID, SpoolID: spoolID, Hash: hash}},
		"sequenced record": {`{"spool_id":"` + spoolID.String() + `","seq":2,"hash":"` + hash + `","count":"x","later":1}`,
			&store.RunnerEventRefusalSpool{RunID: runID, SpoolID: spoolID, Seq: 2, Hash: hash}},
		"data with no place": {`{`, nil},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &refuseStoreFake{}
			err := refuseTestServer(fake, &now).eventFromRunner(t.Context(), uuid.New(), "", runnerwire.Event{Kind: runnerwire.EventSpool, RunID: &runID, Data: []byte(tc.data)})
			var refusal *runnerwire.EventRefusal
			if !errors.As(err, &refusal) {
				t.Fatalf("err = %v, want an EventRefusal", err)
			}
			calls := fake.recorded()
			if len(calls) != 1 {
				t.Fatalf("RefuseRunnerEvent calls = %+v, want one", calls)
			}
			if got := calls[0].Spool; (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("spool place = %+v, want %+v", got, tc.want)
			}
		})
	}
}
