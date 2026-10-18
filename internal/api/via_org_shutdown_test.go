// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

// A stopping daemon waits for a run's via_org engine to close: the run's watcher and the close it starts
// are background work, so WaitBackground returns only once the engine has flushed and shut. The close
// waits for the stream still admitted to it, which ends after WaitBackground has begun waiting.
func TestViaOrg_ShutdownWaitsForTheEngineClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{cfg: Config{BaseCtx: ctx}, bgWaitBudget: 10 * time.Second}
	run, _ := s.viaOrgRunFor(uuid.New(), uuid.New())
	eng, err := proxy.NewViaOrgEngine(context.Background(),
		&proxy.Config{RunID: run.id, ControlPlaneURL: "http://127.0.0.1:9", RunToken: "run-token", DecisionBufferSize: 16},
		proxy.ViaOrgEngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	gen := &viaOrgGen{engine: eng}
	gen.active.Add(1)
	run.mu.Lock()
	run.engine = gen
	run.mu.Unlock()
	go func() {
		time.Sleep(100 * time.Millisecond)
		gen.active.Done()
	}()
	cancel()
	s.WaitBackground()
	if !eng.Closed() {
		t.Fatal("WaitBackground returned before the run's via_org engine was closed")
	}
}
