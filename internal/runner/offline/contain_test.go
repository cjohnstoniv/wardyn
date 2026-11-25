// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// putContainRequest writes a request into m's inbox as wardyn-runnerd contain does, and returns its id.
func putContainRequest(t *testing.T, m *Manager, req containRequest) uuid.UUID {
	t.Helper()
	inbox := filepath.Join(m.dir, "offline", containInbox)
	if err := os.Mkdir(inbox, 0700); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if err := os.WriteFile(filepath.Join(inbox, id.String()+containReqSuffix), b, 0600); err != nil {
		t.Fatal(err)
	}
	return id
}

// containTick runs the inbox, lapse and receipt steps of one Run tick.
func containTick(t *testing.T, m *Manager) error {
	t.Helper()
	m.applyContainRequests(t.Context())
	lapses := m.CheckLapses(t.Context())
	m.writeContainReceipts(lapses)
	return lapses
}

func readReceipt(t *testing.T, m *Manager, id uuid.UUID) containReceipt {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(m.dir, "offline", containInbox, id.String()+containDone))
	if err != nil {
		t.Fatalf("receipt: %v", err)
	}
	var rc containReceipt
	if err := json.Unmarshal(b, &rc); err != nil {
		t.Fatal(err)
	}
	return rc
}

// A latch is only ever set: a second request, a recover_containment and a restart all leave it; only retirement
// would clear it.
func TestContain_LatchIsMonotonic(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	at := time.Now().UTC().Truncate(time.Second)
	putContainRequest(t, m, containRequest{Runs: []uuid.UUID{id}, RequestedAt: at, UID: 1000})
	if err := containTick(t, m); err != nil {
		t.Fatal(err)
	}
	first := *m.state.Runs[id].LocalContain
	if !first.At.Equal(at) || first.Scope != "run" || first.UID != 1000 {
		t.Fatalf("latch = %+v", first)
	}
	putContainRequest(t, m, containRequest{All: true, RequestedAt: at.Add(time.Hour), UID: 0})
	if err := containTick(t, m); err != nil {
		t.Fatal(err)
	}
	if got := *m.state.Runs[id].LocalContain; got != first {
		t.Fatalf("a second request rewrote the latch: %+v, want %+v", got, first)
	}
	recover := pendingContainment(m, id, types.RunnerActionRecoverContainment)
	res, err := m.apply(t.Context(), recover)
	if err != nil || res.Outcome != types.RunnerActionRefused || !strings.Contains(res.Reason, "local operator contained") {
		t.Fatalf("recover_containment on a latched run = %+v, %v; want refused", res, err)
	}
	if m.state.Runs[id].LocalContain == nil {
		t.Fatal("a recover_containment cleared the latch")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(m.dir, m.runnerID, sub, uuid.UUID.String, noOutput, noErase)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.state.Runs[id].LocalContain; got == nil || *got != first {
		t.Fatalf("latch after a restart = %+v, want %+v", got, first)
	}
}

// With the organisation unreachable, a latched run's proxy stops on the next tick, its receipt says stopped, and the
// connection is told, so the next session is fresh. On that session a LEASE for the run is dropped without an error
// (the session stays up) and extends nothing, and the run's STATE row carries the latch. A request written while serve
// was down is applied when the manager starts.
func TestContain_StopsProxyOfflineAndDropsLease(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	other := createLocal(t, m, org)
	notified := 0
	m.SetOnLatch(func() { notified++ })
	m.Offline(t.Context())
	rid := putContainRequest(t, m, containRequest{Runs: []uuid.UUID{id, uuid.Nil}, RequestedAt: time.Now().UTC(), UID: 501})
	if err := containTick(t, m); err != nil {
		t.Fatal(err)
	}
	if sub.stops != 1 || sub.kills != 0 {
		t.Fatalf("stops=%d kills=%d, want the latched run's proxy stopped and nothing killed", sub.stops, sub.kills)
	}
	if m.state.Runs[other].ProxyStoppedAt != nil {
		t.Fatal("an unlatched run's proxy was stopped")
	}
	if notified != 1 {
		t.Fatalf("a latch applied offline told the connection %d times, want 1: a resume after it would send no STATE", notified)
	}
	rc := readReceipt(t, m, rid)
	if rc.Error != "" || rc.Runs[id] != containStopped || rc.Runs[uuid.Nil] != containUnknown {
		t.Fatalf("receipt = %+v", rc)
	}
	if _, err := os.Stat(filepath.Join(m.dir, "offline", containInbox, rid.String()+containReqSuffix)); !os.IsNotExist(err) {
		t.Fatalf("the applied request is still in the inbox: %v", err)
	}
	// The next session is up, as Lease requires.
	m.mu.Lock()
	m.online = true
	m.mu.Unlock()
	before := m.deadlines[id]
	if err := m.Lease(t.Context(), runnerwire.Lease{RunID: id, TokenRenewedAt: org.Add(time.Minute), EndsAt: org.Add(4 * time.Hour)}); err != nil {
		t.Fatalf("a LEASE for a latched run = %v, want it dropped without an error", err)
	}
	if m.deadlines[id] != before {
		t.Fatal("a dropped LEASE extended the run's deadline")
	}
	rows, err := allState(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	var latched int
	for _, r := range rows {
		if r.LocalContain != nil {
			latched++
			if r.RunID != id || r.LocalContain.UID != 501 || r.LocalContain.Scope != "run" {
				t.Fatalf("STATE row = %+v", r)
			}
		}
	}
	if latched != 1 {
		t.Fatalf("STATE rows carrying a latch = %d, want 1", latched)
	}

	// Online too, a latch tells the connection once per pass.
	putContainRequest(t, m, containRequest{All: true, RequestedAt: time.Now().UTC()})
	if err := containTick(t, m); err != nil {
		t.Fatal(err)
	}
	if notified != 2 {
		t.Fatalf("after an online latch the connection was told %d times, want 2", notified)
	}

	// A request written while serve was down is applied by the next New.
	third := createLocal(t, m, org)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	putContainRequest(t, m, containRequest{Runs: []uuid.UUID{third}, RequestedAt: time.Now().UTC()})
	reopened, err := New(m.dir, m.runnerID, sub, uuid.UUID.String, noOutput, noErase)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.state.Runs[third].LocalContain == nil {
		t.Fatal("a request written while serve was down was not applied at start")
	}
}

// The inbox is read only as a private directory of regular 0600 files; a symlinked or loose request is refused,
// answered with why and removed, and latches nothing; a loose inbox is refused whole.
func TestContainInbox_RefusesSymlinkAndLoosePerms(t *testing.T) {
	m, _, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	inbox := filepath.Join(m.dir, "offline", containInbox)
	if err := os.Mkdir(inbox, 0700); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(containRequest{Runs: []uuid.UUID{id}, RequestedAt: time.Now().UTC()})
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, body, 0600); err != nil {
		t.Fatal(err)
	}
	link, loose := uuid.New(), uuid.New()
	if err := os.Symlink(target, filepath.Join(inbox, link.String()+containReqSuffix)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inbox, loose.String()+containReqSuffix), body, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(inbox, loose.String()+containReqSuffix), 0644); err != nil {
		t.Fatal(err)
	}
	if err := containTick(t, m); err != nil {
		t.Fatal(err)
	}
	if m.state.Runs[id].LocalContain != nil {
		t.Fatal("a refused request latched a run")
	}
	for _, q := range []uuid.UUID{link, loose} {
		if rc := readReceipt(t, m, q); !strings.HasPrefix(rc.Error, "refused: ") || len(rc.Runs) != 0 {
			t.Fatalf("receipt for a refused request = %+v", rc)
		}
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the symlink's target was touched: %v", err)
	}

	if err := os.Chmod(inbox, 0755); err != nil {
		t.Fatal(err)
	}
	putContainRequest(t, m, containRequest{Runs: []uuid.UUID{id}, RequestedAt: time.Now().UTC()})
	if err := containTick(t, m); err != nil {
		t.Fatal(err)
	}
	if m.state.Runs[id].LocalContain != nil {
		t.Fatal("a request in a loose inbox latched a run")
	}
}

// A latched run with time left on its lease refuses every execution path: a proxy replacement, an exec, a start, a
// local attach and a selftest, so a revive cannot restart its proxy before the next tick; its proxy stays stopped.
func TestContain_LatchedRunRefusesExecution(t *testing.T) {
	m, _, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	ref := id.String()
	if m.AttachRef(id) == "" {
		t.Fatal("the run refuses a local attach before the latch; the test proves nothing")
	}
	putContainRequest(t, m, containRequest{Runs: []uuid.UUID{id}, RequestedAt: time.Now().UTC()})
	if err := containTick(t, m); err != nil {
		t.Fatal(err)
	}
	if m.state.Runs[id].ProxyStoppedAt == nil {
		t.Fatal("the latch did not stop the proxy")
	}
	if m.expired(id) {
		t.Fatal("the run's lease ran out; the test proves nothing")
	}
	gen := m.state.Runs[id].Lease.ExecutionGeneration
	for _, method := range []string{runnerwire.MethodReplaceProxy, runnerwire.MethodExec, runnerwire.MethodStart} {
		if err := m.BeforeRefCall(t.Context(), method, ref, gen); !errors.Is(err, runner.ErrRunnerOffline) {
			t.Errorf("%s on a latched run = %v, want refused", method, err)
		}
	}
	if got := m.AttachRef(id); got != "" {
		t.Errorf("AttachRef on a latched run = %q, want refused", got)
	}
	if err := m.CheckSelftest(t.Context(), ref, gen); !errors.Is(err, runner.ErrRunnerOffline) {
		t.Errorf("CheckSelftest on a latched run = %v, want refused", err)
	}
	if m.state.Runs[id].ProxyStoppedAt == nil {
		t.Fatal("the latched run's proxy is no longer recorded stopped")
	}
}

// A kill the organisation sent as a call and then queued again (the call's answer was lost on a link that stopped
// answering) settles once: both deliveries answer success, the run holds one contained kill and a stopped proxy, and a
// replay of the queued kill answers from its receipt without asking the substrate again.
func TestKill_DeliveredByCallThenPendingSettlesOnce(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	ref := id.String()
	ctx := t.Context()
	if err := m.BeforeRefCall(ctx, runnerwire.MethodKill, ref, uuid.Nil); err != nil {
		t.Fatalf("the kill call: %v", err)
	}
	if err := sub.KillSandbox(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err := m.AfterRefCall(ctx, runnerwire.MethodKill, ref, "", uuid.Nil); err != nil {
		t.Fatalf("the kill call's settlement: %v", err)
	}
	a := pendingContainment(m, id, types.RunnerActionKill)
	first, err := m.apply(ctx, a)
	if err != nil || first.Outcome != types.RunnerActionSuccess {
		t.Fatalf("the queued kill after the call = %+v, %v; want success", first, err)
	}
	kills := sub.kills
	again, err := m.apply(ctx, a)
	if err != nil || again != first || sub.kills != kills {
		t.Fatalf("a replay of the queued kill = %+v, %v (substrate kills %d -> %d); want its receipt, no new kill", again, err, kills, sub.kills)
	}
	r := m.state.Runs[id]
	if r.ProxyStoppedAt == nil || r.Containment == nil || r.Containment.Operation != string(types.RunnerActionKill) || !r.Containment.Contained || r.MutationFailure != nil {
		t.Fatalf("run after both deliveries = stopped %v containment %+v failure %+v; want one contained kill", r.ProxyStoppedAt, r.Containment, r.MutationFailure)
	}
}

// A request or receipt left half-written by a writer that died is removed once a minute old; a fresh one stays.
func TestContainInbox_SweepsStaleTmp(t *testing.T) {
	m, _, _, _ := managerFixture(t)
	inbox := filepath.Join(m.dir, "offline", containInbox)
	if err := os.Mkdir(inbox, 0700); err != nil {
		t.Fatal(err)
	}
	stale, staleDone, fresh := filepath.Join(inbox, uuid.NewString()+".req.tmp"), filepath.Join(inbox, uuid.NewString()+".done.tmp"), filepath.Join(inbox, uuid.NewString()+".req.tmp")
	for _, p := range []string{stale, staleDone, fresh} {
		if err := os.WriteFile(p, []byte("{"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Minute)
	for _, p := range []string{stale, staleDone} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	m.applyContainRequests(t.Context())
	for _, p := range []string{stale, staleDone} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s survived: %v", filepath.Base(p), err)
		}
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a fresh request being written was removed: %v", err)
	}
}
