// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// activationStore is the activation's store surface over a map-free double.
type activationStore struct {
	store.Store
	on       bool
	lose     bool // another caller's write set the mark first
	sessions []types.ClientSession
	rec      *store.PortalActivation // the committed record, as the winning write stored it
	settled  bool
	stored   func(uuid.UUID) (time.Time, bool) // the shared audit table's time for an event, if it holds it
}

func (s *activationStore) holds(id uuid.UUID) bool {
	at, ok := s.stored(id)
	return ok && at.Equal(s.rec.EventAt)
}

func (s *activationStore) PortalIssuersActivated(context.Context) (bool, error) { return s.on, nil }
func (s *activationStore) ActivatePortalIssuersAs(_ context.Context, a store.PortalActivation) (bool, error) {
	if s.lose {
		return false, nil
	}
	s.on = true
	a.ActivateEventID, a.EventAt = uuid.New(), time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)
	if len(a.Sessions) > 0 {
		a.OverrideEventID = uuid.New()
	}
	s.rec = &a
	return true, nil
}
func (s *activationStore) PendingPortalActivation(context.Context) (store.PortalActivation, bool, error) {
	if s.rec == nil || s.settled {
		return store.PortalActivation{}, false, nil
	}
	return *s.rec, true, nil
}
func (s *activationStore) AuditEventsStored(_ context.Context, want []store.AuditEventKey) (map[store.AuditEventKey]bool, error) {
	out := map[store.AuditEventKey]bool{}
	if s.stored != nil {
		for _, k := range want {
			if at, ok := s.stored(k.ID); ok && at.Equal(k.At) {
				out[store.AuditEventKey{ID: k.ID, At: k.At.UTC().Truncate(time.Microsecond)}] = true
			}
		}
	}
	return out, nil
}

// SettlePortalActivation settles only when both events are in the audit table, and drops the raw record, as the
// store's one statement does.
func (s *activationStore) SettlePortalActivation(context.Context) (bool, error) {
	if s.rec == nil || s.settled || s.stored == nil || !s.holds(s.rec.ActivateEventID) ||
		(len(s.rec.Sessions) > 0 && !s.holds(s.rec.OverrideEventID)) {
		return false, nil
	}
	s.settled, s.rec = true, &store.PortalActivation{}
	return true, nil
}
func (s *activationStore) UntaggedClientSessions(context.Context) ([]types.ClientSession, error) {
	return s.sessions, nil
}

// The activation needs the operator's statement that older servers are stopped: a body without it, or with it
// false, is 422 and switches nothing on.
func TestPortalIssuersActivate_NeedsOldServersStoppedStatement(t *testing.T) {
	fake := &activationStore{}
	h := newHarness(t)
	srv := New(baseTestConfig(h, fake))
	for _, body := range []string{`{}`, `{"old_servers_stopped":false}`, `{"confirm_listed_sessions":true}`} {
		w := do(t, srv, http.MethodPost, "/api/v1/admin/portal-issuers/activate", adminToken, body)
		if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonPortalIssuersStatementRequired {
			t.Errorf("activate with %s = %d %s, want 422 %s", body, w.Code, w.Body, reasonPortalIssuersStatementRequired)
		}
	}
	if fake.on {
		t.Error("a refused activation switched issuer portals on")
	}
}

// Two operators activating at once: the caller whose write did not set the mark is told it is already on, and
// no activation or override row names it.
func TestPortalIssuersActivate_LoserOfARaceIsAlreadyActivated(t *testing.T) {
	fake := &activationStore{lose: true, sessions: []types.ClientSession{{Label: "old-wardynd", ClientAddr: "10.0.0.9"}}}
	h := newHarness(t)
	srv := New(baseTestConfig(h, fake))
	for i := 0; i < 14; i++ {
		srv.portalMonitor().record(time.Now().Add(time.Duration(i-14)*portalSampleEvery), fake.sessions)
	}
	w := do(t, srv, http.MethodPost, "/api/v1/admin/portal-issuers/activate", adminToken, `{"old_servers_stopped":true,"confirm_listed_sessions":true}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"already_activated":true`) {
		t.Fatalf("the losing call = %d %s, want 200 already_activated", w.Code, w.Body)
	}
	for _, ev := range h.audit.snapshot() {
		if strings.HasPrefix(ev.Action, "portal_issuers.activate") {
			t.Errorf("the loser wrote %s", ev.Action)
		}
	}
}

// Started as the daemon starts it, the monitor takes its first sample at once: the replica's rolling window has a
// sample in it and the activation no longer answers "never sampled".
func TestPortalActivationMonitor_RunReachesObservedSample(t *testing.T) {
	fake := &activationStore{}
	srv := New(baseTestConfig(newHarness(t), fake))
	m := srv.portalMonitor()
	if _, wait := m.suspects(time.Now()); wait != portalSampleWindow {
		t.Fatalf("before the monitor runs: wait %s, want a whole window", wait)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.RunPortalActivationMonitor(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		sampled := !m.last.IsZero() && len(m.samples) == 1
		m.mu.Unlock()
		if sampled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the running monitor never recorded a sample")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the monitor outlived its context")
	}
}

// The mark is a store-only key: a PUT /site-config body that names it is 400 and changes nothing.
func TestPortalIssuersActivate_BodyNamingMarkRefused(t *testing.T) {
	fake := &fakeSiteConfigStore{}
	srv, audit := newSiteConfigHarness(t, fake)
	for _, key := range []string{store.PortalIssuersActivatedKey, store.PortalActivationKey} {
		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"`+key+`":"2026-01-01T00:00:00Z"}`)
		if w.Code != http.StatusBadRequest || errorReason(w) != reasonSiteConfigRequestInvalid {
			t.Fatalf("a body naming %s = %d %s, want 400 %s", key, w.Code, w.Body, reasonSiteConfigRequestInvalid)
		}
	}
	if fake.putSeen != nil || len(audit.events) != 0 {
		t.Errorf("the refused body reached the store (%+v) or the audit (%d rows)", fake.putSeen, len(audit.events))
	}
}

// sharedAudit stands for the shared audit table: it keeps one row per event id, as a replay does.
type sharedAudit struct {
	recRecorder
}

func (a *sharedAudit) Record(ctx context.Context, ev types.AuditEvent) error {
	if a.has(ev.ID) {
		return nil
	}
	return a.recRecorder.Record(ctx, ev)
}

func (a *sharedAudit) has(id uuid.UUID) bool {
	_, ok := a.timeOf(id)
	return ok
}

func (a *sharedAudit) timeOf(id uuid.UUID) (time.Time, bool) {
	for _, ev := range a.snapshot() {
		if ev.ID == id {
			return ev.Time, true
		}
	}
	return time.Time{}, false
}

// localSpoolAudit is a replica whose store write fails for the held event ids and whose local spool takes
// them, with the two answers production gives: the spooling recorder's error wrapping audit.ErrSpooled, and the
// sealing recorder's nil for a row it appended to the spool alone.
type localSpoolAudit struct {
	shared  *sharedAudit
	hold    map[uuid.UUID]bool
	nilBack bool
	spooled []types.AuditEvent
}

func (l *localSpoolAudit) Record(ctx context.Context, ev types.AuditEvent) error {
	if !l.hold[ev.ID] {
		return l.shared.Record(ctx, ev)
	}
	l.spooled = append(l.spooled, ev)
	if l.nilBack {
		return nil
	}
	return fmt.Errorf("%w: %w", audit.ErrSpooled, errors.New("the audit store is down"))
}

// A record is settled only when the shared audit table holds both rows. The replica that lost one to its
// local spool, whether the recorder answered ErrSpooled or nil, leaves the record owed and sends nothing
// again for a minute; a second replica with a working store delivers the missing row with the record's ids,
// time, actor and sessions, then settles, and a settled record owes and writes nothing.
func TestPortalActivationDelivery_SettlesOnlyWhatTheSharedTableHolds(t *testing.T) {
	for _, nilBack := range []bool{false, true} {
		fake := &activationStore{}
		h := newHarness(t)
		clock := time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)
		table := &sharedAudit{}
		fake.stored = table.timeOf
		newServer := func(rec audit.Recorder) *Server {
			srv := New(baseTestConfig(h, fake))
			srv.cfg.Audit = rec
			srv.cfg.Now = func() time.Time { return clock }
			return srv
		}
		first := newServer(table)
		first.deliverPortalActivation(context.Background(), fake)
		if len(table.snapshot()) != 0 || first.portalMonitor().owed() {
			t.Fatalf("nilBack=%v, nothing committed: %d rows, owed %v; want 0 and not owed", nilBack, len(table.snapshot()), first.portalMonitor().owed())
		}

		if won, err := fake.ActivatePortalIssuersAs(context.Background(), store.PortalActivation{
			ActorType: string(types.ActorHuman), Actor: "ops@example.com", Sessions: []string{"billing-job@10.0.0.9"}}); err != nil || !won {
			t.Fatalf("seed the committed activation = %v, %v", won, err)
		}
		rec := *fake.rec
		local := &localSpoolAudit{shared: table, hold: map[uuid.UUID]bool{rec.OverrideEventID: true}, nilBack: nilBack}
		first = newServer(local)
		first.deliverPortalActivation(context.Background(), fake)
		if fake.settled || !first.portalMonitor().owed() || !table.has(rec.ActivateEventID) || table.has(rec.OverrideEventID) || len(local.spooled) != 1 {
			t.Fatalf("nilBack=%v, one row in the shared table, one in the local spool: settled %v owed %v activate-in-table %v override-in-table %v spooled %d; want not settled, owed, 1, 0, 1",
				nilBack, fake.settled, first.portalMonitor().owed(), table.has(rec.ActivateEventID), table.has(rec.OverrideEventID), len(local.spooled))
		}
		first.deliverPortalActivation(context.Background(), fake)
		if len(local.spooled) != 1 {
			t.Errorf("nilBack=%v, a tick inside the retry wait spooled %d lines, want 1", nilBack, len(local.spooled))
		}

		// The first replica and its spool are gone. Another replica sees the missing row and delivers it.
		second := newServer(table)
		second.deliverPortalActivation(context.Background(), fake)
		got := table.snapshot()
		if !fake.settled || second.portalMonitor().owed() || len(got) != 2 {
			t.Fatalf("nilBack=%v, after the second replica: settled %v owed %v rows %d; want settled, not owed, 2 rows", nilBack, fake.settled, second.portalMonitor().owed(), len(got))
		}
		for i, want := range []struct {
			action string
			id     uuid.UUID
		}{{"portal_issuers.activate", rec.ActivateEventID}, {"portal_issuers.activate_override", rec.OverrideEventID}} {
			ev := got[i]
			if ev.Action != want.action || ev.ID != want.id || !ev.Time.Equal(rec.EventAt) || ev.Actor != "ops@example.com" || ev.ActorType != types.ActorHuman {
				t.Errorf("nilBack=%v row %d = %s id %s time %s actor %s/%s; want %s id %s time %s actor human/ops@example.com",
					nilBack, i, ev.Action, ev.ID, ev.Time, ev.ActorType, ev.Actor, want.action, want.id, rec.EventAt)
			}
		}
		if !strings.Contains(string(got[1].Data), "billing-job@10.0.0.9") {
			t.Errorf("nilBack=%v, the override row's data %s does not name the overridden session", nilBack, got[1].Data)
		}
		if fake.rec.Actor != "" || len(fake.rec.Sessions) != 0 {
			t.Errorf("nilBack=%v, the settled record still holds %+v", nilBack, *fake.rec)
		}
		second.deliverPortalActivation(context.Background(), fake)
		if n := len(table.snapshot()); n != 2 {
			t.Errorf("nilBack=%v, a tick after settlement wrote %d more rows, want none", nilBack, n-2)
		}
	}

	// An activation with no overridden session owes one row.
	plain := &activationStore{}
	plainTable := &sharedAudit{}
	plain.stored = plainTable.timeOf
	psrv := New(baseTestConfig(newHarness(t), plain))
	psrv.cfg.Audit = plainTable
	_, _ = plain.ActivatePortalIssuersAs(context.Background(), store.PortalActivation{ActorType: string(types.ActorHuman), Actor: "ops@example.com"})
	psrv.deliverPortalActivation(context.Background(), plain)
	if n := len(plainTable.snapshot()); n != 1 || !plain.settled {
		t.Errorf("an activation with no override owes %d rows (settled %v), want 1 and settled", n, plain.settled)
	}
}
