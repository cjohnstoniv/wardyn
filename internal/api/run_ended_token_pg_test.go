// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_EndedRunTokenRefusedAtMintAndInjection is #1176 against a real
// Postgres store: the lease ends a run and keeps it (still RUNNING, its
// identity not revoked), and the token its stopped proxy still holds is
// refused at the credential-mint and injection doors on the very next call,
// not an hour later when it lapses. The same token passes both doors' gate
// before the end, so the refusal is the end's.
func TestPG_EndedRunTokenRefusedAtMintAndInjection(t *testing.T) {
	pool := throwawayPGPool(t)
	st := store.NewPG(pool)
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.Approvals = h.approvals
	cfg.Broker = h.broker
	rn := &leaseRunner{finalizeTailRunner: &finalizeTailRunner{fakeRunner: &fakeRunner{}}}
	cfg.Runner = rn
	cfg.EndedRunGrace = 7 * 24 * time.Hour
	srv := New(cfg)
	ctx := context.Background()

	pastEnd := time.Now().Add(-time.Minute)
	run, err := st.CreateRun(ctx, types.AgentRun{
		ID: uuid.New(), CreatedAt: time.Now(), UpdatedAt: time.Now(),
		CreatedBy: "op@example.com", Agent: "claude-code", Task: "ended-token probe",
		ConfinementClass: types.CC2, State: types.RunRunning, RunnerTarget: "docker",
		Interactive: true, SandboxRef: "wardyn-agent-ended-token", EndsAt: &pastEnd,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	tok := h.mintRunToken(t, run.ID)
	doors := map[string]func() (int, string){
		"mint": func() (int, string) {
			w := do(t, srv, http.MethodPost, "/api/v1/internal/credentials/mint", tok, `{"grant_id":"`+uuid.NewString()+`"}`)
			return w.Code, w.Body.String()
		},
		"injection": func() (int, string) {
			w := do(t, srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), tok, "")
			return w.Code, w.Body.String()
		},
	}
	for name, door := range doors {
		if code, body := door(); code == http.StatusUnauthorized || code == http.StatusForbidden {
			t.Fatalf("%s before the end = %d %s, want it past the liveness gate", name, code, body)
		}
	}

	srv.leaseRun(ctx, st, run)
	got, err := st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != types.RunRunning || got.LostReason != types.LostEnded || rn.endCount() != 1 {
		t.Fatalf("state=%s lost_reason=%q ends=%d, want a RUNNING run ended and kept", got.State, got.LostReason, rn.endCount())
	}
	for name, door := range doors {
		if code, body := door(); code != http.StatusForbidden || !strings.Contains(body, "lost") {
			t.Errorf("%s after the end = %d %s, want 403 run is lost", name, code, body)
		}
	}
}
