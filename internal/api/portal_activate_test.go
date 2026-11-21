// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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
	marked   bool
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
	if s.rec == nil || s.marked {
		return store.PortalActivation{}, false, nil
	}
	return *s.rec, true, nil
}
func (s *activationStore) MarkPortalActivationAudited(context.Context) (bool, error) {
	s.marked = true
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

// downAudit is an audit recorder that refuses every event while down is set, as an audit store that is out.
type downAudit struct {
	recRecorder
	down bool
}

func (d *downAudit) Record(ctx context.Context, ev types.AuditEvent) error {
	if d.down {
		return errors.New("the audit store is down")
	}
	return d.recRecorder.Record(ctx, ev)
}

// The replay decision. A committed activation whose rows were not accepted stays owed and the monitor retries
// it soon; once accepted it is delivered with the record's own ids, time, actor and sessions and marked, and a
// later tick owes and writes nothing. A record with no overridden session owes one row, not two; with nothing
// committed nothing is written.
func TestPortalActivationDelivery_ReplaysTheCommittedRecordOnce(t *testing.T) {
	fake := &activationStore{}
	h := newHarness(t)
	srv := New(baseTestConfig(h, fake))
	rec := &downAudit{down: true}
	srv.cfg.Audit = rec

	srv.deliverPortalActivation(context.Background(), fake)
	if n := len(rec.snapshot()); n != 0 || srv.portalMonitor().owed() {
		t.Fatalf("with nothing committed: %d rows, owed %v; want 0 and not owed", n, srv.portalMonitor().owed())
	}

	if won, err := fake.ActivatePortalIssuersAs(context.Background(), store.PortalActivation{
		ActorType: string(types.ActorHuman), Actor: "ops@example.com", Sessions: []string{"billing-job@10.0.0.9"}}); err != nil || !won {
		t.Fatalf("seed the committed activation = %v, %v", won, err)
	}
	srv.deliverPortalActivation(context.Background(), fake)
	if fake.marked || !srv.portalMonitor().owed() {
		t.Fatalf("rows refused by the audit store: marked %v, owed %v; want unmarked and owed", fake.marked, srv.portalMonitor().owed())
	}

	rec.down = false
	srv.deliverPortalActivation(context.Background(), fake)
	got := rec.snapshot()
	if !fake.marked || srv.portalMonitor().owed() || len(got) != 2 {
		t.Fatalf("after the audit store returned: marked %v, owed %v, %d rows; want marked, not owed, 2 rows", fake.marked, srv.portalMonitor().owed(), len(got))
	}
	for i, want := range []struct {
		action string
		id     uuid.UUID
	}{{"portal_issuers.activate", fake.rec.ActivateEventID}, {"portal_issuers.activate_override", fake.rec.OverrideEventID}} {
		ev := got[i]
		if ev.Action != want.action || ev.ID != want.id || !ev.Time.Equal(fake.rec.EventAt) || ev.Actor != "ops@example.com" || ev.ActorType != types.ActorHuman {
			t.Errorf("row %d = %s id %s time %s actor %s/%s; want %s id %s time %s actor human/ops@example.com",
				i, ev.Action, ev.ID, ev.Time, ev.ActorType, ev.Actor, want.action, want.id, fake.rec.EventAt)
		}
	}
	if !strings.Contains(string(got[1].Data), "billing-job@10.0.0.9") {
		t.Errorf("the override row's data %s does not name the overridden session", got[1].Data)
	}
	srv.deliverPortalActivation(context.Background(), fake)
	if n := len(rec.snapshot()); n != 2 {
		t.Errorf("a tick after delivery wrote %d more rows, want none", n-2)
	}

	plain := &activationStore{}
	psrv := New(baseTestConfig(h, plain))
	prec := &downAudit{}
	psrv.cfg.Audit = prec
	_, _ = plain.ActivatePortalIssuersAs(context.Background(), store.PortalActivation{ActorType: string(types.ActorHuman), Actor: "ops@example.com"})
	psrv.deliverPortalActivation(context.Background(), plain)
	if n := len(prec.snapshot()); n != 1 || !plain.marked {
		t.Errorf("an activation with no override owes %d rows (marked %v), want 1 and marked", n, plain.marked)
	}
}
