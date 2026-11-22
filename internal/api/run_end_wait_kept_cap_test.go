// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run its own end kept before its cap is left to its files grace after the cap passes. The refusal to move its
// end says so and names the retention deadline; a run the sweep is ending still reads "being ended".
func TestEndWaitRunRefusal_KeptPastTheCapNamesTheRetentionDeadline(t *testing.T) {
	grace := 7 * 24 * time.Hour
	s := &Server{cfg: Config{EndedRunGrace: grace}}
	now := time.Now().UTC().Truncate(time.Second)
	capAt := now.Add(-time.Hour)
	lostAt := now.Add(-3 * time.Hour)
	kept := types.AgentRun{State: types.RunRunning, LifetimeCapAt: &capAt, LostReason: types.LostEnded, LostAt: &lostAt}
	live := types.AgentRun{State: types.RunRunning, LifetimeCapAt: &capAt}

	w := httptest.NewRecorder()
	if !s.endWaitRunRefusal(w, kept, now) || w.Code != http.StatusConflict {
		t.Fatalf("a kept run past its cap: refused=%d, want 409", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{reasonRunEndWaitMaxLifetime, "kept for its files", "retention deadline", lostAt.Add(grace).Format(time.RFC3339), "cannot be revived"} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal %s lacks %q", body, want)
		}
	}
	if strings.Contains(body, "being ended") {
		t.Errorf("refusal %s says the kept run is being ended", body)
	}

	w = httptest.NewRecorder()
	if !s.endWaitRunRefusal(w, live, now) || !strings.Contains(w.Body.String(), "being ended") {
		t.Errorf("a live run past its cap: %d %s, want the being-ended refusal", w.Code, w.Body.String())
	}
}
