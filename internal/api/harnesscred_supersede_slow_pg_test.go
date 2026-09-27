// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// slowSupersedeStore is the real PG store with the per-user roster served from
// memory and ONE seam: the first KILLED CAS — a supersede pass, which runs
// inside the per-person lock — parks until the test lets it go. That is the
// slow store the lock can be held across: up to supersedeCASAttempts ×
// killCascadeTimeout (90s) against a db.LoginSupersedeLockWait of 5s.
type slowSupersedeStore struct {
	store.PG
	site    types.SiteConfig
	kills   atomic.Int32
	parked  chan struct{} // closed when the supersede's CAS has parked
	release chan struct{} // closed by the test to let it write
}

func (s *slowSupersedeStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.site, nil
}

func (s *slowSupersedeStore) UpdateRunStateIf(ctx context.Context, id uuid.UUID, from, to types.RunState) (bool, error) {
	// A counter, not a sync.Once: a later Do would block behind the parked
	// one, and an unlocked sign-in (the defect) must not hang the test.
	if to == types.RunKilled && s.kills.Add(1) == 1 {
		close(s.parked)
		<-s.release
	}
	return s.PG.UpdateRunStateIf(ctx, id, from, to)
}

// TestPG_LoginSupersedeSlowStoreRefusesTheNextSignIn is #605's review nit:
// while one sign-in holds the person's lock across a slow supersede, the next
// sign-in by the same person does not queue behind it. It answers 503 with the
// fixed sentence after the 5s wait, and leaves no run row behind.
//
// RED with the refusal removed from lockLoginSupersede (a wait that expires
// proceeding unlocked): the third sign-in then answers 200 and inserts a run.
func TestPG_LoginSupersedeSlowStoreRefusesTheNextSignIn(t *testing.T) {
	pool := throwawayPGPool(t)
	st := &slowSupersedeStore{
		PG:      store.NewPG(pool),
		site:    agentRoster(perUserAWSRow()),
		parked:  make(chan struct{}),
		release: make(chan struct{}),
	}
	released := false
	letGo := func() {
		if !released {
			close(st.release)
			released = true
		}
	}
	t.Cleanup(letGo)

	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.Audit = &memAudit{}
	cfg.Approvals = h.approvals
	cfg.Broker = h.broker
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Runner = &fakeRunner{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.BedrockRegion = "us-east-1"
	cfg.DefaultPolicy = govDeployment()
	srv := New(cfg)
	sess := memberLoginSession(t)
	ctx := context.Background()

	// The first sign-in, up and RUNNING: the one the second will supersede.
	first := uuid.MustParse(launchLoginRun(t, srv, sess))
	deadline := time.Now().Add(10 * time.Second)
	for {
		run, err := st.GetRun(ctx, first)
		if err == nil && run.State == types.RunRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the first sign-in never reached RUNNING (%v, %v)", run.State, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The second takes the lock and parks inside its supersede pass.
	second := make(chan int, 1)
	go func() {
		second <- doSSO(t, srv, http.MethodPost, "/api/v1/setup/harness-login", sess, `{"provider":"aws"}`).Code
	}()
	select {
	case <-st.parked:
	case <-time.After(30 * time.Second):
		t.Fatal("the second sign-in never reached its supersede pass")
	}
	before, err := st.ListRuns(ctx)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}

	// The third meets the held lock: refused after the wait, not queued behind it.
	started := time.Now()
	w := doSSO(t, srv, http.MethodPost, "/api/v1/setup/harness-login", sess, `{"provider":"aws"}`)
	took := time.Since(started)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), signInBusyRefusal) {
		t.Fatalf("third sign-in = %d %s, want 503 carrying %q", w.Code, w.Body.String(), signInBusyRefusal)
	}
	if took < db.LoginSupersedeLockWait/2 || took > 3*db.LoginSupersedeLockWait {
		t.Errorf("third sign-in answered after %s, want about the %s lock wait", took, db.LoginSupersedeLockWait)
	}
	after, err := st.ListRuns(ctx)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("runs = %d, want %d — the refused sign-in left a run row", len(after), len(before))
	}

	letGo()
	select {
	case code := <-second:
		if code != http.StatusOK {
			t.Errorf("the second sign-in = %d, want 200 once its supersede finished", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the second sign-in never answered after its supersede was released")
	}
}
