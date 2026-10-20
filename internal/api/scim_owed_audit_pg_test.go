// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// failOweOnce fails the first insert of an owed refusal row, as a transient store error on that one statement.
type failOweOnce struct {
	store.PG
	fired atomic.Bool
}

func (s *failOweOnce) EnsureDeprovisionJobs(ctx context.Context, id uuid.UUID, kind string, keys []store.JobKey) error {
	if len(keys) == 1 && strings.HasPrefix(keys[0].Step, store.JobStepRefusalAudit) && !s.fired.Swap(true) {
		return errors.New("ledger insert failed")
	}
	return s.PG.EnsureDeprovisionJobs(ctx, id, kind, keys)
}

// failFinishOwed fails the completion write of an owed refusal row while fail is set.
type failFinishOwed struct {
	store.PG
	fail atomic.Bool
}

func (s *failFinishOwed) FinishDeprovisionJob(ctx context.Context, id uuid.UUID, kind string, k store.JobKey, detail map[string]int) error {
	if s.fail.Load() && strings.HasPrefix(k.Step, store.JobStepRefusalAudit) {
		return errors.New("ledger write failed")
	}
	return s.PG.FinishDeprovisionJob(ctx, id, kind, k, detail)
}

// A refused suspension whose owed-row insert fails once still writes its refusal row: the cut stores its
// marker together with the obligation, so the retry that treats the refusal as recorded finds it owed.
func TestSCIMSuspensionKeepsItsRefusalRowWhenTheOweInsertFails(t *testing.T) {
	const email = "twin@corp.example"
	e := newSCIMEnv(t)
	e.seedToken("sub-t1", email)
	e.seedToken("sub-t2", email)
	n := e.node(func(c *Config) { c.Store = &failOweOnce{PG: e.st} })
	n.audit.fail("person.deprovision", true)
	id := e.postUserID(e.a, oidGhost, email, email)
	if w := e.patch(n, id, patchOf("false")); w.Code != http.StatusInternalServerError {
		t.Fatalf("suspend = %d %s, want 5xx", w.Code, w.Body.String())
	}
	e.sweepAs(n, id)
	n.audit.fail("person.deprovision", false)
	e.sweepAs(n, id)
	e.sweepAs(n, id)
	if got := refusalReasons(t, e, e.a, e.b, n); !maps.Equal(got, map[string]int{reasonOwnerAmbiguous: 1}) {
		t.Errorf("person.deprovision reasons = %v, want the refusal row once", got)
	}
}

// The same for a group removal.
func TestSCIMGroupRemovalKeepsItsRefusalRowWhenTheOweInsertFails(t *testing.T) {
	e, _, g, patch := newAmbiguousGroupMember(t)
	n := e.node(func(c *Config) { c.Store = &failOweOnce{PG: e.st} })
	n.audit.fail("person.deprovision", true)
	if r := e.patchGroup(n, g.ID, patch); r.Code != http.StatusInternalServerError {
		t.Fatalf("the removal = %d %s, want 5xx while the owner is ambiguous", r.Code, r.Body.String())
	}
	n.audit.fail("person.deprovision", false)
	if r := e.patchGroup(n, g.ID, patch); r.Code != http.StatusInternalServerError {
		t.Fatalf("the retry = %d %s, want 5xx while the owner is ambiguous", r.Code, r.Body.String())
	}
	if got := refusalReasons(t, e, e.a, e.b, n); !maps.Equal(got, map[string]int{reasonOwnerAmbiguous: 1}) {
		t.Errorf("person.deprovision reasons = %v, want the refusal row once", got)
	}
}

// One owed obligation whose completion write keeps failing emits the same event each time: one id and
// one time, so a repeat is recognisable, and the row is completed once the write works.
func TestSCIMOwedRefusalRowReplaysTheSameEvent(t *testing.T) {
	e, id, _ := ambiguousLeaver(t)
	led := &failFinishOwed{PG: e.st}
	led.fail.Store(true)
	n := e.node(func(c *Config) { c.Store = led })
	e.a.audit.fail("person.deprovision", false)
	e.b.audit.fail("person.deprovision", false)
	for range 3 {
		e.sweepAs(n, id)
	}
	rows := e.rows(n, "person.deprovision")
	if len(rows) < 2 {
		t.Fatalf("%d refusal rows written while the completion write failed, want the obligation replayed", len(rows))
	}
	for _, r := range rows[1:] {
		if r.ID != rows[0].ID || !r.Time.Equal(rows[0].Time) {
			t.Errorf("replayed row %s at %v, want the first row's %s at %v", r.ID, r.Time, rows[0].ID, rows[0].Time)
		}
	}
	led.fail.Store(false)
	e.sweepAs(n, id)
	for _, j := range e.ledger(id) {
		if strings.HasPrefix(j.Step, store.JobStepRefusalAudit) && !j.Done {
			t.Errorf("the owed row %+v is still pending after the completion write worked", j)
		}
	}
}
