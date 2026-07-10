// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package apie2e is a black-box, end-to-end test of the Wardyn control plane's
// PUBLIC surface. Unlike internal/api's white-box tests (which reach into the
// Server struct and drive httptest.NewRecorder directly), every test here boots
// the REAL server over a real httptest.NewServer and drives it through the
// public Go SDK (pkg/client) pointed at the test server's URL. The only
// non-fakes that must be faked are the things that need real external systems:
//
//   - the sandbox runner (CreateSandbox/Exec/Wait) — a deterministic in-package
//     fakeRunner drives the run lifecycle so a run can reach COMPLETED;
//   - the at-rest secret store — an in-process memSecrets so api_key
//     injection-resolve works without a real KMS/age key;
//   - the GitHub installation-token minter — broker.FakeGitHubMinter so an
//     approval-gated github_token grant can actually mint.
//
// EVERYTHING ELSE is the real production code: the real broker
// (broker.New over broker.NewPgxStore), the real approval FSM service (the
// adapter copied verbatim from cmd/wardynd/adapters.go — that package is `main`
// and cannot be imported), the real embedded identity provider, the real chi
// router + middleware, and a live Postgres (db.Connect + db.Migrate).
//
// Substrate: live Postgres. Guarded by WARDYN_TEST_PG; skipped cleanly when unset
// and PASSES when set. Run:
//
//	WARDYN_TEST_PG="postgres://wardyn:wardyn@localhost:55432/wardyn_apie2e?sslmode=disable" \
//	  go test ./test/apie2e/...
package apie2e

import (
	"context"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/embedded"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// adminToken is the bearer token the harness configures on the real server and
// the SDK presents on every admin call.
const adminToken = "apie2e-admin-token"

// trustDomain matches the canonical pg harness in internal/api.
const trustDomain = "wardyn.local"

// internalAudience is the audience run tokens are minted/verified against. It
// must match internal/api's unexported constant ("wardyn-internal"); the harness
// mints its own run tokens for the run-token-gated internal endpoints so it
// re-declares the value here (kept in lockstep with internal/api/server.go).
const internalAudience = "wardyn-internal"

// ─── in-process secret store ──────────────────────────────────────────────────

// memSecrets is a minimal secretstore.Store backed by an in-memory map. It
// mirrors the fake in internal/api/injection_test.go so api_key injection-
// resolve works without a real at-rest secret backend (age/KMS). Values are
// write-only from the API's perspective; this map is the at-rest store the
// broker resolves against.
type memSecrets struct{ m map[string][]byte }

func newMemSecrets() *memSecrets { return &memSecrets{m: map[string][]byte{}} }

func (s *memSecrets) Name() string { return "mem" }
func (s *memSecrets) Put(_ context.Context, name string, v []byte) error {
	s.m[name] = v
	return nil
}
func (s *memSecrets) Get(_ context.Context, name string) ([]byte, error) {
	v, ok := s.m[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return v, nil
}
func (s *memSecrets) Delete(_ context.Context, name string) error { delete(s.m, name); return nil }
func (s *memSecrets) List(_ context.Context) ([]string, error) {
	out := make([]string, 0, len(s.m))
	for k := range s.m {
		out = append(out, k)
	}
	return out, nil
}

// ─── approval service adapter (copied from cmd/wardynd/adapters.go) ─────────────
//
// cmd/wardynd is package `main`, so its approvalService adapter cannot be
// imported. It is copied here verbatim (the ~20-line adapter the lane brief
// describes) so the e2e server runs the SAME real approval FSM the daemon runs:
// Request -> approval.RequestApproval, Decide -> approval.Decide, Get ->
// store.GetApproval, List -> store.ListApprovals over the live pool.

// approvalStore adapts the function-style internal/store API + the audit
// Recorder to the narrow approval.Store interface.
type approvalStore struct {
	pool *pgxpool.Pool
	rec  store.Recorder
}

func (a approvalStore) CreateApproval(ctx context.Context, ar types.ApprovalRequest) (types.ApprovalRequest, error) {
	return store.NewPG(a.pool).CreateApproval(ctx, ar)
}
func (a approvalStore) GetApproval(ctx context.Context, id uuid.UUID) (types.ApprovalRequest, error) {
	return store.NewPG(a.pool).GetApproval(ctx, id)
}
func (a approvalStore) ListApprovals(ctx context.Context, st types.ApprovalState) ([]types.ApprovalRequest, error) {
	return store.NewPG(a.pool).ListApprovals(ctx, st)
}
func (a approvalStore) DecideApproval(ctx context.Context, id uuid.UUID, st types.ApprovalState, decidedBy, reason string) (types.ApprovalRequest, error) {
	return store.NewPG(a.pool).DecideApproval(ctx, id, st, decidedBy, reason)
}
func (a approvalStore) Record(ctx context.Context, ev types.AuditEvent) error {
	return a.rec.Record(ctx, ev)
}

var _ approval.Store = approvalStore{}

// approvalService implements api.ApprovalService over the real approval FSM.
type approvalService struct {
	pool *pgxpool.Pool
	rec  store.Recorder
}

func (s *approvalService) st() approvalStore { return approvalStore{pool: s.pool, rec: s.rec} }

func (s *approvalService) Request(ctx context.Context, req types.ApprovalRequest) (types.ApprovalRequest, error) {
	return approval.RequestApproval(ctx, s.st(), req)
}
func (s *approvalService) Decide(ctx context.Context, id uuid.UUID, approve bool, decidedByType types.ActorType, decidedBy, reason string) (types.ApprovalRequest, error) {
	return approval.Decide(ctx, s.st(), id, approve, decidedByType, decidedBy, reason)
}
func (s *approvalService) Get(ctx context.Context, id uuid.UUID) (types.ApprovalRequest, error) {
	return store.NewPG(s.pool).GetApproval(ctx, id)
}
func (s *approvalService) List(ctx context.Context, state types.ApprovalState) ([]types.ApprovalRequest, error) {
	return store.NewPG(s.pool).ListApprovals(ctx, state)
}

// ─── fake runner ──────────────────────────────────────────────────────────────

// fakeRunner is an in-package runner.Runner that lets the e2e tests drive the
// run lifecycle deterministically. It mirrors the shape of the fake in
// internal/api/interactive_test.go but is tunable per test:
//
//   - CreateSandbox returns a deterministic ref and records the spec.
//   - Exec records the call (the agent process "started").
//   - Wait BLOCKS on a per-test gate until release is signalled, then returns
//     waitExit. A test releases it with releaseWait to make a run reach its
//     terminal state (COMPLETED on exit 0) so the completion watcher fires the
//     end-to-end revokeRunCascade.
//
// All methods are safe for concurrent use (the completion watcher runs Wait on
// a detached goroutine).
type fakeRunner struct {
	// waitExit is the exit code Wait returns once released (0 => COMPLETED). Set
	// before the runner is used; not mutated concurrently.
	waitExit int
	// gate releases a blocked Wait; closed by releaseWait. nil => Wait blocks on
	// ctx only (used where no completion is desired).
	gate chan struct{}

	// mu guards the call counters, which the detached completion-watcher
	// goroutine writes (StopSandbox) while the test reads (race-clean).
	mu          sync.Mutex
	createCalls int
	execCalls   int
	stopCalls   int
	killCalls   int
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{gate: make(chan struct{})}
}

// releaseWait unblocks the run's completion Wait exactly once so the completion
// watcher can advance the run to its terminal state.
func (f *fakeRunner) releaseWait() { close(f.gate) }

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
	f.createCalls++
	f.mu.Unlock()
	return runner.Sandbox{Ref: "fake-" + spec.RunID.String(), Driver: "fake", EnforcedClass: spec.ConfinementClass}, nil
}

func (f *fakeRunner) Exec(context.Context, string, []string) error {
	f.mu.Lock()
	f.execCalls++
	f.mu.Unlock()
	return nil
}

// Wait blocks until the per-run gate is released (then returns waitExit) or the
// detached context is cancelled. Honouring ctx prevents a hung goroutine at
// test teardown.
func (f *fakeRunner) Wait(ctx context.Context, _ string) (int, error) {
	if f.gate == nil {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	select {
	case <-f.gate:
		return f.waitExit, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (f *fakeRunner) Attach(context.Context, string, runner.AttachOptions) (runner.Session, error) {
	return nil, context.Canceled
}
func (f *fakeRunner) Status(context.Context, string) (runner.Status, error) {
	return runner.Status{State: types.RunRunning}, nil
}
func (f *fakeRunner) StopSandbox(context.Context, string) error {
	f.mu.Lock()
	f.stopCalls++
	f.mu.Unlock()
	return nil
}
func (f *fakeRunner) KillSandbox(context.Context, string) error {
	f.mu.Lock()
	f.killCalls++
	f.mu.Unlock()
	return nil
}

// stopCount returns the StopSandbox call count under the lock (race-clean read
// for the test, which races the detached completion watcher).
func (f *fakeRunner) stopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopCalls
}

var _ runner.Runner = (*fakeRunner)(nil)

// ─── harness ──────────────────────────────────────────────────────────────────

// harness is one fully-wired, black-box-testable control plane: the real server
// behind an httptest server, a live pool, the real broker + approval service,
// and the SDK client pointed at the server with the admin token.
type harness struct {
	t       *testing.T
	pool    *pgxpool.Pool
	srv     *httptest.Server
	api     *api.Server
	sdk     *client.Client
	broker  *broker.Broker
	idp     *embedded.Provider
	secrets *memSecrets
	gh      *broker.FakeGitHubMinter
	runner  *fakeRunner
}

// harnessOpts tunes the wiring per flow.
type harnessOpts struct {
	// withRunner wires the fakeRunner so runs are dispatched and the completion
	// watcher runs. nil => headless (runs stay PENDING, no watcher).
	withRunner *fakeRunner
	// defaultPolicy overrides the server's DefaultPolicy. Zero value uses a
	// sensible CC2 default with api.anthropic.com allowed.
	defaultPolicy *types.RunPolicySpec
	// recordingStore, when non-nil, wires the session-recording upload/serve
	// surfaces. nil => those routes are not mounted.
	recordingStore recording.Store
	// composer, when non-nil, wires the AI Run Composer registry so the
	// POST /runs/compose + GET /composer/backends endpoints are enabled. nil =>
	// the composer is disabled (those endpoints 404), preserving the existing
	// tests' behaviour. The hook is intentionally minimal and backward-compatible:
	// existing harnessOpts{...} construct the zero value, leaving cfg.Composer nil.
	composer *composer.Registry
}

// newHarness boots the real control plane end-to-end. Guarded by WARDYN_TEST_PG;
// skips cleanly when unset (the ONLY allowed skip — the substrate is genuinely
// absent). When set the server, broker, approval service and SDK are all real.
func newHarness(t *testing.T, opts harnessOpts) *harness {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping apie2e black-box test")
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

	// Real audit recorder: the pg store is the append-only system of record, so
	// control-plane audit events are queryable through GET /api/v1/audit.
	rec := store.Recorder{Pool: pool}

	idp, err := embedded.New(nil, trustDomain, embedded.NewMemRevocationStore(), rec)
	if err != nil {
		t.Fatalf("embedded.New: %v", err)
	}

	secrets := newMemSecrets()
	gh := &broker.FakeGitHubMinter{Token: "ghs_apie2e"}

	// REAL broker over a real pgx store, real secret store, real audit, real
	// identity, and a fake github minter (the only external dependency).
	brk := broker.New(broker.NewPgxStore(pool), secrets, rec, idp, gh)

	approvals := &approvalService{pool: pool, rec: rec}

	policy := types.RunPolicySpec{
		AllowedDomains:      []string{"api.anthropic.com"},
		MinConfinementClass: types.CC2,
	}
	if opts.defaultPolicy != nil {
		policy = *opts.defaultPolicy
	}

	cfg := api.Config{
		Store:           store.NewPG(pool),
		Identity:        idp,
		Approvals:       approvals,
		Broker:          brk,
		Audit:           rec,
		AdminToken:      adminToken,
		TrustDomain:     trustDomain,
		DefaultPolicy:   policy,
		Secrets:         secrets,
		ControlPlaneURL: "http://wardynd:8080",
		// BaseCtx must outlive a request so the detached completion watcher is
		// not cancelled the moment a create-run handler returns. Bind it to a
		// context cancelled at test cleanup.
		BaseCtx: newCleanupCtx(t),
	}
	if opts.withRunner != nil {
		cfg.Runner = opts.withRunner
	}
	if opts.recordingStore != nil {
		cfg.RecordingStore = opts.recordingStore
	}
	if opts.composer != nil {
		cfg.Composer = opts.composer
	}

	apiSrv := api.New(cfg)
	ts := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(ts.Close)

	sdk := client.New(ts.URL, adminToken)

	return &harness{
		t:       t,
		pool:    pool,
		srv:     ts,
		api:     apiSrv,
		sdk:     sdk,
		broker:  brk,
		idp:     idp,
		secrets: secrets,
		gh:      gh,
		runner:  opts.withRunner,
	}
}

// newCleanupCtx returns a context cancelled at test cleanup. The completion
// watcher derives its detached lifetime from this; cancelling it at teardown
// stops any in-flight Wait goroutine so the test does not leak one.
func newCleanupCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

// mintRunToken mints a real run identity token (internal audience) so the
// harness can drive the run-token-gated INTERNAL endpoints (decisions, mint,
// injection) directly over HTTP — the black-box analogue of a sidecar.
func (h *harness) mintRunToken(runID uuid.UUID) string {
	h.t.Helper()
	id, err := h.idp.MintRunIdentity(context.Background(), runID, "alice@example.com", "", internalAudience)
	if err != nil {
		h.t.Fatalf("mint run identity: %v", err)
	}
	return id.Token
}

// callerClaims builds the identity claims for a run, as the broker would see
// them after token verification. Used by the approvals flow test to call the
// REAL broker.MintForGrant in the approval-gated transaction (mint is only
// reachable with run-token claims, never the admin SDK surface).
func (h *harness) callerClaims(runID uuid.UUID) *identity.Claims {
	return &identity.Claims{
		RunID:    runID,
		SPIFFEID: "spiffe://" + trustDomain + "/agent-run/" + runID.String(),
	}
}

// waitFor polls fn until it returns true or the deadline elapses. Used to wait
// on the detached completion watcher (which advances run state asynchronously).
func waitFor(t *testing.T, d time.Duration, fn func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fn()
}
