// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/store"
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
