// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const admissionOrgURL = "https://org.example"

// admissionFixture is a Server on fresh PostgreSQL whose production Hub admits
// a real keyed, claimed runner owning one RUNNING local run with an issued
// execution generation.
type admissionFixture struct {
	ctx  context.Context
	pool *pgxpool.Pool
	st   store.PG
	srv  *Server
	rn   types.Runner
	key  ed25519.PrivateKey
	run  types.AgentRun
	gen  uuid.UUID
}

func newAdmissionFixture(t *testing.T) *admissionFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	pool := throwawayPGPool(t)
	st := store.NewPG(pool)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	id := uuid.New()
	rn, err := st.CreateRunner(ctx, types.Runner{ID: id, Owner: "admission-owner", Name: "laptop", PublicKey: pub, KeyFingerprint: id.String(),
		Version: "0.9.0", OrgURLSHA256: federation.OrgURLSHA256(admissionOrgURL), Limits: roomyRunnerLimits()})
	if err != nil {
		t.Fatal(err)
	}
	if rn, err = st.ClaimRunner(ctx, rn.ID, rn.Owner, rn.KeyFingerprint, time.Now()); err != nil {
		t.Fatal(err)
	}
	run := newFinalizeRun()
	run.CreatedBy, run.Placement, run.RunnerID = rn.Owner, types.PlacementLocal, &rn.ID
	if run, err = st.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	run.SandboxRef = "runner:" + rn.ID.String() + "/sandbox"
	if err := st.SetSandboxRef(ctx, run.ID, run.SandboxRef); err != nil {
		t.Fatal(err)
	}
	gen := uuid.New()
	if _, err := st.BeginRunnerExecution(ctx, rn.ID, run.ID, uuid.Nil, gen, runnerwire.MethodExec); err != nil {
		t.Fatal(err)
	}
	cfg := baseTestConfig(newHarness(t), st)
	cfg.BaseCtx = ctx
	cfg.Runner = &fakeRunner{}
	cfg.Audit = &syncAudit{}
	cfg.RunnerOrgURL = admissionOrgURL
	cfg.RunnerAuditPrepare = func(_ context.Context, ev types.AuditEvent) (types.AuditEvent, error) { return ev, nil }
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = getErrStore{getErr: secretstore.ErrNotFound}
	return &admissionFixture{ctx: ctx, pool: pool, st: st, srv: New(cfg), rn: rn, key: key, run: run, gen: gen}
}

func (f *admissionFixture) liveRow() runnerwire.RunState {
	return runnerwire.RunState{RunID: f.run.ID, SandboxPresent: true,
		Execution: &runnerwire.ExecutionResult{Generation: f.gen, Operation: runnerwire.MethodExec, Attempted: true}}
}

// session drives one fresh connection of a real runnerwire.Client into the
// production Hub over loopback. It applies every PENDING action as a success,
// calls during after the PENDING results, then sends rows as ordered pages.
type session struct {
	done    chan struct{}
	err     error
	cancel  context.CancelFunc
	mu      sync.Mutex
	applied []uuid.UUID
	peer    *runnerwire.Peer // the runner's side of the session, once online
}

func (f *admissionFixture) connect(t *testing.T, during func(), rows ...runnerwire.RunState) *session {
	t.Helper()
	ctx, cancel := context.WithCancel(f.ctx)
	s := &session{done: make(chan struct{}), cancel: cancel}
	t.Cleanup(cancel)
	hub := f.srv.runnerHub(f.st)
	hooks := runnerwire.RunnerHooks{
		Call: func(context.Context, *runnerwire.Peer, uint32, string, json.RawMessage) (any, error) {
			return nil, errors.New("no calls in this fixture")
		},
		Open:  func(*runnerwire.Peer, *runnerwire.Stream, runnerwire.Open) error { return runnerwire.ErrNotReady },
		Clock: func(runnerwire.Hello, time.Time) error { return nil },
		Pending: func(ctx context.Context, peer *runnerwire.Peer, actions []runnerwire.PendingAction) error {
			for _, a := range actions {
				data, err := runnerwire.Marshal(runnerwire.ActionResult{ID: a.ID, Outcome: types.RunnerActionSuccess, ObservedAt: time.Now().UTC()})
				if err != nil {
					return err
				}
				runID := a.RunID
				if err := peer.EventAck(ctx, runnerwire.Event{Kind: runnerwire.EventActionResult, RunID: &runID, Data: data}); err != nil {
					return err
				}
				s.mu.Lock()
				s.applied = append(s.applied, a.ID)
				s.mu.Unlock()
			}
			if during != nil {
				during()
			}
			return nil
		},
		StatePages: func(_ context.Context, send func(runnerwire.StatePage) error) error {
			runs := slices.Clone(rows)
			slices.SortFunc(runs, func(a, b runnerwire.RunState) int { return bytes.Compare(a.RunID[:], b.RunID[:]) })
			id, after := uuid.New(), uuid.Nil
			for {
				n := min(len(runs), runnerwire.StatePageRows)
				if err := send(runnerwire.StatePage{InventoryID: id, After: after, Runs: runs[:n], Final: n == len(runs)}); err != nil {
					return err
				}
				if n == len(runs) {
					return nil
				}
				after, runs = runs[n-1].RunID, runs[n:]
			}
		},
		Replay:      func(context.Context, *runnerwire.Peer) error { return nil },
		BeforeReady: func(context.Context, *runnerwire.Peer, bool) error { return nil },
		Lease:       func(context.Context, runnerwire.Lease) error { return nil },
		Revoked:     func(context.Context) error { return nil },
		Offline:     func(context.Context) {},
		Online: func(p *runnerwire.Peer, _ bool) {
			s.mu.Lock()
			s.peer = p
			s.mu.Unlock()
		},
	}
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: admissionOrgURL, RunnerID: f.rn.ID.String(), PrivateKey: f.key, Hooks: hooks,
		Dial: func(context.Context) (runnerwire.Conn, error) {
			org, run := runnerwire.Loopback()
			go func() { _ = hub.Serve(ctx, org, f.rn.ID) }()
			return run, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		s.err = client.Connect(ctx)
		close(s.done)
	}()
	return s
}

// result is the connection's end, or nil while it is still open.
func (s *session) result() error {
	select {
	case <-s.done:
		return s.err
	default:
		return nil
	}
}

// waitReady reports whether the runner's production Link admitted dispatch.
func (f *admissionFixture) waitReady(s *session) bool {
	link := f.srv.runnerEndpoint(f.rn.ID).link
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if link.Online() {
			return true
		}
		select {
		case <-s.done:
			return link.Online()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return false
}

func (f *admissionFixture) inventory(t *testing.T) (complete, needs bool) {
	t.Helper()
	if err := f.pool.QueryRow(f.ctx, `SELECT complete, needs_reconciliation FROM runner_inventories WHERE runner_id=$1`, f.rn.ID).Scan(&complete, &needs); err != nil {
		t.Fatalf("no durable inventory: %v", err)
	}
	return complete, needs
}

func TestRunnerAdmissionPG_OfflineKillQueuesAndAFreshSessionAppliesItBeforeReady(t *testing.T) {
	f := newAdmissionFixture(t)
	sub := f.srv.runnerEndpoint(f.rn.ID).sub
	if err := sub.KillSandbox(f.ctx, f.run.SandboxRef); !errors.Is(err, runner.ErrPendingOnRunner) {
		t.Fatalf("offline kill = %v, want queued (ErrPendingOnRunner)", err)
	}
	page, _, err := f.st.ListPendingRunnerActions(f.ctx, f.rn.ID)
	if err != nil || len(page) != 1 || page[0].Kind != types.RunnerActionKill || page[0].Generation != f.gen || page[0].Owner != f.rn.Owner {
		t.Fatalf("queued kill = %+v, %v", page, err)
	}
	s := f.connect(t, nil, f.liveRow())
	if !f.waitReady(s) {
		t.Fatalf("fresh session never reached READY: %v", s.result())
	}
	s.mu.Lock()
	applied := s.applied
	s.mu.Unlock()
	if len(applied) != 1 || applied[0] != page[0].ID {
		t.Fatalf("runner applied %v, want the queued kill", applied)
	}
	var outcome string
	if err := f.pool.QueryRow(f.ctx, `SELECT outcome FROM runner_pending_actions WHERE id=$1 AND applied_at IS NOT NULL`, page[0].ID).Scan(&outcome); err != nil || outcome != types.RunnerActionSuccess {
		t.Fatalf("kill receipt = %q, %v", outcome, err)
	}
	if complete, needs := f.inventory(t); !complete || needs {
		t.Fatalf("inventory complete=%v needs_reconciliation=%v", complete, needs)
	}
}

func TestRunnerAdmissionPG_ActionQueuedDuringTheSessionWithholdsReady(t *testing.T) {
	f := newAdmissionFixture(t)
	sub := f.srv.runnerEndpoint(f.rn.ID).sub
	queued := func() {
		if err := sub.KillSandbox(f.ctx, f.run.SandboxRef); !errors.Is(err, runner.ErrPendingOnRunner) {
			t.Errorf("kill during admission = %v, want queued", err)
		}
	}
	first := f.connect(t, queued, f.liveRow())
	if f.waitReady(first) {
		t.Fatal("READY admitted while an action queued after PENDING was still open")
	}
	if complete, _ := f.inventory(t); !complete {
		t.Fatal("the refused session's Final page was not committed")
	}
	second := f.connect(t, nil, f.liveRow())
	if !f.waitReady(second) {
		t.Fatalf("the next fresh session never reached READY: %v", second.result())
	}
	second.mu.Lock()
	defer second.mu.Unlock()
	if len(second.applied) != 1 {
		t.Fatalf("next session applied %d actions, want the kill", len(second.applied))
	}
}

func TestRunnerAdmissionPG_OmittedLiveRunWithholdsReady(t *testing.T) {
	f := newAdmissionFixture(t)
	s := f.connect(t, nil)
	if f.waitReady(s) {
		t.Fatal("READY admitted with a live candidate missing from the Final page")
	}
	if complete, needs := f.inventory(t); !complete || !needs {
		t.Fatalf("inventory complete=%v needs_reconciliation=%v, want a complete inventory needing reconciliation", complete, needs)
	}
}

func TestRunnerAdmissionPG_ImmutableColumnsRefuseUpdate(t *testing.T) {
	f := newAdmissionFixture(t)
	if err := f.srv.runnerEndpoint(f.rn.ID).sub.KillSandbox(f.ctx, f.run.SandboxRef); !errors.Is(err, runner.ErrPendingOnRunner) {
		t.Fatal(err)
	}
	for name, q := range map[string]string{
		"ref":        `UPDATE runner_pending_actions SET ref = ref || 'x' WHERE runner_id=$1`,
		"owner":      `UPDATE runner_pending_actions SET owner = 'someone-else' WHERE runner_id=$1`,
		"generation": `UPDATE runner_pending_actions SET execution_generation = gen_random_uuid() WHERE runner_id=$1`,
		"kind":       `UPDATE runner_pending_actions SET kind = 'end' WHERE runner_id=$1`,
		"queued_at":  `UPDATE runner_pending_actions SET queued_at = queued_at - interval '1 hour' WHERE runner_id=$1`,
	} {
		if _, err := f.pool.Exec(f.ctx, q, f.rn.ID); err == nil {
			t.Errorf("%s: an UPDATE of an immutable column succeeded", name)
		}
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE runner_pending_actions SET applied_at=now(), outcome='success', observed_at=now() WHERE runner_id=$1`, f.rn.ID); err != nil {
		t.Fatalf("the first receipt write was refused: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE runner_pending_actions SET outcome='deferred' WHERE runner_id=$1`, f.rn.ID); err == nil {
		t.Error("a written receipt was rewritten")
	}
}

type registryRunner struct {
	*fakeRunner
	mu      sync.Mutex
	added   []string
	removed []string
}

func (r *registryRunner) Add(s substrate.Substrate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.added = append(r.added, s.Name())
}

func (r *registryRunner) Remove(s substrate.Substrate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removed = append(r.removed, s.Name())
}

func TestRunnerAdmissionPG_BootRegistersClaimedRunnersOnly(t *testing.T) {
	f := newAdmissionFixture(t)
	unclaimed, err := f.st.CreateRunner(f.ctx, types.Runner{ID: uuid.New(), Owner: f.rn.Owner, Name: "new", PublicKey: make([]byte, 32), KeyFingerprint: uuid.NewString(), Limits: roomyRunnerLimits()})
	if err != nil {
		t.Fatal(err)
	}
	reg := &registryRunner{fakeRunner: &fakeRunner{}}
	f.srv.cfg.Runner = reg
	if err := f.srv.RegisterRunnerSubstrates(f.ctx); err != nil {
		t.Fatal(err)
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	want := placement.RunnerSubstrateName(f.rn.ID.String())
	if len(reg.added) != 1 || reg.added[0] != want {
		t.Fatalf("registered %v, want only %s (not unclaimed %s)", reg.added, want, unclaimed.ID)
	}
}
