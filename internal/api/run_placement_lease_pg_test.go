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
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/identity"
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
}

func newRunnerLeaseFixture(t *testing.T) *runnerLeaseFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	st := store.NewPG(throwawayPGPool(t))
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	id := uuid.New()
	rn, err := st.CreateRunner(ctx, types.Runner{ID: id, Owner: "lease-owner", Name: "laptop", PublicKey: pub, KeyFingerprint: id.String(),
		Version: "0.9.0", OrgURLSHA256: federation.OrgURLSHA256(admissionOrgURL)})
	if err != nil {
		t.Fatal(err)
	}
	if rn, err = st.ClaimRunner(ctx, rn.ID, rn.Owner, rn.KeyFingerprint, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutSiteConfig(ctx, types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	cfg := baseTestConfig(newHarness(t), st)
	cfg.BaseCtx = ctx
	cfg.Runner = orchestrator.New(runnertest.NewFake())
	cfg.Audit = &syncAudit{}
	cfg.RunnerOrgURL = admissionOrgURL
	cfg.RunnerAuditPrepare = func(_ context.Context, ev types.AuditEvent) (types.AuditEvent, error) { return ev, nil }
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = getErrStore{getErr: secretstore.ErrNotFound}
	f := &runnerLeaseFixture{ctx: ctx, st: st, srv: New(cfg), rn: rn, fake: runnertest.NewFake()}

	// The runner side, composed as wardyn-runnerd composes it.
	dir := t.TempDir()
	streams, err := runnerio.New(f.fake, id.String(), dir)
	if err != nil {
		t.Fatal(err)
	}
	f.manager, err = offline.New(dir, id, f.fake, func(run uuid.UUID) string { return "fake-" + run.String() }, streams.OutputSettled, streams.EraseOutput)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.manager.Close() })
	m := f.manager
	dispatcher, err := runneragent.New(runneragent.Config{RunnerID: id.String(), Substrate: f.fake,
		BeforeCreate: m.BeforeCreate, Created: m.Created, CreateFailed: m.CreateFailed,
		BeforeRefCall: m.BeforeRefCall, AfterRefCall: m.AfterRefCall, MutationFinished: m.MutationFinished, ExecutionResult: m.ExecutionResult,
		Streams: streams.Call, Output: streams.Output, BindOutput: streams.BindOutput, ForgetOutput: streams.ForgetOutput})
	if err != nil {
		t.Fatal(err)
	}
	hooks := runnerwire.RunnerHooks{
		Call: dispatcher.Call,
		Open: func(*runnerwire.Peer, *runnerwire.Stream, runnerwire.Open) error {
			return runnerwire.Refuse("unsupported runner stream")
		},
		Clock: m.Clock, Pending: m.Pending, StatePages: m.StatePages,
		BeforeReady: m.BeforeReady, Lease: m.Lease, Revoked: m.Revoked, Offline: m.Offline,
		Replay: func(ctx context.Context, peer *runnerwire.Peer) error {
			support, err := f.fake.Classes(ctx)
			if err != nil {
				return err
			}
			data, err := runnerwire.Marshal(runnerwire.Caps{Support: support, Version: "test"})
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
	hub := f.srv.runnerHub(st)
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: admissionOrgURL, RunnerID: id.String(), PrivateKey: key, Hooks: hooks,
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
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the runner never became ready")
		}
	}
	return f
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
	if _, err := f.st.Pool.Exec(f.ctx, `UPDATE agent_runs SET token_renewed_at = now() - $2::interval WHERE id = $1`, run.ID, stampAge.String()); err != nil {
		t.Fatal(err)
	}
	return run
}

// create asks for the run's sandbox on its runner and records its ref, as dispatch does.
func (f *runnerLeaseFixture) create(t *testing.T, run *types.AgentRun) error {
	t.Helper()
	sb, err := f.srv.createSandboxAtTarget(f.ctx, *run, runner.SandboxSpec{RunID: run.ID, ConfinementClass: types.CC1})
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
	if err := f.srv.renewRunnerLease(f.ctx, run); err != nil {
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
	if err := f.srv.renewRunnerLease(f.ctx, run); !errors.Is(err, errRunnerContained) {
		t.Fatalf("renewal of a contained run = %v, want errRunnerContained", err)
	}
	if !f.srv.runnerEndpoint(f.rn.ID).link.Online() {
		t.Fatal("the runner's session ended")
	}
}
