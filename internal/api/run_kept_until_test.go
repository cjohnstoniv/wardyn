// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// #1320: GET /runs/{id} says until when an ended run's files are kept, so the
// Ended banner can name the day. Display only: nothing reads it back.

// keptUntilStore is the revive store with the audit read GET /runs/{id}'s
// ui_apps lookup makes (the fixture's embedded store has none).
type keptUntilStore struct{ *reviveStore }

func (keptUntilStore) QueryAuditEvents(context.Context, uuid.UUID, int) ([]types.AuditEvent, error) {
	return nil, nil
}

// newEndedFixtureGrace is newEndedFixture with the files grace set before the
// sweep ends the run, so the run.ended row and the run agree on one grace.
func newEndedFixtureGrace(t *testing.T, grace time.Duration) (*reviveFixture, *startingRunner) {
	t.Helper()
	f, _ := newOwnerFixture(t)
	f.srv.cfg.EndedRunGrace = grace
	end := f.now.Add(-time.Minute)
	f.st.run.EndsAt = &end
	f.st.run.LostAt, f.st.run.LostReason = nil, ""
	f.ls.lapsed = false
	sr := &startingRunner{reviveRunner: f.rr}
	f.srv.cfg.Runner = sr
	f.sweep(t)
	f.run = f.st.run
	f.srv.cfg.Store = keptUntilStore{f.rs}
	return f, sr
}

// getRunBody reads the run as its owner.
func (f *reviveFixture) getRunBody(t *testing.T) map[string]any {
	t.Helper()
	w := doSSO(t, f.srv, http.MethodGet, "/api/v1/runs/"+f.run.ID.String(), f.ownerCookie(t), "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET run = %d %s", w.Code, w.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestGetRun_KeptUntilMatchesTheEndedRow(t *testing.T) {
	f, _ := newEndedFixtureGrace(t, 48*time.Hour)
	ev := f.audit.eventsFor(f.run.ID, "run.ended")
	if len(ev) != 1 {
		t.Fatalf("run.ended events = %d, want 1", len(ev))
	}
	want, err := time.Parse(time.RFC3339Nano, leaseAuditData(t, ev[0])["kept_until"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if lostAt, _ := f.st.lost(); !want.Equal(lostAt.Add(48 * time.Hour)) {
		t.Fatalf("fixture: row kept_until %v is not lost_at + 48h", want)
	}
	body := f.getRunBody(t)
	raw, ok := body["kept_until"].(string)
	if !ok {
		t.Fatalf("kept_until absent from %v", body)
	}
	got, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || !got.Equal(want) {
		t.Errorf("kept_until = %q, want %v (the run.ended row's)", raw, want)
	}
	if _, leaked := body["EndedRunGrace"]; leaked {
		t.Error("the grace itself is on the wire")
	}
}

func TestGetRun_KeptUntilAbsentWhenNothingIsKept(t *testing.T) {
	setRun := func(f *reviveFixture, edit func(*types.AgentRun, *types.RunState)) {
		f.st.mu.Lock()
		defer f.st.mu.Unlock()
		edit(&f.st.run, &f.st.state)
	}
	t.Run("stopped by an end with no grace to keep it", func(t *testing.T) {
		f, _ := newEndedFixtureGrace(t, 0)
		if f.st.State() == types.RunRunning {
			t.Fatalf("fixture: a grace of 0 left the run RUNNING")
		}
		if _, ok := f.getRunBody(t)["kept_until"]; ok {
			t.Error("kept_until sent for a stopped run")
		}
	})
	t.Run("stopped", func(t *testing.T) {
		f, _ := newEndedFixtureGrace(t, 48*time.Hour)
		setRun(f, func(_ *types.AgentRun, st *types.RunState) { *st = types.RunStopped })
		if _, ok := f.getRunBody(t)["kept_until"]; ok {
			t.Error("kept_until sent for a STOPPED run")
		}
	})
	t.Run("killed", func(t *testing.T) {
		f, _ := newEndedFixtureGrace(t, 48*time.Hour)
		setRun(f, func(_ *types.AgentRun, st *types.RunState) { *st = types.RunKilled })
		if _, ok := f.getRunBody(t)["kept_until"]; ok {
			t.Error("kept_until sent for a KILLED run")
		}
	})
	t.Run("lost to a reboot", func(t *testing.T) {
		f, _ := newEndedFixtureGrace(t, 48*time.Hour)
		setRun(f, func(r *types.AgentRun, _ *types.RunState) { r.LostReason = types.LostReboot })
		if _, ok := f.getRunBody(t)["kept_until"]; ok {
			t.Error("kept_until sent for a run lost to a reboot")
		}
	})
	t.Run("grace turned to 0 under a kept run", func(t *testing.T) {
		f, _ := newEndedFixtureGrace(t, 48*time.Hour)
		f.srv.cfg.EndedRunGrace = 0
		if _, ok := f.getRunBody(t)["kept_until"]; ok {
			t.Error("kept_until sent under a grace of 0")
		}
	})
	t.Run("revived", func(t *testing.T) {
		f, _ := newEndedFixtureGrace(t, 48*time.Hour)
		if code, body := f.extendAs(t, true, f.now.Add(24*time.Hour)); code != http.StatusOK {
			t.Fatalf("extend = %d %s", code, body)
		}
		f.run = f.st.run
		if code, body := f.reviveAs(t, true); code != http.StatusOK {
			t.Fatalf("revive = %d %s", code, body)
		}
		if _, ok := f.getRunBody(t)["kept_until"]; ok {
			t.Error("kept_until sent for a revived run")
		}
	})
}

func TestGetRun_KeptUntilNotForAnotherMember(t *testing.T) {
	f, _ := newEndedFixtureGrace(t, 48*time.Hour)
	other := ssoSession(t, "sub-someone-else", "else@corp.example", oidc.RoleUser)
	w := doSSO(t, f.srv, http.MethodGet, "/api/v1/runs/"+f.run.ID.String(), other, "")
	if w.Code != http.StatusNotFound {
		t.Errorf("another member's GET = %d %s, want 404", w.Code, w.Body.String())
	}
}
