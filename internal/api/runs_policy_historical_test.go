// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestReviewRev08HistoricalUIAppEnvelope is the independent 0.8 release
// review's reproduction of F07 (#1062), adopted verbatim from
// /tmp/claude-1000/rev08/evidence/compatibility/review_rev08_compatibility_test.go:
// a run whose policy envelope was persisted under the pre-0.8 action name lost
// its UI apps (403) while the identical envelope under the new name resolved
// fine. RED on main before canonicalAction (audit_legacy.go); GREEN with it.
// The evidence file's sibling N-1-proxy test belongs to #1063 and is not
// adopted here.
func TestReviewRev08HistoricalUIAppEnvelope(t *testing.T) {
	for _, action := range []string{"run.policy.resolve", "run.policy.effective"} {
		t.Run(action, func(t *testing.T) {
			h := newUIHarness(t, okBackend())
			h.store.mu.Lock()
			for i := range h.store.events {
				h.store.events[i].Action = action
			}
			h.store.mu.Unlock()
			w := h.enter(url.Values{
				"run": {h.run.ID.String()}, "app": {"code"},
				"ticket": {h.ticket(h.run.ID, h.owner, oidc.RoleUser)},
			})
			if w.Code != http.StatusFound {
				t.Fatalf("persisted policy action %q: gateway status=%d body=%s, want 302", action, w.Code, w.Body.String())
			}
		})
	}
}

// historicalEnvelope is one run.policy.* audit row this file seeds directly
// (bypassing uiMemStore.putEffectivePolicy, which always writes the 0.8 name)
// so a test can control both the action spelling and which apps it declares.
type historicalEnvelope struct {
	action string
	apps   []types.UIApp
}

func seedEnvelopes(h *uiHarness, envelopes []historicalEnvelope) {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	h.store.events = nil // newUIHarness seeds its own default row; this test owns the trail
	for _, e := range envelopes {
		data, _ := json.Marshal(types.RunPolicySpec{UIApps: e.apps})
		h.store.events = append(h.store.events, types.AuditEvent{
			ID: uuid.New(), Time: time.Now(), RunID: &h.run.ID,
			Action: e.action, Outcome: "success", Data: data,
		})
	}
}

// TestUIAppsHistoricalActionNames is C-01's table test: effectiveUIApps must
// recognise both the 0.7 and 0.8 spellings of the policy envelope action, and
// "last wins" (runs_policy.go's chronological fold over QueryAuditEvents)
// must hold regardless of which spelling wrote the winning row. Every case
// declares "code" as (part of) the WINNING envelope, so every case gets the
// gateway's normal 302.
func TestUIAppsHistoricalActionNames(t *testing.T) {
	appCode := []types.UIApp{{Name: "code", Port: uiTestPort, Path: "/ide"}}
	appOther := []types.UIApp{{Name: "other", Port: uiTestPort, Path: "/other"}}

	for _, tc := range []struct {
		name      string
		envelopes []historicalEnvelope
	}{
		{"old-only", []historicalEnvelope{{"run.policy.effective", appCode}}},
		{"new-only", []historicalEnvelope{{"run.policy.resolve", appCode}}},
		{"mixed-old-then-new-later-new-wins", []historicalEnvelope{
			{"run.policy.effective", appOther},
			{"run.policy.resolve", appCode},
		}},
		{"mixed-new-then-old-later-old-wins", []historicalEnvelope{
			{"run.policy.resolve", appOther},
			{"run.policy.effective", appCode},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newUIHarness(t, okBackend())
			seedEnvelopes(h, tc.envelopes)

			w := h.enter(url.Values{
				"run": {h.run.ID.String()}, "app": {"code"},
				"ticket": {h.ticket(h.run.ID, h.owner, oidc.RoleUser)},
			})
			if w.Code != http.StatusFound {
				t.Fatalf("%s: gateway status=%d body=%s, want 302 (the winning envelope declares app \"code\")",
					tc.name, w.Code, w.Body.String())
			}
		})
	}
}

// TestEffectiveUIApps_NoEnvelopeStays403 pins the fail-closed behaviour
// canonicalAction must never widen: a run with no policy envelope at all
// (never dispatched, or the audit store unavailable) yields no apps, so the
// gateway keeps refusing it. C-01's DO NOT TOUCH list names this explicitly.
func TestEffectiveUIApps_NoEnvelopeStays403(t *testing.T) {
	h := newUIHarness(t, okBackend())
	seedEnvelopes(h, nil)

	w := h.enter(url.Values{
		"run": {h.run.ID.String()}, "app": {"code"},
		"ticket": {h.ticket(h.run.ID, h.owner, oidc.RoleUser)},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("no-envelope run: gateway status=%d body=%s, want 403", w.Code, w.Body.String())
	}
}

// TestLoginRunStamp_HistoricalActionName is C-01's sibling reader (found
// alongside F07, not in the reviewer's report): a login run launched before
// 0.8 wrote its launch stamp under harness.login.started (v0.7.12's
// launchHarnessLoginRun, f031df9a7). loginRunStamp must still find
// it under the 0.8 name it reads for, harness.login.start — otherwise the
// stamp reads as empty and ssotoken.go refuses or unpins the upload.
func TestLoginRunStamp_HistoricalActionName(t *testing.T) {
	store := newUIMemStore()
	runID := uuid.New()
	want := loginRunStamp{SSOStartURL: "https://org.awsapps.com/start", CredentialSource: "shared", Owner: "alice"}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	store.events = append(store.events, types.AuditEvent{
		ID: uuid.New(), Time: time.Now(), RunID: &runID,
		Action: "harness.login.started", Outcome: "success", Data: data,
	})

	srv := New(Config{Store: store})
	got, err := srv.loginRunStamp(context.Background(), runID)
	if err != nil {
		t.Fatalf("loginRunStamp: %v", err)
	}
	if got != want {
		t.Fatalf("loginRunStamp = %+v, want %+v (the pre-0.8 action name harness.login.started must still resolve)", got, want)
	}
}
