// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// Two replicas finalise one run: the launcher's incomplete row commits first, then the complete row of the replica
// that took the run over replaces it, and only that replica audits the repair, as a success.
func TestPeerAdopterCompleteRowReplacesLauncherPartial_PG(t *testing.T) {
	l := newMaskLab(t)
	a, b := l.replica(), l.replica()
	run := l.run()
	a.dispatch(t, run)
	st := store.NewPG(l.pool)
	if err := st.InsertPendingRunOutput(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	writeExecOutput(t, a.srv.openExecOutput(run, false), "part\n")
	writeExecOutput(t, b.srv.openExecOutput(run, false), "part\nand the rest\n")
	ta, tb := a.srv.tailFor(run.ID), b.srv.tailFor(run.ID)
	if ta == nil || tb == nil {
		t.Fatal("each replica should hold a tail for the run")
	}

	partial := store.RunOutput{RunID: run.ID, Output: []byte("part\n"), Incomplete: true, Source: "stdout", MaskScope: "run"}
	if got := a.srv.tryPersistOutput(t.Context(), ta, st, &partial); got != persistDone {
		t.Fatalf("A's incomplete row: outcome %v, want persistDone", got)
	}
	if n := replacedAudits(t, l, run.ID); n != 0 {
		t.Fatalf("A's write over the pending row audited %d replacements", n)
	}
	full := store.RunOutput{RunID: run.ID, Output: []byte("part\nand the rest\n"), Source: "stdout", MaskScope: "run"}
	if got := b.srv.tryPersistOutput(t.Context(), tb, st, &full); got != persistDone {
		t.Fatalf("B's complete row: outcome %v, want persistDone", got)
	}
	row, found, err := st.GetRunOutput(t.Context(), run.ID)
	if err != nil || !found || string(row.Output) != "part\nand the rest\n" || row.Incomplete {
		t.Fatalf("stored row = %q incomplete=%v found=%v err=%v, want B's complete bytes", row.Output, row.Incomplete, found, err)
	}
	if n := replacedAudits(t, l, run.ID); n != 1 {
		t.Fatalf("%d run.output.finalize rows with replaced_incomplete, want B's one: %s", n, auditDump(l.rec.snapshot(), run.ID))
	}

	// A's late mark, for the row B replaced, lands after the replacement: it is dropped, and B's row stays complete.
	revA, revB := tailRevision(ta), tailRevision(tb)
	if revA.IsZero() || revB.IsZero() || revA.Equal(revB) {
		t.Fatalf("capture revisions A=%v B=%v, want two distinct stamps", revA, revB)
	}
	if !a.srv.markLateIncomplete(t.Context(), run.ID, revA) {
		t.Fatal("A's superseded mark was not settled")
	}
	if row, _, err := st.GetRunOutput(t.Context(), run.ID); err != nil || row.Incomplete {
		t.Fatalf("after A's superseded mark: incomplete=%v err=%v, want B's row complete", row.Incomplete, err)
	}
	// A byte B's own capture drops after its commit still marks B's row.
	if !b.srv.markLateIncomplete(t.Context(), run.ID, revB) {
		t.Fatal("B's own late mark was not settled")
	}
	if row, _, err := st.GetRunOutput(t.Context(), run.ID); err != nil || !row.Incomplete || string(row.Output) != "part\nand the rest\n" {
		t.Fatalf("after B's own late mark: %q incomplete=%v err=%v, want B's bytes marked incomplete", row.Output, row.Incomplete, err)
	}
}

// tailRevision is the captured_at of the row e committed.
func tailRevision(e *execOutputTail) time.Time {
	e.fmu.Lock()
	defer e.fmu.Unlock()
	return e.revision
}

// replacedAudits counts the run's run.output.finalize rows that report a replaced incomplete row, each checked for
// outcome success and step replace.
func replacedAudits(t *testing.T, l *maskLab, runID uuid.UUID) int {
	t.Helper()
	n := 0
	for _, ev := range l.rec.snapshot() {
		if ev.Action != "run.output.finalize" || ev.RunID == nil || *ev.RunID != runID {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data["replaced_incomplete"] != true {
			continue
		}
		if ev.Outcome != "success" || data["step"] != "replace" {
			t.Errorf("replacement audit outcome %q step %v, want success and replace", ev.Outcome, data["step"])
		}
		n++
	}
	return n
}

// skewedReplica is a second server on f's database whose clock runs skew ahead of the holder's, with its holder view
// read once, so its Link for f's runner routes through f.
func (f *peerFixture) skewedReplica(t *testing.T, skew time.Duration) *Server {
	t.Helper()
	cfg := baseTestConfig(newHarness(t), f.st)
	cfg.BaseCtx = f.ctx
	cfg.Now = func() time.Time { return time.Now().Add(skew) }
	peerShape(newPeerConfig())(&cfg)
	b := New(cfg)
	p := b.runnerPeering()
	if p == nil {
		t.Fatal("the second replica has no peering")
	}
	if err := p.view.Refresh(f.ctx); err != nil {
		t.Fatal(err)
	}
	if !b.runnerEndpoint(f.rn.ID).link.Online() {
		t.Fatal("the second replica does not see the runner held elsewhere")
	}
	return b
}

// leaseFramesFor is how many LEASE frames the runner has received for run.
func (f *peerFixture) leaseFramesFor(run uuid.UUID) int { return f.leasesStamped(run, time.Time{}) }

// A replica that does not hold the runner's session refuses no lease on its own clock: with the two clocks on
// opposite sides of the lapse margin, the holder's clock decides both ways, and a deferred lease on the replica
// whose own clock reads the stamp as lapsed is still sent through the holder.
func TestPeerLeaseFollowsTheHolderClock_PG(t *testing.T) {
	shortenRenewedLeaseRetry(t)
	f := newPeerFixture(t)
	f.waitHolder(t, f.session(t))
	// The stamp is 30 s inside the lapse on the holder's clock; a minute ahead, it is past it.
	age := identity.RunTokenLapseAfter - 30*time.Second
	run := f.localRun(t, 0)
	f.running(t, &run, nil)
	f.placed(t, &run)
	if _, err := f.st.Pool.Exec(f.ctx, `UPDATE agent_runs SET token_renewed_at = now() - $2::interval WHERE id = $1`, run.ID, age.String()); err != nil {
		t.Fatal(err)
	}

	ahead := f.skewedReplica(t, time.Minute)
	before := f.leaseFramesFor(run.ID)
	if err := ahead.renewRunnerLease(f.ctx, run, 0); err != nil {
		t.Fatalf("a lease the holder's clock accepts, asked by a replica whose clock reads it lapsed = %v, want sent", err)
	}
	if n := f.leaseFramesFor(run.ID); n != before+1 {
		t.Fatalf("the runner got %d new LEASE frame(s), want 1", n-before)
	}

	before = f.leaseFramesFor(run.ID)
	ahead.deferRenewedLease(run.ID)
	for deadline := time.Now().Add(10 * time.Second); (deferredLeases(ahead) != 0 || f.leaseFramesFor(run.ID) == before) && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}
	if n := deferredLeases(ahead); n != 0 || f.leaseFramesFor(run.ID) == before {
		t.Fatalf("the deferred lease: %d pending, %d new frame(s); want it sent through the holder", n, f.leaseFramesFor(run.ID)-before)
	}

	inTime := f.skewedReplica(t, 0)
	f.skew.Store(int64(time.Minute)) // now the holder's clock reads the stamp as lapsed
	before = f.leaseFramesFor(run.ID)
	if err := inTime.renewRunnerLease(f.ctx, run, 0); !errors.Is(err, errRunnerLeaseLapsed) {
		t.Fatalf("a lease the holder's clock reads lapsed, asked by a replica whose clock accepts it = %v, want errRunnerLeaseLapsed", err)
	}
	if n := f.leaseFramesFor(run.ID); n != before {
		t.Fatalf("the runner got %d LEASE frame(s) the holder refused", n-before)
	}
}

// R23 (api, the door's case): an output request is refused not_live_session unless the runner's fresh holder row
// names the sender and the token's session, and no_sink for a run with no sink on this replica, each with its class;
// a bad mac carries no class. The first-frame and lease cases are pinned at the door's Substrate end
// (TestPeerOutputOpenForwardedToLauncher, TestPeerOutputSinkYieldsToFreshLease).
func TestPeerDoorOutputRefusals_PG(t *testing.T) {
	f := newPeerFixture(t)
	s := f.session(t)
	f.waitHolder(t, s)
	self := f.srv.replicaName()
	good := remote.PeerToken{From: self, To: self, Runner: f.rn.ID, Session: s, Method: remote.PeerMethodOutput,
		Exp: time.Now().Add(10 * time.Second).Unix(), Target: uuid.New()}
	cases := []struct {
		name  string
		edit  func(*remote.PeerToken)
		key   []byte
		class string
	}{
		{name: "sender is not the holder", edit: func(t *remote.PeerToken) { t.From = "another" }, key: peerTestKey, class: peerRefusedNotLiveSession},
		{name: "not the holder's session", edit: func(t *remote.PeerToken) { t.Session = uuid.New() }, key: peerTestKey, class: peerRefusedNotLiveSession},
		{name: "a runner with no holder row", edit: func(t *remote.PeerToken) { t.Runner = uuid.New() }, key: peerTestKey, class: peerRefusedNotLiveSession},
		{name: "no sink for the run here", key: peerTestKey, class: peerRefusedNoSink},
		{name: "bad mac", key: bytes.Repeat([]byte{3}, 32)},
	}
	for _, c := range cases {
		tok := good
		if c.edit != nil {
			c.edit(&tok)
		}
		w := httptest.NewRecorder()
		f.srv.InternalHandler().ServeHTTP(w, peerRequest(tok, c.key))
		if w.Code != http.StatusForbidden || w.Header().Get(remote.HeaderPeerRefused) != c.class {
			t.Errorf("%s: %d, class %q; want 403, class %q", c.name, w.Code, w.Header().Get(remote.HeaderPeerRefused), c.class)
		}
	}
}

// onlineFrom is the Config shape whose RunnerOnline is the server's own answer, once srv holds it.
func onlineFrom(srv *atomic.Pointer[Server]) func(*Config) {
	return func(c *Config) {
		c.RunnerOnline = func(id uuid.UUID) bool { s := srv.Load(); return s != nil && s.RunnerOnline(id) }
	}
}

// nonHolder is a second replica on f's database that does not hold the runner's session, booted as a replica
// boots (RegisterRunnerSubstrates): an endpoint for each claimed runner, and its holder loop running, so its view
// is read at once and then every 5 s.
func (f *peerFixture) nonHolder(t *testing.T) *Server {
	t.Helper()
	b := f.replicaServer(t)
	if err := b.RegisterRunnerSubstrates(f.ctx); err != nil {
		t.Fatal(err)
	}
	return b
}

// replicaServer is a second server on f's database: its own orchestrator, audit, watcher lease owner, peer config
// and door, with shape applied to its config. Its holder loop is not started.
func (f *peerFixture) replicaServer(t *testing.T, shape ...func(*Config)) *Server {
	t.Helper()
	cfg := f.srv.cfg
	for _, sh := range shape {
		sh(&cfg)
	}
	cfg.Runner = orchestrator.New(runnertest.NewFake())
	cfg.Audit = &syncAudit{}
	pc := newPeerConfig()
	cfg.RunnerPeer = pc
	var self atomic.Pointer[Server]
	onlineFrom(&self)(&cfg)
	b := New(cfg)
	b.owner = "replica-b/" + uuid.NewString()
	self.Store(b)
	serveDoor(t, b, pc)
	return b
}

// hasEndpoint reports whether srv has an endpoint for runner id, without making one.
func hasEndpoint(srv *Server, id uuid.UUID) bool {
	srv.runnerStreams.subsMu.Lock()
	defer srv.runnerStreams.subsMu.Unlock()
	_, ok := srv.runnerStreams.subs[id]
	return ok
}

// peerWithin polls cond every 20 ms until it holds or d passes.
func peerWithin(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("not within %v: %s", d, what)
		}
	}
}

// onlineAnswers is a replica's three online reads for runner id: RunnerOnline, the runner view's Online, and
// whether its orchestrator advertises the runner's facts.
func (f *peerFixture) onlineAnswers(t *testing.T, srv *Server) [3]bool {
	t.Helper()
	row, err := f.st.GetRunner(f.ctx, f.rn.ID)
	if err != nil {
		t.Fatal(err)
	}
	facts, ok := srv.runnerFacts(f.rn.ID)
	return [3]bool{srv.RunnerOnline(f.rn.ID), srv.runnerView(f.ctx, row, types.RunnerTally{}, true).Online, ok && facts.CapacityAdvertised}
}

// R14: a runner online on its holder reads online the same way on another replica, and offline there within one
// refresh of its session's end.
func TestOnlineSameOnEveryReplica_PG(t *testing.T) {
	var a atomic.Pointer[Server]
	f := newPeerFixture(t, onlineFrom(&a))
	a.Store(f.srv)
	f.waitHolder(t, f.session(t))
	b := f.nonHolder(t)
	all := [3]bool{true, true, true}
	peerWithin(t, 6*time.Second, "the holder and the other replica both read the runner online", func() bool {
		return f.onlineAnswers(t, f.srv) == all && f.onlineAnswers(t, b) == all
	})
	f.disconnect()
	peerWithin(t, 6*time.Second, "the other replica reads the runner offline after its session ended", func() bool {
		got := f.onlineAnswers(t, b)
		return !got[0] && !got[1]
	})
}

// placedOnHolder is a RUNNING task run placed and started on the holder's runner, as R15 and R17 exec into.
func (f *peerFixture) placedOnHolder(t *testing.T) types.AgentRun {
	t.Helper()
	run := f.localRun(t, 0)
	f.running(t, &run, nil)
	f.placed(t, &run)
	return run
}

// R15: an exec started on a replica that does not hold the runner's session reaches the runner through the holder,
// and its output reaches that replica's caller.
func TestExecFromNonHolderReplica_PG(t *testing.T) {
	f := newPeerFixture(t)
	s := f.session(t)
	f.waitHolder(t, s)
	run := f.placedOnHolder(t)
	b := f.nonHolder(t)
	peerWithin(t, 6*time.Second, "the other replica reads the runner online", func() bool { return b.RunnerOnline(f.rn.ID) })
	ctx, cancel := context.WithTimeout(f.ctx, 20*time.Second)
	defer cancel()
	es, err := b.cfg.Runner.ExecStream(ctx, run.SandboxRef, runner.ExecSpec{Argv: []string{"cat"}, TTY: true})
	if err != nil {
		t.Fatalf("an exec from the other replica = %v, want it started through the holder", err)
	}
	if _, err := io.WriteString(es.Stdin, "through the holder\n"); err != nil {
		t.Fatal(err)
	}
	_ = es.Stdin.Close()
	out, err := io.ReadAll(es.Stdout)
	if err != nil || string(out) != "through the holder\n" {
		t.Fatalf("the exec's output at the other replica = %q, %v; want the bytes the runner echoed", out, err)
	}
	if !f.fake.Called("exec_stream", "fake-"+run.ID.String()) {
		t.Fatal("the runner saw no exec")
	}
	if got := f.session(t); got != s {
		t.Fatalf("the exec moved the runner to session %s, want it on %s", got, s)
	}
}

// R17: an attach from a replica that does not hold the runner's session carries bytes both ways on the session it
// was opened on, and once the runner moves, the stream ends and its controls answer offline.
func TestAttachStreamFromNonHolderReplica_PG(t *testing.T) {
	f := newPeerFixture(t)
	f.waitHolder(t, f.session(t))
	run := f.placedOnHolder(t)
	b := f.nonHolder(t)
	peerWithin(t, 6*time.Second, "the other replica reads the runner online", func() bool { return b.RunnerOnline(f.rn.ID) })
	ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
	defer cancel()
	sess, err := b.cfg.Runner.Attach(ctx, run.SandboxRef, runner.AttachOptions{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("an attach from the other replica = %v, want it opened through the holder", err)
	}
	defer sess.Close()
	if _, err := io.WriteString(sess, "ping"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(sess, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("the attach read %q, %v; want the runner's echo", buf, err)
	}
	f.disconnect()
	f.disconnect = f.connect(t)
	ended := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(sess)
		ended <- err
	}()
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the attach stream did not end after the runner moved")
	}
	if err := sess.Resize(ctx, 100, 30); !isOffline(err) {
		t.Fatalf("a resize after the move = %v, want offline", err)
	}
}

// capsAnswers is a replica's four caps reads for runner id, as one comparable value.
func capsAnswers(ctx context.Context, srv *Server, id uuid.UUID) (string, bool) {
	capacity, classes, kinds := srv.runnerReportedCaps(id)
	stream, err := srv.runnerStreamCapabilities(ctx, id)
	binds, roots, bound := srv.runnerBoundFolderCaps(id)
	facts, factsOK := srv.runnerFacts(id)
	if capacity == nil || err != nil || !bound || !factsOK {
		return "", false
	}
	b, _ := json.Marshal([]any{*capacity, classes, kinds, fmt.Sprintf("%+v", stream), binds, roots, facts.Classes})
	return string(b), true
}

// countRefreshes counts b's successful view refreshes from now on; set it before b's holder loop starts.
func countRefreshes(b *Server) *atomic.Int32 {
	n := new(atomic.Int32)
	view := b.runnerPeering().view
	route := view.OnRefresh
	view.OnRefresh = func(ctx context.Context, elsewhere []uuid.UUID) {
		route(ctx, elsewhere)
		n.Add(1)
	}
	return n
}

// claimAsOwner claims rn on srv as its owner does, through the claim handler under the owner's session.
func claimAsOwner(t *testing.T, srv *Server, rn types.Runner) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/me/runners/"+rn.ID.String()+"/claim", strings.NewReader(`{"fingerprint":"`+rn.KeyFingerprint+`"}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", rn.ID.String())
	r = r.WithContext(context.WithValue(withOIDCHuman(r.Context(), rn.Owner), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	srv.handleRunnerClaim(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("claim = %d %s", w.Code, w.Body.String())
	}
}

// R22: a runner claimed after a replica booted is routed there by a later refresh, with the holder's caps; a runner
// whose holder row is fresh but whose stored state is revoked gets no endpoint.
func TestRunnerClaimedAfterBootRoutedOnNonHolderReplica_PG(t *testing.T) {
	f := newPeerFixture(t)
	f.waitHolder(t, f.session(t))
	// A runner registered but not yet claimed when the other replica boots.
	rn, key := registeredLeaseRunner(t, f.ctx, f.st)

	// The other replica boots as a replica boots, and its first refresh is in place.
	b := f.replicaServer(t)
	refreshes := countRefreshes(b)
	if err := b.RegisterRunnerSubstrates(f.ctx); err != nil {
		t.Fatal(err)
	}
	peerWithin(t, 5*time.Second, "the other replica's first refresh", func() bool { return refreshes.Load() >= 1 })
	if _, ok := b.runnerPeering().view.Holder(rn.ID); ok || hasEndpoint(b, rn.ID) {
		t.Fatal("the other replica routed a runner nobody had claimed")
	}
	initial := refreshes.Load()

	// Its owner claims it on the first replica, and it connects there.
	claimAsOwner(t, f.srv, rn)
	side := runnerProcessFor(t, f.ctx, f.st, f.srv, rn, key)
	side.disconnect = side.connect(t)
	peerWithin(t, 10*time.Second, "the new runner's holder row names the first replica", func() bool {
		r, ok, err := f.st.RunnerSessionHolder(f.ctx, rn.ID)
		return err == nil && ok && r.ReplicaID == f.srv.replicaName()
	})

	// A second runner, revoked in the store, whose holder row names the holder.
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	id2 := uuid.New()
	r2, err := f.st.CreateRunner(f.ctx, types.Runner{ID: id2, Owner: f.rn.Owner, Name: "revoked", PublicKey: pub, KeyFingerprint: id2.String(),
		Version: f.rn.Version, OrgURLSHA256: f.rn.OrgURLSHA256, Limits: f.rn.Limits})
	if err != nil {
		t.Fatal(err)
	}
	if r2, err = f.st.ClaimRunner(f.ctx, r2.ID, r2.Owner, r2.KeyFingerprint, time.Now()); err != nil {
		t.Fatal(err)
	}
	s2 := uuid.New()
	if err := f.st.BeginRunnerInventory(f.ctx, r2.ID, s2.String()); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.st.ClaimRunnerSessionHolder(f.ctx, r2.ID, s2, f.srv.replicaName(), f.pc.URL); err != nil || !ok {
		t.Fatalf("the second runner's holder row: %v %v", ok, err)
	}
	if _, err := f.st.RevokeRunner(f.ctx, r2.ID, time.Now(), "admin", "test"); err != nil {
		t.Fatal(err)
	}

	peerWithin(t, 6*time.Second, "a refresh after the claim routes the new runner on the other replica", func() bool {
		return hasEndpoint(b, rn.ID) && refreshes.Load() > initial
	})
	want, ok := capsAnswers(f.ctx, f.srv, rn.ID)
	if !ok {
		t.Fatal("the holder has no caps for the new runner")
	}
	peerWithin(t, 6*time.Second, "the other replica answers the holder's caps", func() bool {
		got, ok := capsAnswers(f.ctx, b, rn.ID)
		return ok && got == want
	})
	time.Sleep(6 * time.Second) // one more refresh
	if hasEndpoint(b, r2.ID) {
		t.Fatal("the refresh made an endpoint for a revoked runner")
	}
}

// A claimed runner whose session was lost before a replica started, and which connects again after it, is routed
// there by a later refresh, with the holder's caps.
func TestRunnerRediscoveredAfterLostSessionOnNonHolderReplica_PG(t *testing.T) {
	f := newPeerFixture(t)
	f.waitHolder(t, f.session(t))
	// The runner leaves: no session, and no holder row, before the other replica starts.
	f.disconnect()
	peerWithin(t, 10*time.Second, "the holder's link to the runner is down", func() bool {
		return !f.srv.runnerEndpoint(f.rn.ID).link.HeldHere()
	})
	if _, err := f.st.Pool.Exec(f.ctx, `DELETE FROM runner_session_holders WHERE runner_id=$1`, f.rn.ID); err != nil {
		t.Fatal(err)
	}

	// The other replica starts with no endpoint for it, and its first refresh finds no holder row.
	b := f.replicaServer(t)
	refreshes := countRefreshes(b)
	b.runnerHub(f.st) // its holder loop: the first refresh at once, then every 5 s
	peerWithin(t, 5*time.Second, "the other replica's first refresh", func() bool { return refreshes.Load() >= 1 })
	if _, ok := b.runnerPeering().view.Holder(f.rn.ID); ok || hasEndpoint(b, f.rn.ID) {
		t.Fatal("the other replica routed a runner with no holder row")
	}
	initial := refreshes.Load()

	// The runner connects again on the holder after the other replica's start.
	f.disconnect = f.connect(t)
	f.waitHolder(t, f.session(t))
	peerWithin(t, 6*time.Second, "a refresh after the reconnect routes the runner on the other replica", func() bool {
		return hasEndpoint(b, f.rn.ID) && refreshes.Load() > initial
	})
	want, ok := capsAnswers(f.ctx, f.srv, f.rn.ID)
	if !ok {
		t.Fatal("the holder has no caps for its runner")
	}
	peerWithin(t, 6*time.Second, "the other replica answers the holder's caps", func() bool {
		got, ok := capsAnswers(f.ctx, b, f.rn.ID)
		return ok && got == want
	})
}

// revokeAndWait revokes runner id on by and waits up to d until neither replica has an endpoint or an orchestrator
// substrate for it.
func revokeAndWait(t *testing.T, f *peerFixture, by *Server, d time.Duration, replicas ...*Server) {
	t.Helper()
	if _, _, err := by.revokeRunner(f.ctx, f.rn.ID, types.ActorHuman, "admin", "test"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	peerWithin(t, d, "no replica keeps an endpoint for the revoked runner", func() bool {
		for _, srv := range replicas {
			if _, facts := srv.runnerFacts(f.rn.ID); hasEndpoint(srv, f.rn.ID) || facts {
				return false
			}
		}
		return true
	})
}

// R22b: a revoked runner's endpoint leaves every replica that saw it, the holder's included; a runner whose session
// only ended keeps its endpoint everywhere.
func TestRevokedRunnerUnroutedOnNonHolderReplica_PG(t *testing.T) {
	t.Run("a session that only ended keeps its endpoints, a revoke on the holder removes them", func(t *testing.T) {
		f := newPeerFixture(t)
		f.waitHolder(t, f.session(t))
		b := f.nonHolder(t)
		peerWithin(t, 6*time.Second, "the other replica routes the runner", func() bool { return hasEndpoint(b, f.rn.ID) && b.RunnerOnline(f.rn.ID) })
		f.disconnect()
		peerWithin(t, 6*time.Second, "the other replica reads the ended session offline", func() bool { return !b.RunnerOnline(f.rn.ID) })
		time.Sleep(6 * time.Second) // one more refresh
		if !hasEndpoint(f.srv, f.rn.ID) || !hasEndpoint(b, f.rn.ID) {
			t.Fatal("a runner whose session only ended lost an endpoint")
		}
		f.disconnect = f.connect(t)
		f.waitHolder(t, f.session(t))
		peerWithin(t, 6*time.Second, "the other replica reads the runner online again", func() bool { return b.RunnerOnline(f.rn.ID) })
		revokeAndWait(t, f, f.srv, 6*time.Second, f.srv, b)
	})
	t.Run("a revoke through the other replica removes both", func(t *testing.T) {
		f := newPeerFixture(t)
		f.waitHolder(t, f.session(t))
		b := f.nonHolder(t)
		peerWithin(t, 6*time.Second, "the other replica routes the runner", func() bool { return hasEndpoint(b, f.rn.ID) && b.RunnerOnline(f.rn.ID) })
		revokeAndWait(t, f, b, 21*time.Second, f.srv, b)
	})
}

// R23e (OR37-7): two processes stamp one run's lease. The holder's process keeps its sink at every check, whoever
// stamped last; the other keeps it only while its own stamp is the fresh one; each reads sole only then.
func TestPeerOutputLeaseTwoStampers_PG(t *testing.T) {
	f := newPeerFixture(t)
	f.waitHolder(t, f.session(t))
	b := f.nonHolder(t)
	pa, pb := f.srv.runnerPeering(), b.runnerPeering()
	peerWithin(t, 6*time.Second, "the other replica's view names the holder", func() bool {
		h, ok := pb.view.Holder(f.rn.ID)
		return ok && h.ReplicaID == f.srv.replicaName()
	})
	run := f.localRun(t, 0)
	f.running(t, &run, nil)
	leaseA := pa.outputLease(f.rn.ID, f.srv.runnerEndpoint(f.rn.ID).link)
	leaseB := pb.outputLease(f.rn.ID, b.runnerEndpoint(f.rn.ID).link)
	for i := range 4 {
		aStamps := i%2 == 0
		stamper := b
		if aStamps {
			stamper = f.srv
		}
		stamper.stampRunWatcherLease(f.ctx, run.ID)
		pa.Refresh(f.ctx) // both replica rows fresh
		pb.Refresh(f.ctx)
		ka, sa, errA := leaseA(f.ctx, run.ID)
		kb, sb, errB := leaseB(f.ctx, run.ID)
		if errA != nil || errB != nil {
			t.Fatalf("check %d: lease reads %v, %v", i+1, errA, errB)
		}
		if !ka || sa != aStamps || kb == aStamps || sb == aStamps {
			t.Fatalf("check %d (holder stamped last: %v): holder keep=%v sole=%v, other keep=%v sole=%v", i+1, aStamps, ka, sa, kb, sb)
		}
	}
}

// blockingSink is an org-side output writer whose first write is recorded and then held until the test ends, so the
// run's sink on the Substrate stays open.
type blockingSink struct {
	mu   sync.Mutex
	b    bytes.Buffer
	hold chan struct{}
}

func (w *blockingSink) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.b.Write(p)
	w.mu.Unlock()
	<-w.hold
	return len(p), nil
}

func (w *blockingSink) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// dialDoor opens the door over TLS under tok and serves it as a runner-side peer.
func (f *peerFixture) dialDoor(t *testing.T, ctx context.Context, tok remote.PeerToken) *runnerwire.Peer {
	t.Helper()
	h := http.Header{}
	tok.SetHeaders(h, peerTestKey)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: hopRootsPool(peerTestCA().CertPEM), ServerName: peerTestHost, MinVersion: tls.VersionTLS13}}}
	c, _, err := websocket.Dial(ctx, f.ts.URL+remote.PeerPath, &websocket.DialOptions{HTTPClient: client, HTTPHeader: h, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		t.Fatalf("the door refused an output request for a run with a sink: %v", err)
	}
	p := runnerwire.NewPeer(runnerwire.NewWebSocket(c), runnerwire.PeerConfig{})
	go func() { _ = p.Run(ctx) }()
	t.Cleanup(p.Close)
	return p
}

// peerRefusals is how many refusals of class the server counted since that class's last Warn line.
func peerRefusals(srv *Server, class string) int {
	srv.peering.mu.Lock()
	defer srv.peering.mu.Unlock()
	return srv.peering.refused[class]
}

// R23 (api, the door's upgraded case): for a run with a sink here, a first frame that is a call, or an OPEN for
// another target, is refused wrong_method after the upgrade and reaches no sink; once another process off the holder
// holds the run's lease fresh, the door answers no_sink.
func TestPeerDoorOutputRefusalsAfterUpgrade_PG(t *testing.T) {
	f := newPeerFixture(t)
	s := f.session(t)
	f.waitHolder(t, s)
	sink := &blockingSink{hold: make(chan struct{})}
	t.Cleanup(func() { close(sink.hold) })
	f.execOutput = sink
	run := f.placedOnHolder(t)
	first := "agent output of fake-" + run.ID.String() + "\n"
	peerWithin(t, 10*time.Second, "the run's sink receives the runner's output", func() bool { return sink.String() == first })
	self := f.srv.replicaName()
	tok := remote.PeerToken{From: self, To: self, Runner: f.rn.ID, Session: s, Method: remote.PeerMethodOutput,
		Exp: time.Now().Add(30 * time.Second).Unix(), Target: run.ID}
	ctx, cancel := context.WithTimeout(f.ctx, 20*time.Second)
	defer cancel()
	logs := captureSlog(t)

	p := f.dialDoor(t, ctx, tok)
	if err := p.Call(ctx, runnerwire.MethodStatus, map[string]string{"ref": "x"}, nil); err == nil {
		t.Fatal("a call as the first frame was answered")
	}
	peerWithin(t, 5*time.Second, "a call as the first frame is refused wrong_method", func() bool {
		return strings.Contains(logs.String(), "class="+peerRefusedWrongMethod)
	})
	p = f.dialDoor(t, ctx, tok)
	if _, err := p.Open(ctx, runnerwire.Open{Kind: runnerwire.KindOutput, Target: uuid.NewString()}); err == nil {
		t.Fatal("an OPEN for another target was accepted")
	}
	peerWithin(t, 5*time.Second, "an OPEN for another target is refused wrong_method", func() bool {
		return peerRefusals(f.srv, peerRefusedWrongMethod) == 1
	})
	if got := sink.String(); got != first {
		t.Fatalf("the run's sink holds %q, want only the runner's own output", got)
	}

	if err := f.st.TouchRunnerPeerReplica(f.ctx, "replica-other/p", "replica-other", "https://other.invalid"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(f.ctx, `UPDATE agent_runs SET watcher_owner='replica-other/p', watcher_heartbeat=now() WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	f.srv.InternalHandler().ServeHTTP(w, peerRequest(tok, peerTestKey))
	if w.Code != http.StatusForbidden || w.Header().Get(remote.HeaderPeerRefused) != peerRefusedNoSink {
		t.Fatalf("a run whose lease another process holds fresh: %d, class %q; want 403 %s", w.Code, w.Header().Get(remote.HeaderPeerRefused), peerRefusedNoSink)
	}
}

// launchOn is launchVia with l's lease hold kept until the test ends.
func launchOn(t *testing.T, l *Server, rp *runnerProcess) types.AgentRun {
	t.Helper()
	run, stop := launchVia(t, l, rp)
	t.Cleanup(stop)
	return run
}

// launchVia is a task run on rp's runner launched by l, as dispatch does at the substrate: its row is seeded RUNNING
// (setup, not a state change under test), l holds the run's watcher lease, leases and creates the run on the
// runner with l's output sink, starts its agent under G1 and persists the agent's exec id. stop ends l's lease
// hold, as dispatch's return does; nothing stamps the lease after that.
func launchVia(t *testing.T, l *Server, rp *runnerProcess) (types.AgentRun, func()) {
	t.Helper()
	ctx := rp.ctx
	run := newFinalizeRun()
	run.CreatedBy, run.Placement, run.RunnerID, run.State = rp.rn.Owner, types.PlacementLocal, &rp.rn.ID, types.RunRunning
	run, err := rp.st.CreateRun(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if err := rp.st.RaiseRunDataOut(ctx, run.ID, types.TransferAllowed); err != nil {
		t.Fatal(err)
	}
	if err := rp.st.SetRunRunnerTrustClass(ctx, run.ID, types.RunnerTrustPersonal); err != nil {
		t.Fatal(err)
	}
	if err := rp.st.InsertPendingRunOutput(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := rp.st.Pool.Exec(ctx, `UPDATE agent_runs SET token_renewed_at = now() WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	stop := sync.OnceFunc(l.holdRunWatcherLease(ctx, run.ID))
	t.Cleanup(stop)
	e, err := l.leaseRunOnRunner(ctx, run)
	if err != nil {
		t.Fatalf("lease on runner %s from %s: %v", rp.rn.ID, l.replicaName(), err)
	}
	sb, err := l.createSandboxAtTarget(ctx, run, runner.SandboxSpec{RunID: run.ID, ConfinementClass: types.CC1, ExecOutput: l.openExecOutput(run, false)})
	if err != nil {
		t.Fatalf("create on runner %s from %s: %v", rp.rn.ID, l.replicaName(), err)
	}
	run.SandboxRef = sb.Ref
	if err := rp.st.SetSandboxRef(ctx, run.ID, sb.Ref); err != nil {
		t.Fatal(err)
	}
	execID, err := l.cfg.Runner.Exec(e.bind(ctx), run.SandboxRef, []string{"/usr/local/bin/agent-run", "t"})
	if err == nil {
		err = l.bindRunnerExecution(ctx, e, execID)
	}
	if err == nil {
		err = rp.st.SetRunAgentExecID(ctx, run.ID, cmp.Or(execID, mainProcessExecID))
	}
	if err != nil {
		t.Fatal(err)
	}
	run.AgentExecID = cmp.Or(execID, mainProcessExecID)
	return run, stop
}

// attachEcho attaches to run through srv and reads back what it writes.
func attachEcho(t *testing.T, ctx context.Context, srv *Server, run types.AgentRun, msg string) {
	t.Helper()
	sess, err := srv.cfg.Runner.Attach(ctx, run.SandboxRef, runner.AttachOptions{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("an attach to %s through %s = %v", run.SandboxRef, srv.replicaName(), err)
	}
	defer sess.Close()
	if _, err := io.WriteString(sess, msg); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(sess, buf); err != nil || string(buf) != msg {
		t.Fatalf("the attach through %s read %q, %v; want the runner's echo %q", srv.replicaName(), buf, err, msg)
	}
}

// R18: two runners, each held by a different replica, launch and attach through the other replica. Each run reaches
// its own runner, each stream carries bytes, each run's output reaches its stored output through the replica that
// launched it, and neither runner sees the other's call.
func TestTwoRunnersEachServedThroughOtherReplica_PG(t *testing.T) {
	f := newPeerFixture(t)
	f.waitHolder(t, f.session(t))
	b := f.nonHolder(t)
	r2 := newRunnerSide(t, f.ctx, f.st, b) // R2's session is held by B
	peerWithin(t, 10*time.Second, "R2's holder row names B", func() bool {
		r, ok, err := f.st.RunnerSessionHolder(f.ctx, r2.rn.ID)
		return err == nil && ok && r.ReplicaID == b.replicaName()
	})
	peerWithin(t, 6*time.Second, "each replica reads the other's runner online", func() bool {
		return b.RunnerOnline(f.rn.ID) && f.srv.RunnerOnline(r2.rn.ID) && hasEndpoint(f.srv, r2.rn.ID)
	})

	run1 := launchOn(t, b, f.runnerProcess) // R1, held by A, launched through B
	run2 := launchOn(t, f.srv, r2)          // R2, held by B, launched through A
	ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
	defer cancel()
	attachEcho(t, ctx, b, run1, "one")
	attachEcho(t, ctx, f.srv, run2, "two")

	for _, c := range []struct {
		run  types.AgentRun
		own  *runnertest.Fake
		peer *runnertest.Fake
	}{{run1, f.fake, r2.fake}, {run2, r2.fake, f.fake}} {
		for _, verb := range []string{"exec", "attach"} {
			if !c.own.Called(verb, c.run.SandboxRef) {
				t.Errorf("run %s: its runner saw no %s", c.run.ID, verb)
			}
			if c.peer.Called(verb, c.run.SandboxRef) {
				t.Errorf("run %s: the other runner saw its %s", c.run.ID, verb)
			}
		}
		want := "agent output of " + c.run.SandboxRef + "\n"
		peerWithin(t, 10*time.Second, "run "+c.run.ID.String()+"'s output reaches its stored output", func() bool {
			ch, err := f.st.ReadRunOutputChunks(f.ctx, c.run.ID, 1<<20)
			return err == nil && string(ch.Bytes) == want
		})
	}
	if b.tailFor(run1.ID) == nil || f.srv.tailFor(run2.ID) == nil {
		t.Fatal("a launching replica holds no tail for its run")
	}
}

// placeOn is the create path's placement decision (resolvePlacement) on srv for a background run its owner launches
// on rp's runner from the owner's own pool, with the request a launch sends. It must choose that runner with no
// refusal code.
func placeOn(t *testing.T, srv *Server, rp *runnerProcess) {
	t.Helper()
	pool, err := rp.st.CreateRunnerPool(rp.ctx, types.RunnerPool{Name: "Own runner", Scope: types.RunnerPoolUser, Owner: rp.rn.Owner}, rp.rn.Owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := rp.st.AddOwnRunnerToPool(rp.ctx, pool.ID, rp.rn.ID, rp.rn.Owner, rp.rn.OrgURLSHA256); err != nil {
		t.Fatal(err)
	}
	ctx := operatorCtx(rp.rn.Owner, rp.rn.Owner+"@corp.example", oidc.RoleUser)
	ceiling, err := srv.effectiveCeiling(ctx)
	if err != nil {
		t.Fatal(err)
	}
	req := createRunRequest{Agent: "claude-code", Placement: types.PlacementLocal, RunnerPoolID: pool.ID.String(), Experience: client.ExperienceBackground}
	plan, refusal := srv.resolvePlacement(ctx, placementInput{req: &req, reqCCOK: true, ceiling: ceiling})
	if refusal != nil {
		t.Fatalf("placement on %s refused: %d %+v", srv.replicaName(), refusal.status, refusal.body)
	}
	if plan.placement != types.PlacementLocal || !slices.ContainsFunc(plan.eligible, func(p poolTarget) bool { return p.runnerID != nil && *p.runnerID == rp.rn.ID }) {
		t.Fatalf("placement on %s chose %s %+v, want the runner %s", srv.replicaName(), plan.placement, plan.eligible, rp.rn.ID)
	}
}

// outputVia is GET /runs/{id}/output on srv as the run's owner; a refusal answers its status as the output, so a
// poll never matches it.
func outputVia(t *testing.T, srv *Server, run types.AgentRun) runOutputResponse {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/output", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", run.ID.String())
	r = r.WithContext(context.WithValue(operatorCtx(run.CreatedBy, run.CreatedBy+"@corp.example", oidc.RoleUser), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	srv.handleRunOutput(w, r)
	var out runOutputResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		return runOutputResponse{Output: fmt.Sprintf("status %d: %s", w.Code, w.Body)}
	}
	return out
}

// tailText is what srv's tail for run holds, and false when srv holds none.
func tailText(srv *Server, run uuid.UUID) (string, bool) {
	e := srv.tailFor(run)
	if e == nil {
		return "", false
	}
	e.mw.mu.Lock()
	defer e.mw.mu.Unlock()
	return string(e.ring.buf), true
}

// servable is whether srv's Substrate for runner id has a sink for run that the lease rule serves.
func servable(ctx context.Context, srv *Server, id uuid.UUID, run uuid.UUID) bool {
	return srv.runnerEndpoint(id).sub.ServableOutput(ctx, run)
}

// launcherFresh fails the test unless run's watcher lease is l's and fresh (LeaseFresh) and l's replica row is fresh,
// so a forward reads l from RunOutputLauncher.
func launcherFresh(t *testing.T, st store.PG, ctx context.Context, run uuid.UUID, l *Server, at string) {
	t.Helper()
	row, err := st.RunOutputLauncher(ctx, run)
	if err != nil || row.State != store.LauncherFresh || !row.LeaseFresh || row.WatcherOwner != l.leaseOwner() || row.ReplicaID != l.replicaName() {
		t.Fatalf("%s: launcher %+v %v, want %s's fresh lease and replica row", at, row, err, l.replicaName())
	}
}

// heldOutput is the runner-side writer of run's create, which the fake's OutputHold keeps open after part 1.
func heldOutput(t *testing.T, fake *runnertest.Fake, run uuid.UUID) io.Writer {
	t.Helper()
	for _, spec := range fake.Created {
		if spec.RunID == run && spec.ExecOutput != nil {
			return spec.ExecOutput
		}
	}
	t.Fatalf("the runner holds no output writer for run %s", run)
	return nil
}

// holdOutput makes fake's creates keep their output drain open until the test ends.
func holdOutput(t *testing.T, fake *runnertest.Fake) {
	hold := make(chan struct{})
	fake.OutputHold = hold
	t.Cleanup(func() { close(hold) })
}

// leaseSent reports whether the runner accepted a LEASE frame for run.
func leaseSent(rp *runnerProcess, run uuid.UUID) bool {
	rp.leaseMu.Lock()
	defer rp.leaseMu.Unlock()
	_, ok := rp.leaseEnds[run]
	return ok
}

// R16: a runner claimed after a replica booted is chosen by that replica's placement, launched there, and keeps its
// output at that replica across a reconnect to the holder: the re-open on the holder reaches the launcher's sink.
func TestLaunchPlacedFromNonHolderReplica_PG(t *testing.T) {
	f := newPeerFixture(t)
	f.waitHolder(t, f.session(t))
	rn, key := registeredLeaseRunner(t, f.ctx, f.st)
	b := f.nonHolder(t) // B boots before the runner is claimed, so it makes no endpoint for it at boot
	if hasEndpoint(b, rn.ID) {
		t.Fatal("B made an endpoint at boot for a runner nobody had claimed")
	}
	claimAsOwner(t, f.srv, rn)
	side := runnerProcessFor(t, f.ctx, f.st, f.srv, rn, key)
	holdOutput(t, side.fake)
	side.disconnect = side.connect(t)
	peerWithin(t, 10*time.Second, "B routes the runner with the holder's caps", func() bool {
		_, ok := capsAnswers(f.ctx, b, rn.ID)
		return hasEndpoint(b, rn.ID) && b.RunnerOnline(rn.ID) && ok
	})

	placeOn(t, b, side)
	start := time.Now()
	run, stop := launchVia(t, b, side)
	stop() // dispatch returned: a runner run has no completion watcher, so nothing stamps B's lease again
	launcherFresh(t, f.st, f.ctx, run.ID, b, "after the launch")
	if !leaseSent(side, run.ID) {
		t.Fatal("the runner accepted no LEASE for the run")
	}
	if got, err := f.st.GetRun(f.ctx, run.ID); err != nil || got.State != types.RunRunning {
		t.Fatalf("the run is %v (%v), want it RUNNING", got.State, err)
	}
	part1 := "agent output of " + run.SandboxRef + "\n"
	peerWithin(t, 10*time.Second, "part 1 is read through B", func() bool { return outputVia(t, b, run).Output == part1 })
	launcherFresh(t, f.st, f.ctx, run.ID, b, "at part 1")

	// The runner's link to A drops and comes back on a new connection.
	dropped := time.Now()
	side.disconnect()
	side.disconnect = side.connect(t)
	if d := time.Since(dropped); d >= 5*time.Second {
		t.Fatalf("the drop lasted %v, want under 5s", d)
	}
	part2 := "part 2 after the reconnect " + uuid.NewString() + "\n"
	if _, err := io.WriteString(heldOutput(t, side.fake, run.ID), part2); err != nil {
		t.Fatal(err)
	}
	peerWithin(t, 10*time.Second, "part 2 reaches B's sink through A's re-open", func() bool {
		text, ok := tailText(b, run.ID)
		return ok && strings.HasSuffix(text, part2)
	})
	launcherFresh(t, f.st, f.ctx, run.ID, b, "at part 2")
	if _, ok := tailText(f.srv, run.ID); ok {
		t.Fatal("A holds a tail for a run B launched")
	}
	if !servable(f.ctx, b, rn.ID, run.ID) || servable(f.ctx, f.srv, rn.ID, run.ID) {
		t.Fatal("the sink check should find a servable sink on B and none on A")
	}
	peerWithin(t, 10*time.Second, "the stored output through B is part 1 then part 2", func() bool {
		o := outputVia(t, b, run)
		return o.Output == part1+part2 && !o.CaptureGap && !o.Incomplete
	})
	launcherFresh(t, f.st, f.ctx, run.ID, b, "at the last assertion")
	if d := time.Since(start); d >= 90*time.Second {
		t.Fatalf("launch to last assertion took %v, want under 90s: the dispatch-time lease would be stale", d)
	}
}

// R16b: a run launched on B keeps its output at B while its runner moves from A to C: C forwards the re-open to B,
// which it reads from RunOutputLauncher, and B's dispatch-time lease is the one read.
func TestThreeReplicasOutputFollowsRunnerMove_PG(t *testing.T) {
	f := newPeerFixture(t)
	f.waitHolder(t, f.session(t))
	b, c := f.nonHolder(t), f.nonHolder(t)
	holdOutput(t, f.fake)
	peerWithin(t, 6*time.Second, "B and C read the runner online", func() bool { return b.RunnerOnline(f.rn.ID) && c.RunnerOnline(f.rn.ID) })

	start := time.Now()
	run, stop := launchVia(t, b, f.runnerProcess)
	stop() // dispatch returned
	part1 := "agent output of " + run.SandboxRef + "\n"
	peerWithin(t, 10*time.Second, "part 1 is read through B", func() bool { return outputVia(t, b, run).Output == part1 })

	// The runner's link to A ends and it connects to C.
	moved := time.Now()
	f.disconnect()
	f.disconnect = f.connectTo(t, c)
	peerWithin(t, 5*time.Second-time.Since(moved), "the runner's holder row names C", func() bool {
		r, ok, err := f.st.RunnerSessionHolder(f.ctx, f.rn.ID)
		return err == nil && ok && r.ReplicaID == c.replicaName()
	})
	launcherFresh(t, f.st, f.ctx, run.ID, b, "where C reads the launcher")
	part2 := "part 2 after the move " + uuid.NewString() + "\n"
	if _, err := io.WriteString(heldOutput(t, f.fake, run.ID), part2); err != nil {
		t.Fatal(err)
	}
	peerWithin(t, 10*time.Second, "part 2 reaches B's sink through C", func() bool {
		text, ok := tailText(b, run.ID)
		return ok && strings.HasSuffix(text, part2)
	})
	if _, ok := tailText(c, run.ID); ok || servable(f.ctx, c, f.rn.ID, run.ID) {
		t.Fatal("C holds a tail or a sink for a run B launched")
	}
	peerWithin(t, 10*time.Second, "the stored output through B is part 1 then part 2", func() bool {
		o := outputVia(t, b, run)
		return o.Output == part1+part2 && !o.CaptureGap && !o.Incomplete
	})
	launcherFresh(t, f.st, f.ctx, run.ID, b, "at the end")
	if d := time.Since(start); d >= 90*time.Second {
		t.Fatalf("launch to last assertion took %v, want under 90s", d)
	}
}

// outputStreams records, at the runner, the output streams it opens for each run and the ones the org reset as
// refused.
type outputStreams struct {
	mu      sync.Mutex
	opened  map[string][]uint32 // by Open target, in order
	refused map[uint32]bool
}

func (o *outputStreams) see(in bool, fr runnerwire.Frame) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case !in && fr.Type == runnerwire.TypeOpen:
		var op runnerwire.Open
		if runnerwire.Unmarshal(fr.Payload, &op) == nil && op.Kind == runnerwire.KindOutput {
			o.opened[op.Target] = append(o.opened[op.Target], fr.Stream)
		}
	case in && fr.Type == runnerwire.TypeReset:
		if code, err := runnerwire.DecodeReset(fr.Payload); err == nil && code == runnerwire.ResetRefused {
			o.refused[fr.Stream] = true
		}
	}
}

// firstRefused reports whether the first output stream the runner opened for target was reset as refused.
func (o *outputStreams) firstRefused(target string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	ids := o.opened[target]
	return len(ids) > 0 && o.refused[ids[0]]
}

// R16c: a Placement-local task run launched on B, which has no completion watcher, is adopted by the holder once its
// lease ages out: B's sink yields within one sink check, the runner sees its old stream refused, A holds the live
// capture, and B reads the stored chunks with each part once.
func TestPlacementLocalRunFromNonHolderReplicaAdopted_PG(t *testing.T) {
	f := newPeerFixture(t)
	f.waitHolder(t, f.session(t))
	streams := &outputStreams{opened: map[string][]uint32{}, refused: map[uint32]bool{}}
	f.disconnect()
	f.observe = streams.see
	f.disconnect = f.connect(t)
	f.waitHolder(t, f.session(t))
	b := f.replicaServer(t, func(c *Config) { c.RunnerOutputSinkCheck = 50 * time.Millisecond })
	if err := b.RegisterRunnerSubstrates(f.ctx); err != nil {
		t.Fatal(err)
	}
	holdOutput(t, f.fake)
	peerWithin(t, 6*time.Second, "B reads the runner online", func() bool { return b.RunnerOnline(f.rn.ID) })

	run, stop := launchVia(t, b, f.runnerProcess)
	stop() // B's dispatch hold ends when the launch returns
	part1 := "agent output of " + run.SandboxRef + "\n"
	peerWithin(t, 10*time.Second, "part 1 is read through B", func() bool { return outputVia(t, b, run).Output == part1 })

	// The lease ages out and A's sweep claims and adopts the run.
	if _, err := f.st.Pool.Exec(f.ctx, `UPDATE agent_runs SET watcher_heartbeat = now() - interval '91 seconds' WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.sweepRunWatchers(f.ctx); err != nil {
		t.Fatal(err)
	}
	peerWithin(t, 10*time.Second, "A holds the run's lease", func() bool {
		var owner *string
		err := f.st.Pool.QueryRow(f.ctx, `SELECT watcher_owner FROM agent_runs WHERE id = $1`, run.ID).Scan(&owner)
		return err == nil && owner != nil && *owner == f.srv.leaseOwner()
	})
	part2 := "part 2 after the adoption " + uuid.NewString() + "\n"
	if _, err := io.WriteString(heldOutput(t, f.fake, run.ID), part2); err != nil {
		t.Fatal(err)
	}

	// Within one sink check B yields: no sink, no tail; the runner's old stream was refused; A's live capture holds
	// the post-adoption part, and A stored it as its recovery's chunk.
	peerWithin(t, time.Second, "B's sink and tail are gone and the runner saw its old stream refused", func() bool {
		_, tail := tailText(b, run.ID)
		return !tail && !servable(f.ctx, b, f.rn.ID, run.ID) && streams.firstRefused(run.ID.String())
	})
	yielded := time.Now()
	peerWithin(t, 5*time.Second, "A's tail holds the part written after the adoption", func() bool {
		text, ok := tailText(f.srv, run.ID)
		return ok && strings.Contains(text, part2)
	})
	peerWithin(t, 5*time.Second, "A stored the post-adoption part as its recovery's chunk", func() bool {
		var n int
		err := f.st.Pool.QueryRow(f.ctx, `SELECT count(*) FROM run_output_chunks WHERE run_id = $1 AND replica = $2 AND position($3::bytea in bytes_masked) > 0`,
			run.ID, "recovered:"+f.srv.replicaName(), []byte(part2)).Scan(&n)
		return err == nil && n > 0
	})

	// B, with no tail, answers from the chunks.
	peerWithin(t, time.Second-time.Since(yielded), "B reads part 1 then part 2 from the chunks", func() bool {
		return outputVia(t, b, run).Output == part1+part2
	})
	part3 := "part 3 " + uuid.NewString() + "\n"
	if _, err := io.WriteString(heldOutput(t, f.fake, run.ID), part3); err != nil {
		t.Fatal(err)
	}
	peerWithin(t, time.Second, "B reads part 1 to part 3 once each, in order", func() bool {
		o := outputVia(t, b, run)
		return o.Output == part1+part2+part3 && !o.CaptureGap && !o.Incomplete
	})
	ch, err := f.st.ReadRunOutputChunks(f.ctx, run.ID, 1<<20)
	if err != nil || string(ch.Bytes) != part1+part2+part3 {
		t.Fatalf("stored chunks %q (%v), want part 1 to part 3 byte for byte", ch.Bytes, err)
	}
}
