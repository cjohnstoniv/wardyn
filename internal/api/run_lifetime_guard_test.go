// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run at its lifetime cap is over before the lease sweep kills it (up to a minute later): the doors
// the sandbox, the token and the deciders reach refuse it from the cap.

func TestPastLifetimeCap(t *testing.T) {
	now := time.Now().UTC()
	at := func(d time.Duration) *time.Time { c := now.Add(d); return &c }
	for _, tc := range []struct {
		name  string
		capAt *time.Time
		want  bool
	}{
		{"no cap", nil, false},
		{"cap in the future", at(time.Second), false},
		{"cap exactly now", at(0), true},
		{"cap one second behind", at(-time.Second), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pastLifetimeCap(types.AgentRun{LifetimeCapAt: tc.capAt}, now); got != tc.want {
				t.Errorf("pastLifetimeCap = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLifetimePassedEnd(t *testing.T) {
	now := time.Now().UTC()
	past, future := now.Add(-time.Second), now.Add(time.Hour)
	if end := lifetimePassedEnd(types.AgentRun{LifetimeCapAt: &future}, now); end != nil {
		t.Errorf("a cap ahead ended the start: %+v", end)
	}
	if end := lifetimePassedEnd(types.AgentRun{}, now); end != nil {
		t.Errorf("no cap ended the start: %+v", end)
	}
	end := lifetimePassedEnd(types.AgentRun{LifetimeCapAt: &past}, now)
	if end == nil || end.token != string(types.RunEndMaxLifetimeReached) || end.hint != types.RunEndMaxLifetimeReached.Sentence() {
		t.Errorf("a cap behind = %+v, want max_lifetime_reached with its sentence", end)
	}
}

// TestLifetimePassedEndElevationHint: a run capped at its elevation's end is told the elevation ended; a pool
// cap keeps the plain sentence.
func TestLifetimePassedEndElevationHint(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-time.Second)
	end := lifetimePassedEnd(types.AgentRun{LifetimeCapAt: &past, LifetimeCapSource: types.LimitSourceElevation}, now)
	if end == nil || end.token != string(types.RunEndMaxLifetimeReached) || end.hint != "the elevation it was launched under has ended" {
		t.Errorf("an elevation cap behind = %+v, want max_lifetime_reached naming the elevation", end)
	}
	end = lifetimePassedEnd(types.AgentRun{LifetimeCapAt: &past, LifetimeCapSource: types.LimitSourcePool}, now)
	if end == nil || end.hint != types.RunEndMaxLifetimeReached.Sentence() {
		t.Errorf("a pool cap behind = %+v, want the plain sentence", end)
	}
}

// lifetimeCapRunStore serves one RUNNING run with a lifetime cap.
type lifetimeCapRunStore struct {
	touchStore
	capAt time.Time
}

func (s *lifetimeCapRunStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	capAt := s.capAt
	return types.AgentRun{ID: id, State: types.RunRunning, UpdatedAt: time.Now().UTC(), LifetimeCapAt: &capAt}, nil
}

var lifetimeGracedDoors = map[string]bool{"recording upload": true, "recording part upload": true, "scan-result upload": true, "sso-token upload": true}

func cappedDoorServer(t *testing.T, capAt time.Time) (*harness, *Server, uuid.UUID, uuid.UUID, string) {
	t.Helper()
	h := newHarness(t)
	runID, grantID := uuid.New(), uuid.New()
	cfg := baseTestConfig(h, &lifetimeCapRunStore{capAt: capAt})
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.RecordingStore = &fakeRecordingStore{}
	cfg.Approvals = h.approvals
	cfg.Broker = h.broker
	return h, New(cfg), runID, grantID, h.mintRunToken(t, runID)
}

func TestInternalDoorRefusedPastLifetimeCap(t *testing.T) {
	// Well past the upload grace, so no door is let through.
	h, srv, runID, grantID, tok := cappedDoorServer(t, time.Now().UTC().Add(-terminalUploadGrace-time.Minute))
	refused := 0
	for _, d := range internalDoors(runID, grantID) {
		if d.name == "token renew" {
			continue // renew answers for itself: TestTokenRenewRefusedPastLifetimeCap
		}
		t.Run(d.name, func(t *testing.T) {
			w := do(t, srv, d.method, d.path, tok, d.body)
			if w.Code != http.StatusForbidden || errorReason(w) != "run_terminal" {
				t.Fatalf("code = %d reason = %q, want 403 run_terminal; body=%s", w.Code, errorReason(w), w.Body.String())
			}
			refused++
		})
	}
	var rows int
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "authz.denied" && strings.Contains(string(ev.Data), `"reason":"run_terminal"`) && strings.Contains(string(ev.Data), `"lifetime_cap_at"`) {
			rows++
		}
	}
	if rows != refused || rows == 0 {
		t.Errorf("authz.denied rows with lifetime_cap_at = %d, want one per refused door (%d)", rows, refused)
	}
}

func TestInternalDoorBeforeLifetimeCapUnchanged(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, &lifetimeCapRunStore{capAt: time.Now().UTC().Add(time.Hour)}))
	tok := h.mintRunToken(t, uuid.New())
	w := do(t, srv, http.MethodPost, "/api/v1/internal/decisions", tok,
		`{"request":{"host":"api.anthropic.com","method":"POST"},"decision":"allow"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("code = %d, want 202 for a run an hour short of its cap; body=%s", w.Code, w.Body.String())
	}
}

func TestInternalUploadGraceFromLifetimeCap(t *testing.T) {
	for name, age := range map[string]time.Duration{"inside the grace": time.Second, "after the grace": terminalUploadGrace + time.Minute} {
		t.Run(name, func(t *testing.T) {
			_, srv, runID, grantID, tok := cappedDoorServer(t, time.Now().UTC().Add(-age))
			for _, d := range internalDoors(runID, grantID) {
				if !lifetimeGracedDoors[d.name] {
					continue
				}
				w := do(t, srv, d.method, d.path, tok, d.body)
				gate := w.Code == http.StatusForbidden && errorReason(w) == "run_terminal"
				if gate == (age < terminalUploadGrace) {
					t.Errorf("%s %s: code = %d reason = %q; the gate's refusal should be %v", name, d.name, w.Code, errorReason(w), age >= terminalUploadGrace)
				}
			}
		})
	}
}

func TestTokenRenewRefusedPastLifetimeCap(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, &lifetimeCapRunStore{capAt: time.Now().UTC().Add(-time.Second)}))
	tok := h.mintRunToken(t, uuid.New())
	w := do(t, srv, http.MethodPost, "/api/v1/internal/token/renew", tok, "")
	if w.Code != http.StatusForbidden || errorReason(w) != "run_terminal" {
		t.Fatalf("code = %d reason = %q, want 403 run_terminal; body=%s", w.Code, errorReason(w), w.Body.String())
	}
	var denied bool
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "identity.renew" && ev.Outcome == "denied" && strings.Contains(string(ev.Data), `"reason":"run_past_lifetime"`) {
			denied = true
		}
	}
	if !denied {
		t.Error("no identity.renew denied row with reason run_past_lifetime")
	}
}

func TestDecideRefusedPastLifetimeCap(t *testing.T) {
	capRun := func(f *scopeFixture, capAt time.Time) {
		f.store.mu.Lock()
		r := f.store.runs[f.runID]
		r.LifetimeCapAt = &capAt
		f.store.runs[f.runID] = r
		f.store.mu.Unlock()
	}
	state := func(f *scopeFixture, id uuid.UUID) types.ApprovalState {
		f.approval.mu.Lock()
		defer f.approval.mu.Unlock()
		return f.approval.byID[id].State
	}
	for _, verb := range []string{"approve", "deny"} {
		t.Run(verb, func(t *testing.T) {
			f := newScopeFixture(t)
			capRun(f, time.Now().UTC().Add(-time.Second))
			id := f.seedEgress(t, "registry.npmjs.org")
			w := do(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/"+verb, adminToken, `{}`)
			if w.Code != http.StatusConflict || errorReason(w) != "approval_run_past_lifetime" {
				t.Fatalf("code = %d reason = %q, want 409 approval_run_past_lifetime; body=%s", w.Code, errorReason(w), w.Body.String())
			}
			if got := state(f, id); got != types.ApprovalPending {
				t.Errorf("approval state = %q after the refusal, want PENDING (the sweep that ends the run cancels it)", got)
			}
		})
	}
	t.Run("before the cap decides", func(t *testing.T) {
		f := newScopeFixture(t)
		capRun(f, time.Now().UTC().Add(time.Hour))
		id := f.seedEgress(t, "registry.npmjs.org")
		if w := do(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", adminToken, `{}`); w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
		}
	})
	t.Run("the admin token's spent bypass leaves a failure row", func(t *testing.T) {
		t.Setenv(envEgressSecondHuman, "1")
		f := newScopeFixture(t)
		capRun(f, time.Now().UTC().Add(-time.Second))
		id := f.seedEgress(t, "registry.npmjs.org")
		w := do(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", adminToken, `{}`)
		if w.Code != http.StatusConflict {
			t.Fatalf("code = %d, want 409; body=%s", w.Code, w.Body.String())
		}
		var row bool
		for _, ev := range f.audit() {
			if ev.Action == "approval.second_human.bypass" && ev.Outcome == "failure" && strings.Contains(string(ev.Data), `"error":"run_past_lifetime"`) {
				row = true
			}
		}
		if !row {
			t.Error("no approval.second_human.bypass failure row with class run_past_lifetime")
		}
	})
	t.Run("a member is refused like anyone", func(t *testing.T) {
		f := newScopeFixture(t)
		capRun(f, time.Now().UTC().Add(-time.Second))
		id := f.seedEgress(t, "registry.npmjs.org")
		owner := ssoSession(t, f.memberID, "member@corp.example", oidc.RoleAdmin)
		w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", owner, `{}`)
		if w.Code != http.StatusConflict || errorReason(w) != "approval_run_past_lifetime" {
			t.Fatalf("code = %d reason = %q, want 409 approval_run_past_lifetime; body=%s", w.Code, errorReason(w), w.Body.String())
		}
	})
}
