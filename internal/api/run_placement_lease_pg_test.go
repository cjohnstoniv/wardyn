// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The lease and execution generation of a run on a runner (design H3 slice 2 §9) against the REAL runner
// side: a runnerwire client serving runneragent's dispatcher over offline.Manager, connected over loopback
// to this Server's production Hub, on a fresh Postgres. Only the sandbox substrate under the runner is a
// fake. Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/offline"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runner/runneragent"
	"github.com/cjohnstoniv/wardyn/internal/runner/runnerio"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type runnerLeaseFixture struct {
	ctx     context.Context
	st      store.PG
	srv     *Server
	rn      types.Runner
	fake    *runnertest.Fake // the sandbox substrate under the runner
	manager *offline.Manager
	hooks   runnerwire.RunnerHooks
	key     ed25519.PrivateKey
	// execs counts the Exec frames that reached the runner.
	execs atomic.Int32
	// oldRunner makes the runner one from before the BYOI selftest call, from its next session on.
	oldRunner atomic.Bool
	// disconnect ends the current session.
	disconnect context.CancelFunc
	h          *harness
	// sub is the substrate the runner serves: the fake, with calls that can be made to hang.
	sub *stallingSub
	// leaseEnds is the end of the last LEASE the runner accepted for each run.
	leaseMu   sync.Mutex
	leaseEnds map[uuid.UUID]time.Time
	// dir is the runner's state directory.
	dir string
	// streams is the runner's output server.
	streams *runnerio.Server
	// oldCaps makes the runner's caps event omit retire, as a runner older
	// than 0.9.3 sends it.
	oldCaps atomic.Bool
	// execOutput, when set, is the org's sink for each create's output.
	execOutput io.Writer
}

// leaseEnd waits for the runner to hold a lease of run that ends at want, and reports the end it holds.
func (f *runnerLeaseFixture) leaseEnd(run uuid.UUID, want time.Time) (got time.Time) {
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		f.leaseMu.Lock()
		got = f.leaseEnds[run]
		f.leaseMu.Unlock()
		if got.Equal(want) || time.Now().After(deadline) {
			return got
		}
	}
}

// stallingSub is the runner's sandbox substrate with one verb that can be made to hang until the call's
// context ends: a runner that takes a call and never answers it.
type stallingSub struct {
	*runnertest.Fake
	stall   atomic.Value // the verb that hangs: "can_replace" or "replace_proxy"
	release chan struct{}
	// goneIsStopped answers Status and AgentStatus for a removed sandbox as
	// the docker driver does (stopped) instead of the fake's ErrSandboxGone.
	goneIsStopped atomic.Bool
}

func (s *stallingSub) gone(st runner.Status, err error) (runner.Status, error) {
	if s.goneIsStopped.Load() && errors.Is(err, runner.ErrSandboxGone) {
		return runner.Status{State: types.RunStopped, Message: "container not found"}, nil
	}
	return st, err
}

func (s *stallingSub) Status(ctx context.Context, ref string) (runner.Status, error) {
	return s.gone(s.Fake.Status(ctx, ref))
}

func (s *stallingSub) AgentStatus(ctx context.Context, ref, execID string) (runner.Status, error) {
	return s.gone(s.Fake.AgentStatus(ctx, ref, execID))
}

func (s *stallingSub) hang(ctx context.Context, verb string) error {
	if v, _ := s.stall.Load().(string); v != verb {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.release:
		return errors.New("released")
	}
}

func (s *stallingSub) CanReplaceProxy(ctx context.Context, ref string) error {
	if err := s.hang(ctx, "can_replace"); err != nil {
		return err
	}
	return s.Fake.CanReplaceProxy(ctx, ref)
}

func (s *stallingSub) ReplaceProxy(ctx context.Context, ref string, cfg []byte) error {
	if err := s.hang(ctx, "replace_proxy"); err != nil {
		return err
	}
	return s.Fake.ReplaceProxy(ctx, ref, cfg)
}

func newRunnerLeaseFixture(t *testing.T) *runnerLeaseFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	st := store.NewPG(throwawayPGPool(t))
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	id := uuid.New()
	rn, err := st.CreateRunner(ctx, types.Runner{ID: id, Owner: "lease-owner", Name: "laptop", PublicKey: pub, KeyFingerprint: id.String(),
		Version: "0.9.0", OrgURLSHA256: federation.OrgURLSHA256(admissionOrgURL), Limits: roomyRunnerLimits()})
	if err != nil {
		t.Fatal(err)
	}
	if rn, err = st.ClaimRunner(ctx, rn.ID, rn.Owner, rn.KeyFingerprint, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutSiteConfig(ctx, types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.BaseCtx = ctx
	cfg.Runner = orchestrator.New(runnertest.NewFake())
	cfg.Audit = &syncAudit{}
	cfg.RunnerOrgURL = admissionOrgURL
	cfg.RunnerAuditPrepare = func(_ context.Context, ev types.AuditEvent) (types.AuditEvent, error) { return ev, nil }
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = getErrStore{getErr: secretstore.ErrNotFound}
	cfg.RunConfigKey = make([]byte, 32)
	cfg.ControlPlaneURL = "http://127.0.0.1:8080" // a hop the proxy config loader accepts
	f := &runnerLeaseFixture{ctx: ctx, st: st, srv: New(cfg), rn: rn, fake: runnertest.NewFake(), h: h, leaseEnds: map[uuid.UUID]time.Time{}}
	f.sub = &stallingSub{Fake: f.fake, release: make(chan struct{})}
	t.Cleanup(func() { close(f.sub.release) })

	// The runner side, composed as wardyn-runnerd composes it.
	dir := t.TempDir()
	f.dir = dir
	streams, err := runnerio.New(f.sub, id.String(), dir)
	if err != nil {
		t.Fatal(err)
	}
	f.manager, err = offline.New(dir, id, f.sub, func(run uuid.UUID) string { return "fake-" + run.String() }, streams.OutputSettled, streams.EraseOutput)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.manager.Close() })
	f.streams = streams
	m := f.manager
	m.ReleaseSettledOutput(streams.ReleaseOutput)
	dispatcher, err := runneragent.New(runneragent.Config{RunnerID: id.String(), Substrate: f.sub, HoldsSandbox: m.HoldsSandbox,
		BeforeCreate: m.BeforeCreate, Created: m.Created, CreateFailed: m.CreateFailed,
		BeforeRefCall: m.BeforeRefCall, AfterRefCall: m.AfterRefCall, MutationFinished: m.MutationFinished, ExecutionResult: m.ExecutionResult,
		CheckSelftest: m.CheckSelftest,
		Streams:       streams.Call, Output: streams.Output, BindOutput: streams.BindOutput, ForgetOutput: streams.ForgetOutput})
	if err != nil {
		t.Fatal(err)
	}
	f.key = key
	f.hooks = runnerwire.RunnerHooks{
		Call: func(ctx context.Context, peer *runnerwire.Peer, id uint32, method string, args json.RawMessage) (any, error) {
			if method == runnerwire.MethodExec {
				f.execs.Add(1)
			}
			if method == runnerwire.MethodSelftest && f.oldRunner.Load() {
				return nil, runnerwire.Refuse("unknown runner method") // what an older dispatcher answers
			}
			return dispatcher.Call(ctx, peer, id, method, args)
		},
		Open: func(*runnerwire.Peer, *runnerwire.Stream, runnerwire.Open) error {
			return runnerwire.Refuse("unsupported runner stream")
		},
		Clock: m.Clock, Pending: m.Pending, StatePages: m.StatePages,
		BeforeReady: m.BeforeReady, Revoked: m.Revoked, Offline: m.Offline,
		Lease: func(ctx context.Context, l runnerwire.Lease) error {
			err := m.Lease(ctx, l)
			if err == nil {
				f.leaseMu.Lock()
				f.leaseEnds[l.RunID] = l.EndsAt
				f.leaseMu.Unlock()
			}
			return err
		},
		Replay: func(ctx context.Context, peer *runnerwire.Peer) error {
			support, err := f.fake.Classes(ctx)
			if err != nil {
				return err
			}
			data, err := runnerwire.Marshal(runnerwire.Caps{Support: support, Version: "test", BYOISelftest: dispatcher.Selftests() && !f.oldRunner.Load(), Retire: !f.oldCaps.Load()})
			if err != nil {
				return err
			}
			if err := peer.EventAck(ctx, runnerwire.Event{Kind: runnerwire.EventCaps, Data: data}); err != nil {
				return err
			}
			return m.Replay(ctx, peer)
		},
		Online:        func(peer *runnerwire.Peer, _ bool) { m.Online(peer); streams.Online(peer) },
		StreamOffline: streams.Offline,
	}
	f.disconnect = f.connect(t)
	return f
}

// connect opens a fresh session of the runner (a new runner process's first connection, with the same
// state directory) and waits until it is READY. The returned func ends the session.
func (f *runnerLeaseFixture) connect(t *testing.T) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(f.ctx)
	t.Cleanup(cancel)
	hub := f.srv.runnerHub(f.st)
	id := f.rn.ID
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: admissionOrgURL, RunnerID: id.String(), PrivateKey: f.key, Hooks: f.hooks,
		Dial: func(context.Context) (runnerwire.Conn, error) {
			org, run := runnerwire.Loopback()
			go func() { _ = hub.Serve(ctx, org, id) }()
			return run, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = client.Connect(ctx) }()
	ep := f.srv.runnerEndpoint(id)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, ok := ep.sub.Caps(); ok && ep.link.Online() {
			return cancel
		}
		if time.Now().After(deadline) {
			t.Fatal("the runner never became ready")
		}
	}
}

// localRun is a STARTING task run bound to the fixture's runner whose token was stamped stampAge ago.
func (f *runnerLeaseFixture) localRun(t *testing.T, stampAge time.Duration) types.AgentRun {
	t.Helper()
	run := newFinalizeRun()
	run.CreatedBy, run.Placement, run.RunnerID, run.State = f.rn.Owner, types.PlacementLocal, &f.rn.ID, types.RunStarting
	run, err := f.st.CreateRun(f.ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	// Admission records the class the run was admitted at; a revive holds the runner to it.
	if err := f.st.SetRunRunnerTrustClass(f.ctx, run.ID, types.RunnerTrustPersonal); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(f.ctx, `UPDATE agent_runs SET token_renewed_at = now() - $2::interval WHERE id = $1`, run.ID, stampAge.String()); err != nil {
		t.Fatal(err)
	}
	return run
}

// create asks for the run's sandbox on its runner and records its ref, as dispatch does.
func (f *runnerLeaseFixture) create(t *testing.T, run *types.AgentRun) error {
	t.Helper()
	sb, err := f.srv.createSandboxAtTarget(f.ctx, *run, runner.SandboxSpec{RunID: run.ID, ConfinementClass: types.CC1, ExecOutput: f.execOutput})
	if err != nil {
		return err
	}
	run.SandboxRef = sb.Ref
	return f.st.SetSandboxRef(f.ctx, run.ID, sb.Ref)
}

func (f *runnerLeaseFixture) execution(t *testing.T, run types.AgentRun) store.RunnerExecution {
	t.Helper()
	cur, err := f.st.RunnerExecutionOf(f.ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return cur
}

// wrongGeneration is a substrate on the runner's own link that sends gen, bypassing the organisation's
// check, so the runner's own refusal is what is tested.
func (f *runnerLeaseFixture) wrongGeneration(gen uuid.UUID) *remote.Substrate {
	return remote.New(f.rn.ID.String(), f.srv.runnerEndpoint(f.rn.ID).link, remote.Options{
		ExecutionGeneration: func(context.Context, string, string) (uuid.UUID, error) { return gen, nil }})
}

func TestPG_RunnerLease_ExecutionGenerationEndToEnd(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	// The token was stamped just inside its lapse window, so the runner contains the run within seconds
	// unless a renewal reaches it.
	const margin = 6 * time.Second
	run := f.localRun(t, identity.RunTokenLapseAfter-margin)
	local := "fake-" + run.ID.String()

	// (a) The runner refuses a create no lease authorizes; leased, the create succeeds.
	if err := f.create(t, &run); err == nil || !strings.Contains(err.Error(), "lease") {
		t.Fatalf("create with no lease = %v, want the runner's lease refusal", err)
	}
	if len(f.fake.Created) != 0 {
		t.Fatal("an unleased create reached the substrate")
	}
	g1, err := f.srv.leaseRunOnRunner(f.ctx, run)
	if err != nil || g1.generation == uuid.Nil {
		t.Fatalf("lease = %+v, %v", g1, err)
	}
	if err := f.create(t, &run); err != nil {
		t.Fatalf("leased create: %v", err)
	}

	// (b) Exec carries G1 from the dispatch context. Without it the call is refused before it is sent, and
	// a generation the lease does not carry is refused by the runner; neither reaches the substrate.
	argv := []string{"/usr/local/bin/agent-run", "t"}
	if _, err := f.srv.cfg.Runner.Exec(f.ctx, run.SandboxRef, argv); err == nil {
		t.Fatal("an Exec with no generation was sent")
	}
	if _, err := f.wrongGeneration(uuid.New()).Exec(f.ctx, run.SandboxRef, argv); err == nil || !strings.Contains(err.Error(), "generation") {
		t.Fatalf("an Exec with another generation = %v, want the runner's refusal", err)
	}
	if f.fake.Called("exec", local) {
		t.Fatal("a refused Exec reached the substrate")
	}
	execID, err := f.srv.cfg.Runner.Exec(g1.bind(f.ctx), run.SandboxRef, argv)
	if err != nil {
		t.Fatalf("Exec under G1: %v", err)
	}
	if err := f.srv.bindRunnerExecution(f.ctx, g1, execID); err != nil {
		t.Fatal(err)
	}
	if cur := f.execution(t, run); cur.Generation != g1.generation || cur.ExecID != execID || execID == "" {
		t.Fatalf("execution = %+v, want G1 bound to %q", cur, execID)
	}
	// The agent's exit comes back through the runner: what the completion watcher waits on.
	if code, err := f.srv.cfg.Runner.Wait(f.ctx, run.SandboxRef); err != nil || code != 0 {
		t.Fatalf("Wait = %d, %v, want the agent's exit 0", code, err)
	}

	// A run that execs nothing (an Interactive one) is leased with no generation and created.
	idle := f.localRun(t, 0)
	idle.Task = ""
	if _, err := f.st.Pool.Exec(f.ctx, `UPDATE agent_runs SET task = '' WHERE id = $1`, idle.ID); err != nil {
		t.Fatal(err)
	}
	if e, err := f.srv.leaseRunOnRunner(f.ctx, idle); err != nil || e.generation != uuid.Nil {
		t.Fatalf("lease of a run with no task = %+v, %v, want no generation", e, err)
	}
	if err := f.create(t, &idle); err != nil {
		t.Fatalf("create of a leased run with no task: %v", err)
	}

	// (c) A renewal re-leases the run: past the window it was first leased for, its proxy stays up, while a
	// run that was never renewed is contained.
	control := f.localRun(t, identity.RunTokenLapseAfter-margin)
	if _, err := f.srv.leaseRunOnRunner(f.ctx, control); err != nil {
		t.Fatal(err)
	}
	if err := f.create(t, &control); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.StampRunTokenRenewed(f.ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.renewRunnerLease(f.ctx, run, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(margin + time.Second)
	if err := f.manager.CheckLapses(f.ctx); err != nil {
		t.Fatal(err)
	}
	if !f.fake.Called("stop_proxy", "fake-"+control.ID.String()) {
		t.Fatal("the unrenewed control run was not contained: the window did not pass")
	}
	if f.fake.Called("stop_proxy", local) {
		t.Fatal("the renewed run was contained at its first lease's lapse window")
	}

	// (d) Revive starts the stopped agent under a fresh generation G2, leased first; G1 is refused.
	if err := f.srv.startRunOnRunner(f.ctx, f.srv.cfg.Runner.(runner.SandboxStarter), run); err != nil {
		t.Fatalf("revive Start: %v", err)
	}
	cur := f.execution(t, run)
	if cur.Generation == g1.generation || cur.Operation != runnerwire.MethodStart || cur.ExecID != runner.MainProcessExecID {
		t.Fatalf("execution after revive = %+v, want a new Start generation bound", cur)
	}
	if !f.fake.Called("start", local) {
		t.Fatal("revive never started the agent")
	}
	if err := f.wrongGeneration(g1.generation).StartSandbox(f.ctx, run.SandboxRef); err == nil || !strings.Contains(err.Error(), "generation") {
		t.Fatalf("a Start under the spent G1 = %v, want the runner's refusal", err)
	}
}

// A run its runner holds contained (a kill queued for it) is never leased again: the runner would
// refuse the lease and end its session. The renewal is refused here, and the runner stays connected.
func TestPG_RunnerLease_NeverLeasesAContainedRun(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	run := f.localRun(t, 0)
	if _, err := f.srv.leaseRunOnRunner(f.ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := f.create(t, &run); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.st.QueueRunnerTeardown(f.ctx, f.rn.ID, types.RunnerActionKill, run.SandboxRef); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.renewRunnerLease(f.ctx, run, 0); !errors.Is(err, errRunnerContained) {
		t.Fatalf("renewal of a contained run = %v, want errRunnerContained", err)
	}
	if !f.srv.runnerEndpoint(f.rn.ID).link.Online() {
		t.Fatal("the runner's session ended")
	}
}

// running makes the run RUNNING with the given end, as dispatch and a set end leave it.
func (f *runnerLeaseFixture) running(t *testing.T, run *types.AgentRun, end *time.Time) {
	t.Helper()
	if _, err := f.st.Pool.Exec(f.ctx, `UPDATE agent_runs SET state = 'RUNNING', ends_at = $2 WHERE id = $1`, run.ID, end); err != nil {
		t.Fatal(err)
	}
	run.State, run.EndsAt = types.RunRunning, end
}

// placed leases and creates the run on the runner and starts its agent under G1, as dispatch does.
func (f *runnerLeaseFixture) placed(t *testing.T, run *types.AgentRun) runnerExecution {
	t.Helper()
	e, err := f.srv.leaseRunOnRunner(f.ctx, *run)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.create(t, run); err != nil {
		t.Fatal(err)
	}
	execID, err := f.srv.cfg.Runner.Exec(e.bind(f.ctx), run.SandboxRef, []string{"/usr/local/bin/agent-run", "t"})
	if err == nil {
		err = f.srv.bindRunnerExecution(f.ctx, e, execID)
	}
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (f *runnerLeaseFixture) contained(run types.AgentRun) bool {
	return f.fake.Called("stop_proxy", "fake-"+run.ID.String())
}

// fix-r1 F1: changing a run's end re-sends its lease, so the runner holds the new end, not the old. Both
// ends are hours ahead and the test waits on the LEASE the runner accepts, so the wall clock decides
// nothing: a busy host only makes it slower.
func TestPG_RunnerLease_EndChangeReLeases(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	first := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	extended, abandoned, renewed, control := f.localRun(t, 0), f.localRun(t, 0), f.localRun(t, 0), f.localRun(t, 0)
	for _, run := range []*types.AgentRun{&extended, &abandoned, &renewed, &control} {
		f.running(t, run, &first)
		f.placed(t, run)
		if got := f.leaseEnd(run.ID, first); !got.Equal(first) {
			t.Fatalf("the runner holds a lease ending %s for a run placed with the end %s", got, first)
		}
	}
	end := first.Add(time.Hour)
	if w := do(t, f.srv, http.MethodPatch, "/api/v1/runs/"+extended.ID.String(), adminToken, `{"ends_at":"`+end.Format(time.RFC3339)+`"}`); w.Code != http.StatusOK {
		t.Fatalf("set end = %d %s", w.Code, w.Body)
	}
	// fix-r2 N3: a renewal handed a copy of the run read before its end changed sends the end it has now.
	stale := renewed
	moved := renewed
	f.running(t, &moved, &end)
	if err := f.srv.renewRunnerLease(f.ctx, stale, 0); err != nil {
		t.Fatal(err)
	}
	// fix-r2 L2: the end is committed and the request that set it has gone; the re-lease still runs.
	f.running(t, &abandoned, &end)
	gone, cancel := context.WithCancel(f.ctx)
	cancel()
	f.srv.renewLeaseAfterEndChange(gone, abandoned.ID)
	for _, tc := range []struct {
		run  types.AgentRun
		what string
	}{
		{extended, "the run whose end was changed"},
		{abandoned, "the run whose request went away"},
		{renewed, "the run renewed from a stale copy"},
	} {
		if got := f.leaseEnd(tc.run.ID, end); !got.Equal(end) {
			t.Errorf("%s: the runner holds a lease ending %s, want its new end %s", tc.what, got, end)
		}
	}
	if got := f.leaseEnd(control.ID, first); !got.Equal(first) {
		t.Errorf("the run whose end did not change: the runner holds a lease ending %s, want %s", got, first)
	}
}

// End to end: after a run's end moves, the runner does not contain it at the old end, while a run whose
// end did not move is contained there. Two runs only, with ends far enough ahead that placing both fits
// before a lease is refused near its end; the test waits on the lease the runner accepts before the old
// end passes.
func TestPG_RunnerLease_OldEndIsNotEnforcedAfterAnEndChange(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	const margin = 10 * time.Second
	soon := time.Now().Add(margin).UTC().Truncate(time.Second)
	extended, control := f.localRun(t, 0), f.localRun(t, 0)
	for _, run := range []*types.AgentRun{&extended, &control} {
		f.running(t, run, &soon)
		f.placed(t, run)
	}
	end := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	if w := do(t, f.srv, http.MethodPatch, "/api/v1/runs/"+extended.ID.String(), adminToken, `{"ends_at":"`+end.Format(time.RFC3339)+`"}`); w.Code != http.StatusOK {
		t.Fatalf("set end = %d %s", w.Code, w.Body)
	}
	if got := f.leaseEnd(extended.ID, end); !got.Equal(end) {
		t.Fatalf("the runner holds a lease ending %s, want the new end %s", got, end)
	}
	time.Sleep(time.Until(soon) + time.Second)
	if err := f.manager.CheckLapses(f.ctx); err != nil {
		t.Fatal(err)
	}
	if !f.contained(control) {
		t.Fatal("the control run was not contained at its end: the end did not pass")
	}
	if f.contained(extended) {
		t.Fatal("the run whose end moved was contained at its old end")
	}
}

// fix-r1 Fable M1: a lease the runner would refuse is never sent, since a refused LEASE ends the runner's
// whole session. The run is refused alone; nothing is persisted for it, and the session carries on.
func TestPG_RunnerLease_RefusesALeaseTheRunnerWouldRefuse(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	lapsed := f.localRun(t, identity.RunTokenLapseAfter+time.Minute)
	if _, err := f.srv.leaseRunOnRunner(f.ctx, lapsed); !errors.Is(err, errRunnerLeaseLapsed) {
		t.Fatalf("lease of a lapsed run = %v, want errRunnerLeaseLapsed", err)
	}
	ended := f.localRun(t, 0)
	past := time.Now().Add(-time.Minute)
	f.running(t, &ended, &past)
	if _, err := f.srv.leaseRunOnRunner(f.ctx, ended); !errors.Is(err, errRunnerLeaseEnded) {
		t.Fatalf("lease of an ended run = %v, want errRunnerLeaseEnded", err)
	}
	for _, run := range []types.AgentRun{lapsed, ended} {
		if _, err := f.st.RunnerExecutionOf(f.ctx, run.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a refused lease persisted a generation for %s: %v", run.ID, err)
		}
	}
	time.Sleep(time.Second)
	live := f.localRun(t, 0)
	f.placed(t, &live)
	if !f.srv.runnerEndpoint(f.rn.ID).link.Online() {
		t.Fatal("the runner's session ended")
	}
	f.disconnect()
	for deadline := time.Now().Add(5 * time.Second); f.srv.runnerEndpoint(f.rn.ID).link.Online() && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}
	if err := f.srv.renewRunnerLease(f.ctx, live, 0); !errors.Is(err, errRunnerNotReady) {
		t.Fatalf("lease to a session that is not READY = %v, want errRunnerNotReady", err)
	}
}

// fix-r1 Fable M2: a new runner session is leased again once READY. A renewal that could not reach the
// runner while its session was down is carried by the new session, so the run keeps its proxy past the
// window it was first leased for.
func TestPG_RunnerLease_NewSessionReLeasesLiveRuns(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	const margin = 12 * time.Second
	start := time.Now()
	run := f.localRun(t, identity.RunTokenLapseAfter-margin)
	f.running(t, &run, nil)
	f.placed(t, &run)
	f.disconnect()
	for deadline := time.Now().Add(5 * time.Second); f.srv.runnerEndpoint(f.rn.ID).link.Online() && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}
	if _, err := f.st.StampRunTokenRenewed(f.ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.renewRunnerLease(f.ctx, run, 0); !errors.Is(err, errRunnerNotReady) {
		t.Fatalf("renewal while the runner is away = %v, want errRunnerNotReady", err)
	}
	f.disconnect = f.connect(t)
	time.Sleep(time.Until(start.Add(margin + time.Second)))
	if err := f.manager.CheckLapses(f.ctx); err != nil {
		t.Fatal(err)
	}
	if f.contained(run) {
		t.Fatal("the run was contained: its new session was not leased")
	}
}

// fix-r1 Opus F3: the organisation refuses a spent generation itself, before any frame reaches the runner.
func TestPG_RunnerLease_OrgRefusesASpentGeneration(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	run := f.localRun(t, 0)
	g1 := f.placed(t, &run)
	before := f.execs.Load()
	if _, err := f.srv.cfg.Runner.Exec(g1.bind(f.ctx), run.SandboxRef, []string{"again"}); !errors.Is(err, errRunnerExecutionUnbound) {
		t.Fatalf("an Exec reusing the bound G1 = %v, want the organisation's refusal", err)
	}
	if n := f.execs.Load(); n != before {
		t.Fatalf("%d Exec frame(s) reached the runner", n-before)
	}
}

// fix-r1 Opus I2, fix-r2 N1: a renewal never waits for the run's lock. While a holder (here a revive) has
// it, the renewal yields at once and sends nothing, so it neither lands between the revive's newer lease and
// its Start nor holds a lock connection waiting. Once the lock is free, a renewal goes through.
func TestPG_RunnerLease_ConcurrentRenewalYieldsToTheRunLock(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	run := f.localRun(t, 0)
	f.running(t, &run, nil)
	f.placed(t, &run)
	lockCtx, unlock, err := f.srv.lockRunOp(f.ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.srv.renewRunnerLease(f.ctx, run, 0) }()
	select {
	case err := <-done:
		if !errors.Is(err, errRunnerLeaseBusy) {
			unlock()
			t.Fatalf("renewal under a held lock = %v, want errRunnerLeaseBusy", err)
		}
	case <-time.After(3 * time.Second):
		unlock()
		t.Fatal("the renewal waited for the run's lock")
	}
	if err := f.srv.startRunOnRunner(lockCtx, f.srv.cfg.Runner.(runner.SandboxStarter), run); err != nil {
		unlock()
		t.Fatalf("revive Start under the lock: %v", err)
	}
	unlock()
	if err := f.srv.renewRunnerLease(f.ctx, run, 0); err != nil {
		t.Fatalf("renewal once the lock is free: %v", err)
	}
}

// fix-r2 N1: a runner that stops acknowledging cannot hold the run's lock past the LEASE send timeout. The
// runner side here reads every frame and never ACKs, so the session's replay room fills; the renewal under
// the lock gives up at leaseSendTimeout and the lock is free for its next user.
func TestPG_RunnerLease_ConcurrentStalledSendReleasesTheLock(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	run := f.localRun(t, 0)
	f.running(t, &run, nil)
	f.placed(t, &run)
	f.mute(t)
	done := make(chan error, 1)
	go func() { done <- f.srv.renewRunnerLease(f.ctx, run, 0) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a lease went through a session with no replay room")
		}
	case <-time.After(leaseSendTimeout + 5*time.Second):
		t.Fatal("the stalled send held the run's lock past its timeout")
	}
	ctx, cancel := context.WithTimeout(f.ctx, 2*time.Second)
	defer cancel()
	_, unlock, err := f.srv.lockRunOp(ctx, run.ID)
	if err != nil {
		t.Fatalf("the run's lock after the stalled send: %v", err)
	}
	unlock()
}

// fix-r2 F3: the organisation refuses a generation other than the persisted one itself, before any frame
// reaches the runner, and the persisted one still goes through.
func TestPG_RunnerLease_OrgRefusesAnotherGeneration(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	run := f.localRun(t, 0)
	g1, err := f.srv.leaseRunOnRunner(f.ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.create(t, &run); err != nil {
		t.Fatal(err)
	}
	other := g1
	other.generation = uuid.New()
	before := f.execs.Load()
	if _, err := f.srv.cfg.Runner.Exec(other.bind(f.ctx), run.SandboxRef, []string{"t"}); !errors.Is(err, errRunnerExecutionUnbound) {
		t.Fatalf("an Exec under another generation = %v, want the organisation's refusal", err)
	}
	if n := f.execs.Load(); n != before {
		t.Fatalf("%d Exec frame(s) reached the runner", n-before)
	}
	if _, err := f.srv.cfg.Runner.Exec(g1.bind(f.ctx), run.SandboxRef, []string{"t"}); err != nil {
		t.Fatalf("the persisted generation: %v", err)
	}
}

// fix-r2 N4: dispatchAdmitted holds the dispatch ceiling's deny_placements to the run, so a lane that never
// passed a create door fails the run before anything is claimed.
func TestPG_RunnerLease_DispatchAdmittedRefusesADeniedPlacement(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	run := f.localRun(t, 0)
	if _, err := f.st.Pool.Exec(f.ctx, `UPDATE agent_runs SET state = 'PENDING' WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	run.State = types.RunPending
	limits := types.GovernanceLimits{DenyPlacements: []types.Placement{types.PlacementLocal}}
	denied := ceilingForDispatch(governanceCeiling{Profile: &ResolvedProfile{Name: "no-local", Limits: limits}, Limits: limits}, adoEntraUngraded(), bedrockCredUngraded())
	if f.srv.dispatchAdmitted(f.ctx, run, denied) {
		t.Fatal("dispatch admitted a placement its profile denies")
	}
	stored, err := f.st.GetRun(f.ctx, run.ID)
	if err != nil || stored.State != types.RunFailed {
		t.Fatalf("run after a denied dispatch = %s (%v), want FAILED", stored.State, err)
	}
}

// fix-r1 Fable M3 and Opus I1: a run on a runner revives through the door (reviveRunProxy) on the real
// runner side: re-leased on the claim's stamp, its proxy replaced, its stopped agent started under a fresh
// generation. Once the owner's profile denies its placement, the next revive is refused placement_denied.
func TestPG_RunnerLease_ReviveThroughTheDoor(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	run := f.localRun(t, 0)
	f.running(t, &run, nil)
	g1 := f.placed(t, &run)
	if err := f.srv.keepRunProxyConfig(f.ctx, run.ID, runner.ProxyConfig{RunToken: "old", ControlPlaneURL: "http://127.0.0.1:8081"}); err != nil {
		t.Fatal(err)
	}
	if lost, err := f.st.MarkRunLost(f.ctx, run.ID, types.LostReboot, time.Now(), 0); err != nil || !lost {
		t.Fatalf("mark lost = %v, %v", lost, err)
	}
	stored, err := f.st.GetRun(f.ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	res, rerr := f.srv.reviveRunProxy(f.ctx, stored, types.ActorSystem, "test", true)
	if rerr != nil {
		t.Fatalf("revive = %+v", rerr)
	}
	local := "fake-" + run.ID.String()
	replaced := slices.ContainsFunc(f.fake.Calls, func(c string) bool {
		return strings.HasPrefix(c, "replace_proxy ") && strings.Contains(c, run.ID.String())
	})
	if !res.AgentStarted || !replaced || !f.fake.Called("start", local) {
		t.Fatalf("revive = %+v, calls %v: want the proxy replaced and the agent started", res, f.fake.Calls)
	}
	if cur := f.execution(t, run); cur.Generation == g1.generation || cur.Operation != runnerwire.MethodStart || cur.ExecID != runner.MainProcessExecID {
		t.Fatalf("execution after revive = %+v, want a new Start generation bound", cur)
	}

	profile, err := f.st.UpsertGovernanceProfile(f.ctx, types.GovernanceProfile{Name: "no-local", CreatedBy: "seed",
		Limits: types.GovernanceLimits{DenyPlacements: []types.Placement{types.PlacementLocal}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(f.ctx, `UPDATE agent_runs SET governance_profile_id = $2 WHERE id = $1`, run.ID, profile.ID); err != nil {
		t.Fatal(err)
	}
	if stored, err = f.st.GetRun(f.ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, rerr := f.srv.reviveRunProxy(f.ctx, stored, types.ActorSystem, "test", true); rerr == nil || rerr.reason != string(placement.ReasonPlacementDenied) {
		t.Fatalf("revive under a profile denying local = %+v, want placement_denied", rerr)
	}
	// fix-r4 R3-3: a managed-harness sign-in run is exempt at revive, as at create and dispatch.
	signIn := stored
	signIn.Task = harnessLoginTask
	if rerr := f.srv.revivePlacementDenied(f.ctx, signIn, ownerCeiling{profile: profile.Name, denyPlacements: profile.Limits.DenyPlacements}, types.ActorSystem, "test"); rerr != nil {
		t.Fatalf("revive of a sign-in run under a profile denying its placement = %+v, want it exempt", rerr)
	}
	if rerr := f.srv.revivePlacementDenied(f.ctx, stored, ownerCeiling{profile: profile.Name, denyPlacements: profile.Limits.DenyPlacements}, types.ActorSystem, "test"); rerr == nil {
		t.Fatal("the same run without the sign-in task was not denied")
	}
	// Dispatch holds the same profile to the run's placement.
	denied := ceilingForDispatch(governanceCeiling{Profile: &ResolvedProfile{ID: profile.ID, Name: profile.Name, Limits: profile.Limits}, Limits: profile.Limits}, adoEntraUngraded(), bedrockCredUngraded())
	if !f.srv.dispatchPlacementDenied(f.ctx, stored, denied) {
		t.Fatal("dispatch let a denied placement through")
	}
	if f.srv.dispatchPlacementDenied(f.ctx, stored, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded())) {
		t.Fatal("dispatch refused a run no profile denies")
	}
}

// fix-r2 N5: a runner's new session re-leases every live run on it, a run held for a confirmation included.
func TestPG_RunnerLease_LiveRunsIncludeHeldRuns(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	want := map[uuid.UUID]bool{}
	for _, state := range []types.RunState{types.RunStarting, types.RunRunning, types.RunWaiting, types.RunCompleted} {
		run := f.localRun(t, 0)
		if _, err := f.st.Pool.Exec(f.ctx, `UPDATE agent_runs SET state = $2 WHERE id = $1`, run.ID, string(state)); err != nil {
			t.Fatal(err)
		}
		if state != types.RunCompleted {
			want[run.ID] = true
		}
	}
	runs, err := f.st.ListLiveRunnerRuns(f.ctx, f.rn.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]bool{}
	for _, r := range runs {
		got[r.ID] = true
	}
	if !maps.Equal(got, want) {
		t.Fatalf("live runs = %v, want %v", got, want)
	}
}

// mute replaces the runner's session with one that reads every frame and never acknowledges, and fills its
// replay room: every later LEASE send waits for room until its deadline.
func (f *runnerLeaseFixture) mute(t *testing.T) {
	t.Helper()
	f.muteSession(t, f.rn.ID)
}

// muteSession gives runner id a session that reads every frame and never answers or acknowledges, its
// replay room filled.
func (f *runnerLeaseFixture) muteSession(t *testing.T, id uuid.UUID) {
	t.Helper()
	org, mute := runnerwire.Loopback()
	go func() {
		for {
			if _, err := mute.ReadFrame(f.ctx); err != nil {
				return
			}
		}
	}()
	link := f.srv.runnerEndpoint(id).link
	link.Attach(f.ctx, org)
	payload, err := runnerwire.Marshal(runnerwire.Lease{RunID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		ctx, cancel := context.WithTimeout(f.ctx, 200*time.Millisecond)
		err := link.Send(ctx, runnerwire.Frame{Type: runnerwire.TypeLease, Payload: payload})
		cancel()
		if err != nil {
			return
		}
		if i > 50000 {
			t.Fatal("the replay room never filled")
		}
	}
}

// fix-r3 R2-2: a mute runner holds at most one lock connection however many of its runs are re-leased: one
// LEASE send per runner at a time. With nine of its runs renewing at once, an unrelated lock still gets a
// connection.
func TestPG_RunnerLease_ConcurrentMuteRunnerHoldsOneLockConnection(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	var runs []types.AgentRun
	for range 9 {
		run := f.localRun(t, 0)
		f.running(t, &run, nil)
		f.placed(t, &run)
		runs = append(runs, run)
	}
	f.mute(t)
	done := make(chan error, len(runs))
	for _, run := range runs {
		go func() { done <- f.srv.renewRunnerLease(f.ctx, run, 0) }()
	}
	time.Sleep(500 * time.Millisecond)
	ctx, cancel := context.WithTimeout(f.ctx, 2*time.Second)
	defer cancel()
	_, unlock, err := f.srv.lockRunOp(ctx, uuid.New())
	if err != nil {
		t.Fatalf("an unrelated lock while the mute runner's runs renew: %v", err)
	}
	unlock()
	busy := 0
	for range runs {
		if errors.Is(<-done, errRunnerLeaseBusy) {
			busy++
		}
	}
	if busy < len(runs)-1 {
		t.Errorf("%d of %d renewals yielded the runner's slot, want all but one", busy, len(runs))
	}
}

// fix-r3 R2-1, R2-6: a revive of a run on a runner releases the run's lock at its one runner deadline,
// whichever call the runner never answers: before the claim (CanReplaceProxy) or after it (ReplaceProxy).
func TestPG_RunnerLease_ConcurrentReviveReleasesTheLockAtItsDeadline(t *testing.T) {
	saved := reviveRunnerTimeout
	reviveRunnerTimeout = 2 * time.Second
	t.Cleanup(func() { reviveRunnerTimeout = saved })
	for _, verb := range []string{"can_replace", "replace_proxy"} {
		t.Run(verb, func(t *testing.T) {
			f := newRunnerLeaseFixture(t)
			run := f.localRun(t, 0)
			f.running(t, &run, nil)
			f.placed(t, &run)
			if err := f.srv.keepRunProxyConfig(f.ctx, run.ID, runner.ProxyConfig{RunToken: "old", ControlPlaneURL: "http://127.0.0.1:8081"}); err != nil {
				t.Fatal(err)
			}
			if lost, err := f.st.MarkRunLost(f.ctx, run.ID, types.LostReboot, time.Now(), 0); err != nil || !lost {
				t.Fatalf("mark lost = %v, %v", lost, err)
			}
			stored, err := f.st.GetRun(f.ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			f.sub.stall.Store(verb)
			done := make(chan *reviveError, 1)
			go func() {
				_, rerr := f.srv.reviveRunProxy(f.ctx, stored, types.ActorSystem, "test", true)
				done <- rerr
			}()
			select {
			case rerr := <-done:
				if rerr == nil {
					t.Fatal("a revive whose runner never answered succeeded")
				}
			case <-time.After(reviveRunnerTimeout + 8*time.Second):
				t.Fatal("the revive held the run's lock past its runner deadline")
			}
			_, unlock, ok := f.srv.tryLockRunOp(f.ctx, run.ID)
			if !ok {
				t.Fatal("the run's lock is still held after the revive returned")
			}
			unlock()
		})
	}
}

// fix-r3 R2-4: an end change waits out a lease sender holding the run's lock (a renewal that read the old
// end) instead of yielding to it, so its own lease, with the new end, lands last.
func TestPG_RunnerLease_ConcurrentEndChangeWaitsOutARenewal(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	const margin = 6 * time.Second
	soon := time.Now().Add(margin)
	run := f.localRun(t, 0)
	f.running(t, &run, &soon)
	f.placed(t, &run)
	_, unlock, err := f.srv.lockRunOp(f.ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Hour)
	f.running(t, &run, &later)
	done := make(chan struct{})
	go func() { f.srv.renewLeaseAfterEndChange(f.ctx, run.ID); close(done) }()
	time.Sleep(time.Second)
	unlock()
	<-done
	time.Sleep(time.Until(soon.Add(time.Second)))
	if err := f.manager.CheckLapses(f.ctx); err != nil {
		t.Fatal(err)
	}
	if f.contained(run) {
		t.Fatal("the end change yielded to the lock's holder and the run was contained at its old end")
	}
}

// fix-r3 R2-5: a token renewal that meets another lease sender answers 503 (the proxy keeps its token and
// retries in a minute); once the sender is done the renewal goes through.
func TestPG_RunnerLease_BusyRenewalAnswers503(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	run := f.localRun(t, 0)
	f.running(t, &run, nil)
	f.placed(t, &run)
	tok := f.h.mintRunToken(t, run.ID)
	mints := &mintCounter{Provider: f.srv.cfg.Identity}
	f.srv.cfg.Identity = mints
	release, ok := f.srv.runnerOwnerSlots(f.rn.Owner).acquireLease(f.ctx, 0)
	if !ok {
		t.Fatal("the runner's slot is taken")
	}
	// The renewal arrives over the run's own runner relay, as a run on a runner's always does.
	renew := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/internal/token/renew", nil)
		r = r.WithContext(context.WithValue(audit.WithRelay(r.Context(), f.rn.ID), relayRunnerKey{}, f.rn.ID))
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		panicFails(t, f.srv.Handler()).ServeHTTP(w, r)
		return w
	}
	w := renew()
	release()
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), reasonRunRenewLeaseBusy) {
		t.Fatalf("renewal while the runner's lease is being sent = %d %s, want 503 %s", w.Code, w.Body, reasonRunRenewLeaseBusy)
	}
	// fix-r4 R3-2: a busy renewal changed nothing: no token minted, so no orphan identity.mint row.
	if n := mints.n.Load(); n != 0 {
		t.Fatalf("a busy renewal minted %d token(s)", n)
	}
	if w := renew(); w.Code != http.StatusOK {
		t.Fatalf("renewal once the sender is done = %d %s, want 200", w.Code, w.Body)
	}
}

// mintCounter counts run identities minted.
type mintCounter struct {
	identity.Provider
	n atomic.Int32
}

func (m *mintCounter) MintRunIdentity(ctx context.Context, runID uuid.UUID, humanSub, sponsor, audience string, operatorOwned bool) (identity.RunIdentity, error) {
	m.n.Add(1)
	return m.Provider.MintRunIdentity(ctx, runID, humanSub, sponsor, audience, operatorOwned)
}

// fix-r4 R3-1: one runner's revives hold at most one lock connection: the runner's revive slot is taken
// before the run's lock. With nine of a mute runner's runs revived at once, an unrelated lock still gets a
// connection.
func TestPG_RunnerLease_ConcurrentMuteRunnerRevivesHoldOneLockConnection(t *testing.T) {
	saved := reviveRunnerTimeout
	reviveRunnerTimeout = 5 * time.Second
	t.Cleanup(func() { reviveRunnerTimeout = saved })
	f := newRunnerLeaseFixture(t)
	var runs []types.AgentRun
	for range 9 {
		run := f.localRun(t, 0)
		f.running(t, &run, nil)
		f.placed(t, &run)
		if err := f.srv.keepRunProxyConfig(f.ctx, run.ID, runner.ProxyConfig{RunToken: "old", ControlPlaneURL: "http://127.0.0.1:8081"}); err != nil {
			t.Fatal(err)
		}
		stored, err := f.st.GetRun(f.ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, stored)
	}
	f.mute(t)
	done := make(chan *reviveError, len(runs))
	for _, run := range runs {
		go func() {
			_, rerr := f.srv.reviveRunProxy(f.ctx, run, types.ActorSystem, "test", true)
			done <- rerr
		}()
	}
	time.Sleep(time.Second)
	ctx, cancel := context.WithTimeout(f.ctx, 2*time.Second)
	defer cancel()
	_, unlock, err := f.srv.lockRunOp(ctx, uuid.New())
	if err != nil {
		t.Fatalf("an unrelated lock while the mute runner's runs are revived: %v", err)
	}
	unlock()
	for range runs {
		select {
		case rerr := <-done:
			if rerr == nil {
				t.Error("a revive against a mute runner succeeded")
			}
		case <-time.After(reviveRunnerTimeout + 3*leaseWait):
			t.Fatal("a revive did not return")
		}
	}
}

// muteRunner is a claimed runner of owner whose session never answers.
func (f *runnerLeaseFixture) muteRunner(t *testing.T, owner string) uuid.UUID {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	id := uuid.New()
	rn, err := f.st.CreateRunner(f.ctx, types.Runner{ID: id, Owner: owner, Name: "spare", PublicKey: pub, KeyFingerprint: id.String(),
		Version: "0.9.0", OrgURLSHA256: federation.OrgURLSHA256(admissionOrgURL), Limits: roomyRunnerLimits()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.ClaimRunner(f.ctx, rn.ID, owner, rn.KeyFingerprint, time.Now()); err != nil {
		t.Fatal(err)
	}
	f.muteSession(t, id)
	return id
}

// muteRun is a RUNNING run of owner on runnerID, with a sandbox and a stored proxy config to revive from.
func (f *runnerLeaseFixture) muteRun(t *testing.T, owner string, runnerID uuid.UUID) types.AgentRun {
	t.Helper()
	run := newFinalizeRun()
	run.CreatedBy, run.Placement, run.RunnerID = owner, types.PlacementLocal, &runnerID
	run, err := f.st.CreateRun(f.ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetSandboxRef(f.ctx, run.ID, "runner:"+runnerID.String()+"/fake-"+run.ID.String()); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.keepRunProxyConfig(f.ctx, run.ID, runner.ProxyConfig{RunToken: "old", ControlPlaneURL: "http://127.0.0.1:8081"}); err != nil {
		t.Fatal(err)
	}
	if run, err = f.st.GetRun(f.ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	return run
}

// fix-r5 R4-1: both slots are the runner owner's, so one person holds at most two lock connections however
// many runners they claim: four mute runners, a revive and a renewal on each at once, and an unrelated lock
// still gets a connection. Another owner's revive is not held behind the first owner's slot.
func TestPG_RunnerLease_ConcurrentOneOwnerHoldsTwoLockConnections(t *testing.T) {
	saved := reviveRunnerTimeout
	// Longer than leaseWait, so the owner's other revives give up waiting for the slot the first one holds.
	reviveRunnerTimeout = leaseWait + 2*time.Second
	t.Cleanup(func() { reviveRunnerTimeout = saved })
	f := newRunnerLeaseFixture(t)
	revived, renewed := make(chan *reviveError, 4), make(chan error, 4)
	for range 4 {
		id := f.muteRunner(t, f.rn.Owner)
		revive, renew := f.muteRun(t, f.rn.Owner, id), f.muteRun(t, f.rn.Owner, id)
		go func() {
			_, rerr := f.srv.reviveRunProxy(f.ctx, revive, types.ActorSystem, "test", true)
			revived <- rerr
		}()
		go func() { renewed <- f.srv.renewRunnerLease(f.ctx, renew, 0) }()
	}
	other := f.muteRun(t, "other-owner", f.muteRunner(t, "other-owner"))
	time.Sleep(time.Second)
	otherDone := make(chan *reviveError, 1)
	go func() {
		_, rerr := f.srv.reviveRunProxy(f.ctx, other, types.ActorSystem, "test", true)
		otherDone <- rerr
	}()
	ctx, cancel := context.WithTimeout(f.ctx, 2*time.Second)
	defer cancel()
	_, unlock, err := f.srv.lockRunOp(ctx, uuid.New())
	if err != nil {
		t.Fatalf("an unrelated lock while one owner's mute runners are revived and renewed: %v", err)
	}
	unlock()
	wait := reviveRunnerTimeout + 3*leaseWait
	select {
	case rerr := <-otherDone:
		if rerr == nil || rerr.reason == reasonLockUnavailable {
			t.Errorf("another owner's revive = %+v, want it past the first owner's slot to its own runner", rerr)
		}
	case <-time.After(wait):
		t.Fatal("another owner's revive did not return")
	}
	slotBusy := 0
	for range 4 {
		select {
		case rerr := <-revived:
			if rerr != nil && rerr.reason == reasonLockUnavailable {
				slotBusy++
			}
		case <-time.After(wait):
			t.Fatal("a revive did not return")
		}
		select {
		case <-renewed:
		case <-time.After(wait):
			t.Fatal("a renewal did not return")
		}
	}
	// One revive slot for the owner: the revives on the owner's other runners waited for it and gave up.
	if slotBusy < 2 {
		t.Errorf("%d of the owner's 4 revives answered lock_unavailable, want at least 2: the revive slot is not the owner's", slotBusy)
	}
}
