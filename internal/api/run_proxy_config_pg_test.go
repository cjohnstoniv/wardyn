// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_RunProxyConfigIsDroppedWhenTheRunGoesTerminal (#1176) against a real
// Postgres: the stored config is sealed (the row holds neither the token, the
// CA key nor the upstream credential), and it is deleted on every way a run
// goes terminal — the shared terminal tail, the kill, and the purge that backs
// up a delete that failed — while a live run keeps its own.
func TestPG_RunProxyConfigIsDroppedWhenTheRunGoesTerminal(t *testing.T) {
	st := store.NewPG(throwawayPGPool(t))
	h := newHarness(t)
	h.srv.cfg.Store = st
	h.srv.cfg.RunConfigKey = []byte("0123456789abcdef0123456789abcdef")
	ctx := context.Background()
	newRun := func() types.AgentRun {
		r, err := st.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedAt: time.Now(), UpdatedAt: time.Now(),
			CreatedBy: "op@example.com", Agent: "claude-code", Task: "t", ConfinementClass: types.CC1,
			State: types.RunRunning, RunnerTarget: "docker"})
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		if err := h.srv.keepRunProxyConfig(ctx, r.ID, runner.ProxyConfig{
			RunToken: "tok-" + r.ID.String(), ControlPlaneURL: "https://wardynd:8443",
			MITMCAKeyPEM: "KEY-" + r.ID.String(), UpstreamProxyURL: "http://u:PASS-" + r.ID.String() + "@proxy:3128",
		}); err != nil {
			t.Fatalf("keepRunProxyConfig: %v", err)
		}
		return r
	}
	kept := func(id uuid.UUID) bool {
		_, err := st.GetRunProxyConfig(ctx, id)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetRunProxyConfig: %v", err)
		}
		return err == nil
	}

	live, finished, killed, stranded := newRun(), newRun(), newRun(), newRun()
	sealed, err := st.GetRunProxyConfig(ctx, live.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"tok-", "KEY-", "PASS-"} {
		if strings.Contains(string(sealed), secret) {
			t.Errorf("the stored row holds %q in the clear", secret)
		}
	}

	if ok, err := st.UpdateRunStateIf(ctx, finished.ID, types.RunRunning, types.RunCompleted); err != nil || !ok {
		t.Fatalf("UpdateRunStateIf: %v %v", ok, err)
	}
	h.srv.finalizeRunTail(ctx, finished.ID, "", "run.complete", "success", map[string]any{})
	if kept(finished.ID) {
		t.Error("a completed run's proxy config survived its terminal tail")
	}

	h.srv.killTeardownTail(ctx, killed, types.ActorSystem, "test", nil)
	if kept(killed.ID) {
		t.Error("a killed run's proxy config survived the kill")
	}

	if ok, err := st.UpdateRunStateIf(ctx, stranded.ID, types.RunRunning, types.RunFailed); err != nil || !ok {
		t.Fatalf("UpdateRunStateIf: %v %v", ok, err)
	}
	if err := h.srv.purgeTerminalRunProxyConfigs(ctx); err != nil {
		t.Fatalf("purgeTerminalRunProxyConfigs: %v", err)
	}
	if kept(stranded.ID) {
		t.Error("a terminal run whose delete never ran kept its proxy config past the purge")
	}
	if !kept(live.ID) {
		t.Error("a live run lost its proxy config")
	}
}
