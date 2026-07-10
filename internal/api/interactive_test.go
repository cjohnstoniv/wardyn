// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/identity/embedded"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fakeRunner is a minimal runner.Runner that records whether Exec was called. It
// lets us assert the dispatch contract for interactive runs: CreateSandbox is
// invoked, the run goes RUNNING, but Exec is NEVER called (no agent task) and no
// completion watcher runs.
type fakeRunner struct {
	mu          sync.Mutex
	execCalls   int
	createCalls int
	lastSpec    runner.SandboxSpec
}

func (f *fakeRunner) Name() string { return "fake" }

func (f *fakeRunner) Capabilities(context.Context) (runner.Capabilities, error) {
	return runner.Capabilities{
		Driver:             "fake",
		ConfinementClasses: []types.ConfinementClass{types.CC1, types.CC2, types.CC3},
		StructuralEgress:   true,
	}, nil
}

func (f *fakeRunner) CreateSandbox(_ context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	f.lastSpec = spec
	return runner.Sandbox{Ref: "fake-" + spec.RunID.String(), Driver: "fake", EnforcedClass: spec.ConfinementClass}, nil
}

func (f *fakeRunner) Exec(context.Context, string, []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execCalls++
	return nil
}

// Wait blocks forever (until ctx is done): in a non-interactive run the watcher
// would call this, but for the interactive test it must never be reached. We
// honour ctx so a stray watcher (if the contract regressed) does not hang.
func (f *fakeRunner) Wait(ctx context.Context, _ string) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

func (f *fakeRunner) Attach(context.Context, string, runner.AttachOptions) (runner.Session, error) {
	return nil, context.Canceled
}
func (f *fakeRunner) Status(context.Context, string) (runner.Status, error) {
	return runner.Status{State: types.RunRunning}, nil
}
func (f *fakeRunner) StopSandbox(context.Context, string) error { return nil }
func (f *fakeRunner) KillSandbox(context.Context, string) error { return nil }

func (f *fakeRunner) execCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.execCalls
}

// pgHarnessWithRunner builds a pool-backed Server wired to the given runner.
// Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
func pgHarnessWithRunner(t *testing.T, r runner.Runner) (*Server, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed interactive test")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(pool.Close)

	audit := &recRecorder{}
	idp, err := embedded.New(nil, "wardyn.local", embedded.NewMemRevocationStore(), audit)
	if err != nil {
		t.Fatalf("embedded.New: %v", err)
	}
	srv := New(Config{
		Store:       store.NewPG(pool),
		Pool:        pool,
		Identity:    idp,
		Approvals:   newFakeApprovals(),
		Broker:      &fakeBroker{},
		Audit:       audit,
		Runner:      r,
		AdminToken:  adminToken,
		TrustDomain: "wardyn.local",
		DefaultPolicy: types.RunPolicySpec{
			AllowedDomains:      []string{"api.anthropic.com"},
			MinConfinementClass: types.CC2,
			// never-reap so an interactive run is not stopped on idle.
			AutoStopAfterSec: -1,
		},
		ControlPlaneURL: "http://wardynd:8080",
	})
	return srv, pool
}

// TestCreateRun_Interactive_SkipsExec is the PIECE 2 contract test: an
// interactive create dispatches the sandbox (CreateSandbox + RUNNING) but does
// NOT call Runner.Exec and leaves the run RUNNING (idle, awaiting attach).
func TestCreateRun_Interactive_SkipsExec(t *testing.T) {
	fr := &fakeRunner{}
	srv, _ := pgHarnessWithRunner(t, fr)

	body := `{"agent":"claude-code","repo":"acme/widgets","task":"ignored when interactive","interactive":true}`
	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create interactive run: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var run types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}

	if run.State != types.RunRunning {
		t.Errorf("interactive run state = %q, want RUNNING (idle, awaiting attach)", run.State)
	}
	if fr.createCalls != 1 {
		t.Errorf("CreateSandbox calls = %d, want 1", fr.createCalls)
	}
	if got := fr.execCount(); got != 0 {
		t.Errorf("Runner.Exec calls = %d, want 0 (interactive must NOT exec the agent task)", got)
	}
}

// TestCreateRun_NonInteractive_StillExecs guards the negative: a normal run with
// a task DOES exec the agent (so the interactive skip is conditional, not a
// blanket disable).
func TestCreateRun_NonInteractive_StillExecs(t *testing.T) {
	fr := &fakeRunner{}
	srv, _ := pgHarnessWithRunner(t, fr)

	body := `{"agent":"claude-code","repo":"acme/widgets","task":"do the thing"}`
	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create run: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if fr.createCalls != 1 {
		t.Errorf("CreateSandbox calls = %d, want 1", fr.createCalls)
	}
	if got := fr.execCount(); got != 1 {
		t.Errorf("Runner.Exec calls = %d, want 1 (non-interactive run must exec the task)", got)
	}
}

// compile-time assertion that fakeRunner satisfies the interface.
var _ runner.Runner = (*fakeRunner)(nil)
