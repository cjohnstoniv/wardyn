// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Erasing a person ends their live session with an end row that names no one, leaves an already-ended one with
// no second row, and their session answers 404 to them afterwards.
func TestAssistantErasureEndsTheSessionAndNamesNoOne(t *testing.T) {
	site, bind := assistantTwoProviders()
	f := newAssistantFake(site)
	srv, h := assistantSessionServer(t, f)
	person := "sub-erase-" + uuid.NewString()
	live := assistantLiveSession(person, nil, bind)
	ended := assistantLiveSession(person, nil, bind)
	ended.Purpose, ended.State, ended.EndReason = types.AssistantReview, types.AssistantSessionEnded, types.AssistantEndPerson
	f.seed(live)
	f.seed(ended)
	n, err := srv.eraseAssistantSessions(context.Background(), f, []string{person})
	if err != nil || n != 2 {
		t.Fatalf("erase = %d, %v; want 2 sessions", n, err)
	}
	rows := govCovAudits(h, assistantSessionEndAction)
	if len(rows) != 1 || rows[0].Target != live.ID.String() {
		t.Fatalf("end rows = %+v, want one for the live session", rows)
	}
	var data map[string]any
	if err := json.Unmarshal(rows[0].Data, &data); err != nil || data["end_reason"] != types.AssistantEndErased {
		t.Errorf("end row data = %s, want end_reason erased", rows[0].Data)
	}
	if rows[0].ActorType != types.ActorSystem || rows[0].Actor != "wardynd" || string(rows[0].Data) == "" ||
		containsAny(string(rows[0].Data)+rows[0].Actor, person) {
		t.Errorf("end row = %s %s %s, want the system actor and no person", rows[0].ActorType, rows[0].Actor, rows[0].Data)
	}
	if got := f.session(live.ID); got.Live() || got.EndReason != types.AssistantEndErased {
		t.Errorf("live session = %s/%s, want ended as erased", got.State, got.EndReason)
	}
	cookie := ssoSession(t, person, person+"@corp.example", oidc.RoleUser)
	wantReason(t, doSSO(t, srv, http.MethodPost, assistantPath(live.ID, "/messages"), cookie, `{"text":"hi"}`), http.StatusNotFound, reasonAssistantSessionNotFound)
}

// The sweep takes ended sessions older than the run-output window, and none when the window is zero.
func TestAssistantSweepWindow(t *testing.T) {
	f := newAssistantFake(types.SiteConfig{})
	srv, _ := assistantSessionServer(t, f)
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	srv.cfg.Now = func() time.Time { return now }
	if err := srv.sweepAssistantSessions(context.Background()); err != nil || f.sweptBefore != nil {
		t.Fatalf("a zero window swept before %v (%v)", f.sweptBefore, err)
	}
	srv.cfg.RunOutputRetention = 72 * time.Hour
	if err := srv.sweepAssistantSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.sweptBefore == nil || !f.sweptBefore.Equal(now.Add(-72*time.Hour)) {
		t.Errorf("swept before %v, want %v", f.sweptBefore, now.Add(-72*time.Hour))
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && len(s) >= len(sub) && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
