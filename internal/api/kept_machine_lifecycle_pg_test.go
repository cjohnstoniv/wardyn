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
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// keptMacTransport is a Mac that keeps machines: kept_machine deletes the one it names (already_absent
// the second time), unless fail says otherwise.
type keptMacTransport struct {
	capsTransport
	mu      sync.Mutex
	fail    keptMacFailure
	deleted []string
}

// keptMacFailure is how the Mac's kept_machine call goes wrong.
type keptMacFailure int

const (
	keptMacOK         keptMacFailure = iota
	keptMacRefuses                   // a typed refusal before anything is removed
	keptMacLosesReply                // it deletes the folder, and the answer never arrives
	keptMacLockFails                 // it deletes the folder, then fails to remove the lock file
)

func (t *keptMacTransport) Call(_ context.Context, method string, args, out any, _ ...runnerwire.CallOption) error {
	a, ok := args.(runnerwire.KeptMachineArgs)
	if method != runnerwire.MethodKeptMachine || !ok {
		return errors.New("keptMacTransport: kept_machine only")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.fail == keptMacRefuses {
		return runnerwire.Refuse("kept machine %s is running on this Mac", a.ID)
	}
	outcome := runnerwire.KeptMachineDeleted
	if slices.Contains(t.deleted, a.ID) {
		outcome = runnerwire.KeptMachineAlreadyAbsent
	}
	t.deleted = append(t.deleted, a.ID)
	switch t.fail {
	case keptMacLosesReply:
		return context.DeadlineExceeded
	case keptMacLockFails:
		return (&runnerwire.Error{Code: runnerwire.CodeInternal, Message: "docker: delete kept machine " + a.ID + ": remove lock: permission denied"}).Err()
	}
	*out.(*runnerwire.KeptMachineResult) = runnerwire.KeptMachineResult{Outcome: outcome}
	return nil
}

func (t *keptMacTransport) asked() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.deleted...)
}

// keptFixture is a PG server with the admission fixture's person, their claimed Mac online here and
// keeping machines, their RUNNING run on it, and the site switch on.
type keptFixture struct {
	*admissionFixture
	owner  string
	audit  *syncAudit
	mac    *keptMacTransport
	sub    *remote.Substrate
	mu     sync.Mutex
	online map[uuid.UUID]bool
}

func newKeptFixture(t *testing.T) *keptFixture {
	t.Helper()
	f := newAdmissionFixture(t)
	k := &keptFixture{admissionFixture: f, owner: f.rn.Owner, audit: &syncAudit{},
		mac: &keptMacTransport{capsTransport: capsTransport{online: true}}, online: map[uuid.UUID]bool{f.rn.ID: true}}
	f.srv.cfg.Audit = k.audit
	f.srv.cfg.RunnerOnline = func(id uuid.UUID) bool { k.mu.Lock(); defer k.mu.Unlock(); return k.online[id] }
	k.sub = k.connect(f.rn.ID, k.mac)
	k.inventory(t)
	on := true
	if _, err := f.st.PutSiteConfig(f.ctx, types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowKeptMachines: &on}}); err != nil {
		t.Fatal(err)
	}
	return k
}

func (k *keptFixture) setOnline(id uuid.UUID, on bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.online[id] = on
}

// inventory sends the Mac's caps: it keeps machines, and holds ms.
// connect holds runnerID's session on this replica through tr, online.
func (k *keptFixture) connect(runnerID uuid.UUID, tr *keptMacTransport) *remote.Substrate {
	sub := remote.New(runnerID.String(), tr, remote.Options{})
	k.srv.runnerStreams.subsMu.Lock()
	if k.srv.runnerStreams.subs == nil {
		k.srv.runnerStreams.subs = map[uuid.UUID]runnerEndpoint{}
	}
	k.srv.runnerStreams.subs[runnerID] = runnerEndpoint{sub: sub, link: &remote.Link{}}
	k.srv.runnerStreams.subsMu.Unlock()
	k.setOnline(runnerID, true)
	return sub
}

func (k *keptFixture) inventory(t *testing.T, ms ...runnerwire.MacVMKeptMachine) {
	t.Helper()
	inventoryOf(t, k.sub, ms...)
}

// inventoryOf sends sub's caps: its Mac keeps machines, and holds ms.
func inventoryOf(t *testing.T, sub *remote.Substrate, ms ...runnerwire.MacVMKeptMachine) {
	t.Helper()
	data, err := json.Marshal(runnerwire.Caps{MacOSVM: &runnerwire.MacVMCaps{KeptMachines: true, Kept: &runnerwire.MacVMKept{Machines: ms}}})
	if err != nil {
		t.Fatal(err)
	}
	sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
}

// newRun stores a RUNNING local run of createdBy's on runnerID.
func (k *keptFixture) newRun(t *testing.T, createdBy string, runnerID uuid.UUID) types.AgentRun {
	t.Helper()
	run := newFinalizeRun()
	run.CreatedBy, run.Placement, run.RunnerID = createdBy, types.PlacementLocal, &runnerID
	run, err := k.st.CreateRun(k.ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// claimedMac registers and claims another Mac of owner's.
func (k *keptFixture) claimedMac(t *testing.T, owner string) types.Runner {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	id := uuid.New()
	rn, err := k.st.CreateRunner(k.ctx, types.Runner{ID: id, Owner: owner, OwnerKind: types.RunnerOwnerPerson, Name: "mac-" + id.String()[:4],
		PublicKey: pub, KeyFingerprint: id.String(), Version: "0.9.0", OrgURLSHA256: federation.OrgURLSHA256(admissionOrgURL), Limits: roomyRunnerLimits()})
	if err != nil {
		t.Fatal(err)
	}
	if rn, err = k.st.ClaimRunner(k.ctx, rn.ID, owner, rn.KeyFingerprint, time.Now()); err != nil {
		t.Fatal(err)
	}
	return rn
}

// machine makes owner's machine named name on runnerID for run, in state: creating, in_use or kept.
func (k *keptFixture) machine(t *testing.T, owner string, runnerID, run uuid.UUID, name string, state types.KeptMachineState) types.KeptMachine {
	t.Helper()
	m, err := k.st.CreateKeptMachine(k.ctx, types.KeptMachine{Name: name, OwnerSubject: owner, RunnerID: runnerID, Base: "base-1",
		BasePreparedAt: time.Now().Add(-time.Hour).UTC(), RunID: &run, ExpiresAt: time.Now().Add(30 * 24 * time.Hour).UTC()}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if state == types.KeptMachineCreating {
		return m
	}
	if m, err = k.st.BeginKeptMachineUse(k.ctx, m.ID, owner, run); err != nil {
		t.Fatal(err)
	}
	if state == types.KeptMachineKept {
		m, err = k.st.RecordKeptMachineStop(k.ctx, store.KeptMachineStop{ID: m.ID, RunnerID: runnerID, RunID: run, Scrubbed: true,
			DiskBytes: 1 << 30, ExpiresAt: time.Now().Add(30 * 24 * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// row is machine id's row, false once it is gone.
func (k *keptFixture) row(t *testing.T, id uuid.UUID) (types.KeptMachine, bool) {
	t.Helper()
	m, err := k.st.GetKeptMachine(k.ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return m, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return m, true
}

func (k *keptFixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := k.pool.Exec(k.ctx, q, args...); err != nil {
		t.Fatal(err)
	}
}

// rows is every audit row of action on target, with its data.
func (k *keptFixture) rows(t *testing.T, action string, target uuid.UUID) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ev := range k.audit.snapshot() {
		if ev.Action != action || ev.Target != target.String() {
			continue
		}
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		d["_outcome"], d["_actor"] = ev.Outcome, ev.Actor
		out = append(out, d)
	}
	return out
}

func (k *keptFixture) sweep(t *testing.T) {
	t.Helper()
	if err := k.srv.sweepKeptMachines(k.ctx); err != nil {
		t.Fatal(err)
	}
}

func (k *keptFixture) kill(t *testing.T, run types.AgentRun, actorType types.ActorType, killer string) {
	t.Helper()
	if applied, _, err := k.srv.killRunCascade(k.ctx, run, actorType, killer, nil); err != nil || !applied {
		t.Fatalf("kill: applied %v, %v", applied, err)
	}
}

// requireDeletedBy: the machine's row is gone, its Mac was asked, and machine.delete says by and outcome.
func (k *keptFixture) requireDeletedBy(t *testing.T, m types.KeptMachine, by, outcome string) map[string]any {
	t.Helper()
	if _, ok := k.row(t, m.ID); ok {
		t.Fatalf("machine %s is still recorded", m.Name)
	}
	rows := k.rows(t, "machine.delete", m.ID)
	if len(rows) == 0 {
		t.Fatalf("no machine.delete row for %s", m.Name)
	}
	last := rows[len(rows)-1]
	if last["by"] != by || last["outcome"] != outcome || last["_outcome"] != "success" {
		t.Fatalf("machine.delete = %v, want by %s outcome %s success", last, by, outcome)
	}
	return last
}

// TestNonOwnerKillDeletesTheKeptMachine: an admin's kill of the run on a machine deletes the machine on
// its Mac once the VM is down, audited with the killer.
func TestNonOwnerKillDeletesTheKeptMachine(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
	k.kill(t, k.run, types.ActorHuman, "sub-admin")
	d := k.requireKillDeleted(t, m)
	if d["killer"] != "sub-admin" {
		t.Errorf("killer = %v, want sub-admin", d["killer"])
	}
}

// TestServiceIdentityKillDeletesTheKeptMachine: a keep run is never a service identity's, so its kill is
// always someone else's and deletes the machine.
func TestServiceIdentityKillDeletesTheKeptMachine(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
	k.kill(t, k.run, serviceActorType, types.ServicePrincipalPrefix+"ci-bot")
	k.requireKillDeleted(t, m)
}

// requireKillDeleted: a kill's delete, once the background work it started has finished. The kill
// kicks the runner's resident erase, whose kept-machine sweep settles the same kill intent; whichever
// reaches the Mac first deletes and the other finds the machine already absent. So: the machine is
// gone, the Mac was asked about this machine only, every machine.delete row is the kill's, and exactly
// one of them deleted it. It returns that row.
func (k *keptFixture) requireKillDeleted(t *testing.T, m types.KeptMachine) map[string]any {
	t.Helper()
	k.srv.bg.Wait()
	if _, ok := k.row(t, m.ID); ok {
		t.Fatalf("machine %s is still recorded", m.Name)
	}
	for _, id := range k.mac.asked() {
		if id != m.ID.String() {
			t.Errorf("Mac asked to delete %s, want only %s", id, m.ID)
		}
	}
	var deleted []map[string]any
	for _, r := range k.rows(t, "machine.delete", m.ID) {
		if r["by"] != keptMachineByNonOwnerKill || r["_outcome"] != "success" {
			t.Errorf("machine.delete = %v, want by %s success", r, keptMachineByNonOwnerKill)
		}
		switch r["outcome"] {
		case runnerwire.KeptMachineDeleted:
			deleted = append(deleted, r)
		case runnerwire.KeptMachineAlreadyAbsent:
		default:
			t.Errorf("machine.delete outcome %v, want deleted or already_absent", r["outcome"])
		}
	}
	if len(deleted) != 1 {
		t.Fatalf("%d machine.delete rows deleted %s, want exactly 1", len(deleted), m.Name)
	}
	return deleted[0]
}

// requireKept: the machine is still recorded, with no delete started or intended, and its Mac was never
// asked to delete anything.
func (k *keptFixture) requireKept(t *testing.T, m types.KeptMachine) {
	t.Helper()
	got, ok := k.row(t, m.ID)
	if !ok || got.State == types.KeptMachineDeleting {
		t.Fatalf("machine %s: recorded %v, state %s; want kept", m.Name, ok, got.State)
	}
	if intents, err := k.st.ListKeptMachineDeleteIntents(k.ctx); err != nil || len(intents) != 0 {
		t.Fatalf("delete intents %v, %v; want none", intents, err)
	}
	if got := k.mac.asked(); len(got) != 0 {
		t.Fatalf("Mac asked to delete %v, want nothing", got)
	}
}

// TestOwnerKillKeepsTheMachine: the owner's own kill is a hard stop, and the machine stays.
func TestOwnerKillKeepsTheMachine(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
	k.kill(t, k.run, types.ActorHuman, k.owner)
	k.sweep(t)
	k.requireKept(t, m)
}

// TestLifetimeKillKeepsTheMachine: a system kill (the lifetime cap) keeps the machine as a hard stop.
func TestLifetimeKillKeepsTheMachine(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
	k.kill(t, k.run, types.ActorSystem, "wardynd")
	k.sweep(t)
	k.requireKept(t, m)
}

// TestQueueCancelLeavesTheMachine: cancelling a queued resume, even an admin's, leaves the machine as it
// was: a queued run holds no booted machine.
func TestQueueCancelLeavesTheMachine(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineKept)
	queued := k.newRun(t, k.owner, k.rn.ID)
	k.exec(t, `UPDATE agent_runs SET state = 'QUEUED' WHERE id = $1`, queued.ID)
	queued.State = types.RunQueued
	k.kill(t, queued, types.ActorHuman, "sub-admin")
	k.sweep(t)
	k.requireKept(t, m)
	if got, _ := k.row(t, m.ID); got.State != types.KeptMachineKept || *got.RunID != k.run.ID {
		t.Errorf("machine after the cancel: %s held by %v, want kept by its last run", got.State, got.RunID)
	}
}

// TestSweepRecoveredKillTailKeepsTheMachine: a system kill whose replica stops right after its claim
// marks nothing, so the sweep keeps the machine.
func TestSweepRecoveredKillTailKeepsTheMachine(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
	applied, intent, err := k.srv.claimKill(k.ctx, k.run, types.ActorSystem, "wardynd")
	if err != nil || !applied || !intent.At.IsZero() {
		t.Fatalf("claim: %v %+v %v; want claimed, no intent", applied, intent, err)
	}
	k.sweep(t)
	k.requireKept(t, m)
}

// TestSweepRecoveredAdminKillStillDeletes: an admin's kill whose replica stops right after its claim,
// before the tombstone and the teardown, still deletes: the claim wrote the intent, which blocks a resume,
// and the sweep deletes once the Mac has the VM down.
func TestSweepRecoveredAdminKillStillDeletes(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
	applied, intent, err := k.srv.claimKill(k.ctx, k.run, types.ActorHuman, "sub-admin")
	if err != nil || !applied || intent.At.IsZero() {
		t.Fatalf("claim: %v %+v %v; want claimed with an intent", applied, intent, err)
	}
	// The replica is gone here. The Mac still has the VM booted, then reports the stop.
	k.inventory(t, runnerwire.MacVMKeptMachine{ID: m.ID.String(), InUse: true, RunID: k.run.ID.String()})
	k.srv.runnerMacOSVMDisks(k.ctx, k.rn.ID, []runner.MacOSVMDisk{diskReport(k.run.ID, &runner.KeptScrub{When: "stop", Outcome: keptScrubNotRun})})
	k.requireNoResume(t, m)
	k.sweep(t)
	if got, ok := k.row(t, m.ID); !ok || got.State != types.KeptMachineDeleting || len(k.mac.asked()) != 0 {
		t.Fatalf("while the VM is up: recorded %v state %s, Mac asked %v; want deleting, not asked", ok, got.State, k.mac.asked())
	}
	k.inventory(t, runnerwire.MacVMKeptMachine{ID: m.ID.String()})
	k.exec(t, `UPDATE kept_machines SET delete_started_at = delete_started_at - interval '2 minutes' WHERE id = $1`, m.ID)
	k.sweep(t)
	k.requireDeletedBy(t, m, keptMachineByNonOwnerKill, runnerwire.KeptMachineDeleted)
}

// requireNoResume: the owner's resume of m is refused.
func (k *keptFixture) requireNoResume(t *testing.T, m types.KeptMachine) {
	t.Helper()
	resume := k.newRun(t, k.owner, k.rn.ID)
	if _, err := k.st.BeginKeptMachineUse(k.ctx, m.ID, k.owner, resume.ID); !errors.Is(err, store.ErrKeptMachineDeleting) {
		t.Fatalf("resume of a machine an admin's kill ended = %v, want refused (ErrKeptMachineDeleting)", err)
	}
}

// keptFaultStore is the PG store whose kept-machine writes after a kill's claim fail: the tombstone, and
// the old separate intent write.
type keptFaultStore struct {
	store.PG
}

var errKeptFault = errors.New("injected: the kept machine write failed")

func (keptFaultStore) BeginKeptMachineDeleteForKill(context.Context, uuid.UUID, time.Time) (store.KeptMachineDeletion, error) {
	return store.KeptMachineDeletion{}, errKeptFault
}

func (keptFaultStore) SetKeptMachineDeleteIntent(context.Context, uuid.UUID, string, string) (store.KeptMachineDeletion, error) {
	return store.KeptMachineDeletion{}, errKeptFault
}

func (keptFaultStore) GetKeptMachineByRun(context.Context, uuid.UUID) (types.KeptMachine, error) {
	return types.KeptMachine{}, errKeptFault
}

// TestKillWithFailedKeptMachineWritesStillDeletes: an admin's kill whose every kept-machine write after
// the claim fails still contains the run (KILLED, teardown run), leaves the machine unresumable, and the
// sweep, once the store works, deletes it.
func TestKillWithFailedKeptMachineWritesStillDeletes(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
	k.srv.cfg.Store = keptFaultStore{PG: k.st}
	k.kill(t, k.run, types.ActorHuman, "sub-admin")
	if run, err := k.st.GetRun(k.ctx, k.run.ID); err != nil || run.State != types.RunKilled {
		t.Fatalf("run %s, %v; want KILLED", run.State, err)
	}
	k.requireNoResume(t, m)
	if got, ok := k.row(t, m.ID); !ok || len(k.mac.asked()) != 0 {
		t.Fatalf("machine recorded %v (%s), Mac asked %v; want it waiting for the sweep", ok, got.State, k.mac.asked())
	}
	k.srv.cfg.Store = k.st
	k.sweep(t)
	k.requireDeletedBy(t, m, keptMachineByNonOwnerKill, runnerwire.KeptMachineDeleted)
}

// TestLostKillClaimMarksNothing: a kill that loses its claim to a run that moved on leaves the machine as
// it was, with no intent.
func TestLostKillClaimMarksNothing(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
	stale := k.run
	stale.State = types.RunStarting // the kill read the run before it started: its compare-and-swap loses
	if applied, _, err := k.srv.killRunCascade(k.ctx, stale, types.ActorHuman, "sub-admin", nil); err != nil || applied {
		t.Fatalf("kill: applied %v, %v; want a lost claim", applied, err)
	}
	k.requireKept(t, m)
	if got, _ := k.row(t, m.ID); got.State != types.KeptMachineInUse {
		t.Errorf("state %s, want in_use", got.State)
	}
}

// diskReport is the runner's stop report for run: kept as a kept machine, with scrub.
func diskReport(run uuid.UUID, scrub *runner.KeptScrub) runner.MacOSVMDisk {
	return runner.MacOSVMDisk{RunID: run, State: runner.MacOSVMDiskKept, Reason: runner.MacOSVMDiskKeptMachine, At: time.Now().UTC(), Scrub: scrub}
}

// TestKeepIsTheStopReportNotTheTerminalState: the machine is kept by the runner's stop report, while the
// run is still RUNNING in its ended-run grace; machine.keep and machine.scrub are written once, however
// often the report comes, and a report from another runner changes nothing.
func TestKeepIsTheStopReportNotTheTerminalState(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
	k.exec(t, `UPDATE agent_runs SET lost_at = now(), lost_reason = 'ended' WHERE id = $1`, k.run.ID)
	scrub := &runner.KeptScrub{When: "stop", Outcome: keptScrubDone, Removed: map[string]int{"managed": 2}, CAs: []string{"ab12"}}
	k.srv.runnerMacOSVMDisks(k.ctx, uuid.New(), []runner.MacOSVMDisk{diskReport(k.run.ID, scrub)})
	if got, _ := k.row(t, m.ID); got.State != types.KeptMachineInUse {
		t.Fatalf("another runner's report: state %s, want in_use", got.State)
	}
	for range 2 {
		k.srv.runnerMacOSVMDisks(k.ctx, k.rn.ID, []runner.MacOSVMDisk{diskReport(k.run.ID, scrub)})
	}
	got, _ := k.row(t, m.ID)
	if got.State != types.KeptMachineKept || !got.Scrubbed {
		t.Fatalf("after the report: state %s scrubbed %v, want kept and scrubbed", got.State, got.Scrubbed)
	}
	if run, err := k.st.GetRun(k.ctx, k.run.ID); err != nil || run.State != types.RunRunning {
		t.Fatalf("run %s, %v; want still RUNNING", run.State, err)
	}
	keeps, scrubs := k.rows(t, "machine.keep", m.ID), k.rows(t, "machine.scrub", m.ID)
	if len(keeps) != 1 || keeps[0]["stop"] != keptMachineStopGraceful || keeps[0]["scrubbed"] != true {
		t.Fatalf("machine.keep = %v, want one graceful scrubbed", keeps)
	}
	if len(scrubs) != 1 || scrubs[0]["when"] != "stop" || scrubs[0]["outcome"] != keptScrubDone || scrubs[0]["_outcome"] != "success" {
		t.Fatalf("machine.scrub = %v, want one stop done", scrubs)
	}
}

// TestHardStopReportKeepsTheMachineUnscrubbed: a hard stop runs no scrub, and the machine is kept with
// scrubbed false, for its next boot to clean.
func TestHardStopReportKeepsTheMachineUnscrubbed(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
	k.srv.runnerMacOSVMDisks(k.ctx, k.rn.ID, []runner.MacOSVMDisk{diskReport(k.run.ID, &runner.KeptScrub{When: "stop", Outcome: keptScrubNotRun})})
	if got, _ := k.row(t, m.ID); got.State != types.KeptMachineKept || got.Scrubbed {
		t.Fatalf("state %s scrubbed %v, want kept and not scrubbed", got.State, got.Scrubbed)
	}
	if keeps := k.rows(t, "machine.keep", m.ID); len(keeps) != 1 || keeps[0]["stop"] != keptMachineStopHard {
		t.Fatalf("machine.keep = %v, want one hard stop", keeps)
	}
}

// TestInventoryKeepsAMachineWhoseStopReportNeverCame: the Mac lists a killed run's machine as not in use
// and no stop report came: the sweep keeps it with the Mac's own scrub and size.
func TestInventoryKeepsAMachineWhoseStopReportNeverCame(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
	k.exec(t, `UPDATE agent_runs SET state = 'KILLED', ended_at = now() WHERE id = $1`, k.run.ID)
	exp := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	k.inventory(t, runnerwire.MacVMKeptMachine{ID: m.ID.String(), Bytes: 7 << 30, ExpiresAt: exp})
	k.sweep(t)
	got, _ := k.row(t, m.ID)
	if got.State != types.KeptMachineKept || got.Scrubbed || got.DiskBytes != 7<<30 || !got.ExpiresAt.Equal(exp) {
		t.Fatalf("after the sweep: %+v; want kept, unscrubbed, the Mac's size and expiry", got)
	}
	if keeps := k.rows(t, "machine.keep", m.ID); len(keeps) != 1 || keeps[0]["stop"] != keptMachineStopUnreported {
		t.Fatalf("machine.keep = %v, want one unreported", keeps)
	}
}

// TestReconcileIgnoresAnotherRunnersInventory: a runner's inventory speaks only for the machines
// recorded on that runner. Mac B listing Mac A's machine as stopped neither keeps it nor sets its size,
// and its stop report for A's run changes nothing; A's own inventory still decides.
func TestReconcileIgnoresAnotherRunnersInventory(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
	k.exec(t, `UPDATE agent_runs SET state = 'KILLED', ended_at = now() WHERE id = $1`, k.run.ID)
	b := k.claimedMac(t, k.owner)
	macB := &keptMacTransport{capsTransport: capsTransport{online: true}}
	inventoryOf(t, k.connect(b.ID, macB), runnerwire.MacVMKeptMachine{ID: m.ID.String(), Bytes: 9 << 30, Scrubbed: true,
		ExpiresAt: time.Now().Add(24 * time.Hour).UTC()})
	k.srv.runnerMacOSVMDisks(k.ctx, b.ID, []runner.MacOSVMDisk{diskReport(k.run.ID, &runner.KeptScrub{When: "stop", Outcome: keptScrubDone})})
	k.sweep(t)
	if got, _ := k.row(t, m.ID); got.State != types.KeptMachineInUse || got.DiskBytes == 9<<30 || got.Scrubbed {
		t.Fatalf("after Mac B's inventory and report: %+v; want A's machine untouched", got)
	}
	if keeps := k.rows(t, "machine.keep", m.ID); len(keeps) != 0 || len(macB.asked()) != 0 {
		t.Fatalf("machine.keep %v, Mac B asked %v; want neither", keeps, macB.asked())
	}
	k.inventory(t, runnerwire.MacVMKeptMachine{ID: m.ID.String(), Bytes: 2 << 30, ExpiresAt: time.Now().Add(24 * time.Hour).UTC()})
	k.sweep(t)
	if got, _ := k.row(t, m.ID); got.State != types.KeptMachineKept || got.DiskBytes != 2<<30 {
		t.Fatalf("after Mac A's own inventory: %+v; want kept at A's size", got)
	}
}

// TestReconcileLeavesALiveRunsMachine: a live run's machine the Mac does not show in use (its boot has not
// begun yet) stays the run's.
func TestReconcileLeavesALiveRunsMachine(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
	k.inventory(t, runnerwire.MacVMKeptMachine{ID: m.ID.String()})
	k.sweep(t)
	if got, _ := k.row(t, m.ID); got.State != types.KeptMachineInUse {
		t.Fatalf("state %s, want in_use", got.State)
	}
}

// TestReconcileDropsAKeepThatNeverStarted: a machine still being created whose run ended long ago and
// which its Mac does not list was never made, and its row goes, so an erasure does not meet it as held.
func TestReconcileDropsAKeepThatNeverStarted(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineCreating)
	k.exec(t, `UPDATE agent_runs SET state = 'FAILED', ended_at = now() - interval '11 minutes' WHERE id = $1`, k.run.ID)
	k.sweep(t)
	if _, ok := k.row(t, m.ID); ok {
		t.Fatal("an unstarted keep's machine is still recorded")
	}
}

// TestExpiredKeptMachineIsDeleted: a kept machine past its expiry is deleted on its Mac by the sweep.
func TestExpiredKeptMachineIsDeleted(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineKept)
	k.exec(t, `UPDATE kept_machines SET expires_at = now() - interval '1 minute' WHERE id = $1`, m.ID)
	k.sweep(t)
	k.requireDeletedBy(t, m, keptMachineByExpiry, runnerwire.KeptMachineDeleted)
}

// TestRevokedRunnersKeptMachinesCloseRecordOnly: a revoked runner erases its kept machines itself, so
// the sweep closes their rows record-only and never calls it.
func TestRevokedRunnersKeptMachinesCloseRecordOnly(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineKept)
	k.exec(t, `UPDATE runners SET revoked_at = now() WHERE id = $1`, k.rn.ID)
	k.sweep(t)
	k.requireDeletedBy(t, m, keptMachineByRevokedRunner, driveReclaimOutcomeRecordOnly)
	if got := k.mac.asked(); len(got) != 0 {
		t.Errorf("revoked Mac asked %v", got)
	}
}

func (k *keptFixture) deleteMine(sub string, id uuid.UUID) *httptest.ResponseRecorder {
	return memberCall(personCtx(sub), k.srv.handleDeleteMyMachine, http.MethodDelete, "/api/v1/me/machines/"+id.String(), "", map[string]string{"id": id.String()})
}

func (k *keptFixture) listMine(t *testing.T, sub string) client.MyMachines {
	t.Helper()
	w := memberCall(personCtx(sub), k.srv.handleListMyMachines, http.MethodGet, "/api/v1/me/machines", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var out client.MyMachines
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestMacReconnectFinishesAKeptMachineDelete: the owner's delete while the Mac is offline is 202 and
// keeps the tombstone (the list says deleting, by the person); once the Mac is back the sweep deletes it.
func TestMacReconnectFinishesAKeptMachineDelete(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineKept)
	k.setOnline(k.rn.ID, false)
	if w := k.deleteMine(k.owner, m.ID); w.Code != http.StatusAccepted {
		t.Fatalf("offline delete: %d %s, want 202", w.Code, w.Body.String())
	}
	list := k.listMine(t, k.owner)
	if len(list.Machines) != 1 || list.Machines[0].State != types.KeptMachineDeleting || list.Machines[0].DeleteReason != client.KeptMachineDeletedByPerson {
		t.Fatalf("list while waiting: %+v, want one deleting by the person", list.Machines)
	}
	k.exec(t, `UPDATE kept_machines SET delete_started_at = delete_started_at - interval '2 minutes' WHERE id = $1`, m.ID)
	k.sweep(t) // still offline: waits
	if _, ok := k.row(t, m.ID); !ok || len(k.mac.asked()) != 0 {
		t.Fatal("the sweep deleted a machine whose Mac is offline")
	}
	k.setOnline(k.rn.ID, true)
	k.sweep(t)
	k.requireDeletedBy(t, m, keptMachineByOwner, runnerwire.KeptMachineDeleted)
	if rows := k.rows(t, "machine.delete", m.ID); len(rows) != 2 || rows[0]["outcome"] != driveReclaimOutcomeDeferred {
		t.Errorf("machine.delete rows = %v, want deferred then deleted", rows)
	}
}

// TestKeptMachineDeleteOfAMachineARunHoldsIsRefused: the owner's and an admin's delete of a machine a
// run holds are refused with kept_machine_in_use and its sentence, audited as refused, and change nothing.
func TestKeptMachineDeleteOfAMachineARunHoldsIsRefused(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
	body := wantReason(t, k.deleteMine(k.owner, m.ID), http.StatusConflict, reasonKeptMachineInUse)
	if body.Error != "This machine is in use by a run. End that run first, then delete it." {
		t.Errorf("owner: %q", body.Error)
	}
	w := memberCall(adminCtx(), k.srv.handleDeleteKeptMachineAdmin, http.MethodDelete, "/api/v1/machines/kept/"+m.ID.String(), "", map[string]string{"id": m.ID.String()})
	wantReason(t, w, http.StatusConflict, reasonKeptMachineInUse)
	if got, _ := k.row(t, m.ID); got.State != types.KeptMachineInUse || len(k.mac.asked()) != 0 {
		t.Fatalf("after the refusals: state %s, Mac asked %v", got.State, k.mac.asked())
	}
	if rows := k.rows(t, "machine.delete", m.ID); len(rows) != 2 || rows[0]["outcome"] != driveReclaimOutcomeRefused || rows[1]["by"] != keptMachineByAdmin {
		t.Errorf("machine.delete rows = %v, want two refused (owner, admin)", rows)
	}
}

// TestKeptMachineDeleteForeignIDUniform404: another person's machine and an id that names nothing get the
// same 404, and the Mac is never asked.
func TestKeptMachineDeleteForeignIDUniform404(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineKept)
	var first []byte
	for _, id := range []uuid.UUID{m.ID, uuid.New()} {
		w := k.deleteMine("sub-someone-else", id)
		wantReason(t, w, http.StatusNotFound, reasonKeptMachineNotFound)
		b := bytes.ReplaceAll(w.Body.Bytes(), []byte(id.String()), []byte("ID"))
		if first == nil {
			first = b
		} else if !bytes.Equal(b, first) {
			t.Errorf("body %s, want the same bytes as %s", b, first)
		}
	}
	if got, _ := k.row(t, m.ID); got.State != types.KeptMachineKept || len(k.mac.asked()) != 0 {
		t.Fatal("another person's delete touched the machine")
	}
	if list := k.listMine(t, "sub-someone-else"); len(list.Machines) != 0 {
		t.Errorf("another person lists %+v", list.Machines)
	}
}

// TestKeptMachineOwnerDeleteDeletesOnTheMac: the owner's delete of a kept machine is 204 once its Mac has
// deleted it.
func TestKeptMachineOwnerDeleteDeletesOnTheMac(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineKept)
	if w := k.deleteMine(k.owner, m.ID); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s, want 204", w.Code, w.Body.String())
	}
	k.requireDeletedBy(t, m, keptMachineByOwner, runnerwire.KeptMachineDeleted)
}

// TestKeptMachineFailedDeleteKeepsTheMachine: a Mac that refuses the delete before removing anything
// leaves the machine kept and resumable, 503 kept_machine_delete_failed.
func TestKeptMachineFailedDeleteKeepsTheMachine(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineKept)
	k.mac.fail = keptMacRefuses
	wantReason(t, k.deleteMine(k.owner, m.ID), http.StatusServiceUnavailable, reasonKeptMachineDeleteFailed)
	if got, ok := k.row(t, m.ID); !ok || got.State != types.KeptMachineKept {
		t.Fatalf("after a refused delete: recorded %v, state %s; want kept", ok, got.State)
	}
	if _, err := k.st.BeginKeptMachineUse(k.ctx, m.ID, k.owner, k.newRun(t, k.owner, k.rn.ID).ID); err != nil {
		t.Fatalf("resume after a refused delete: %v", err)
	}
}

// TestKeptMachineUnconfirmedDeleteStaysPending: a Mac that deleted the folder but whose answer was lost,
// or that deleted it and then failed on the lock file, may have removed the machine: the delete stays
// pending (503 kept_machine_delete_unconfirmed, the list says deleting), no resume takes it, and the
// sweep retries until the Mac confirms, already_absent.
func TestKeptMachineUnconfirmedDeleteStaysPending(t *testing.T) {
	for name, fail := range map[string]keptMacFailure{"reply lost": keptMacLosesReply, "lock cleanup fails": keptMacLockFails} {
		t.Run(name, func(t *testing.T) {
			k := newKeptFixture(t)
			m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineKept)
			k.mac.fail = fail
			body := wantReason(t, k.deleteMine(k.owner, m.ID), http.StatusServiceUnavailable, reasonKeptMachineDeleteUnconfirmed)
			if body.Error != "Wardyn couldn't confirm the delete. It will keep trying; the machine can't be resumed meanwhile." {
				t.Errorf("sentence %q", body.Error)
			}
			if list := k.listMine(t, k.owner); len(list.Machines) != 1 || list.Machines[0].State != types.KeptMachineDeleting {
				t.Fatalf("list: %+v, want the machine deleting", list.Machines)
			}
			k.requireNoResume(t, m)
			if rows := k.rows(t, "machine.delete", m.ID); len(rows) != 1 || rows[0]["outcome"] != keptMachineOutcomeUnconfirmed || rows[0]["_outcome"] != "failure" {
				t.Fatalf("machine.delete = %v, want one unconfirmed failure", rows)
			}
			k.mac.fail = keptMacOK
			k.exec(t, `UPDATE kept_machines SET delete_started_at = delete_started_at - interval '2 minutes' WHERE id = $1`, m.ID)
			k.sweep(t)
			k.requireDeletedBy(t, m, keptMachineByOwner, runnerwire.KeptMachineAlreadyAbsent)
		})
	}
}

// TestMyMachinesSaysWhetherThePersonMayKeep: the list carries the person's machines with their Mac's
// name, the count and limit, and allowed folds in the site switch, the Mac's pools and the limit.
func TestMyMachinesSaysWhetherThePersonMayKeep(t *testing.T) {
	k := newKeptFixture(t)
	got := k.listMine(t, k.owner)
	if !got.Allowed || got.Reason != "" || got.Count != 0 || got.Limit != types.MaxKeptMachinesPerPersonDefault || got.WorstBytes != keptMachineNewWorstBytes {
		t.Fatalf("own Mac, nothing kept: %+v; want allowed", got)
	}
	if got.IdleDays != types.KeptMachineIdleDaysDefault {
		t.Fatalf("idle_days = %d, want the site default %d", got.IdleDays, types.KeptMachineIdleDaysDefault)
	}
	k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineKept)
	got = k.listMine(t, k.owner)
	if got.Allowed || got.Reason != reasonKeptMachinePersonLimit || got.Count != 1 || len(got.Machines) != 1 || got.Machines[0].RunnerName != k.rn.Name {
		t.Fatalf("at the limit: %+v; want person_limit and the machine on %s", got, k.rn.Name)
	}
	on := true
	if _, err := k.st.PutSiteConfig(k.ctx, types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowKeptMachines: &on, MaxKeptMachinesPerPerson: 2}}); err != nil {
		t.Fatal(err)
	}
	if got = k.listMine(t, k.owner); !got.Allowed || got.Limit != 2 {
		t.Fatalf("under a limit of 2: %+v; want allowed", got)
	}
	theirs, err := k.st.CreateRunnerPool(k.ctx, types.RunnerPool{ID: uuid.New(), Name: "Theirs", Scope: types.RunnerPoolUser, Owner: "sub-someone-else"}, "sub-someone-else")
	if err != nil {
		t.Fatal(err)
	}
	k.exec(t, `INSERT INTO runner_pool_members (pool_id, runner_id) VALUES ($1, $2)`, theirs.ID, k.rn.ID)
	if got = k.listMine(t, k.owner); got.Allowed || got.Reason != reasonKeptMachineNotYourMac {
		t.Fatalf("Mac in another person's pool: %+v; want not_your_mac", got)
	}
	if _, err := k.st.PutSiteConfig(k.ctx, types.SiteConfig{}); err != nil {
		t.Fatal(err)
	}
	if got = k.listMine(t, k.owner); got.Allowed || got.Reason != reasonKeptMachineSiteOff || len(got.Machines) != 1 {
		t.Fatalf("switch off: %+v; want site_off, the machine still listed", got)
	}
}

// TestKeptMachineAdminListAndDelete: the super admin lists every person's machines, narrowed by owner and
// runner, and deletes one on its Mac, audited as the admin's.
func TestKeptMachineAdminListAndDelete(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineKept)
	other := k.claimedMac(t, "sub-other")
	theirs := k.machine(t, "sub-other", other.ID, k.newRun(t, "sub-other", other.ID).ID, "box", types.KeptMachineKept)
	list := func(q string) client.KeptMachineList {
		w := memberCall(adminCtx(), k.srv.handleListKeptMachines, http.MethodGet, "/api/v1/machines/kept"+q, "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("list %s: %d %s", q, w.Code, w.Body.String())
		}
		var out client.KeptMachineList
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := list(""); got.Total != 2 || len(got.Machines) != 2 {
		t.Fatalf("all: %+v", got)
	}
	if got := list("?owner=sub-other"); got.Total != 1 || got.Machines[0].ID != theirs.ID {
		t.Fatalf("by owner: %+v", got)
	}
	if got := list("?runner=" + k.rn.ID.String()); got.Total != 1 || got.Machines[0].ID != m.ID || got.Machines[0].RunnerName != k.rn.Name {
		t.Fatalf("by runner: %+v", got)
	}
	w := memberCall(adminCtx(), k.srv.handleDeleteKeptMachineAdmin, http.MethodDelete, "/api/v1/machines/kept/"+m.ID.String(), "", map[string]string{"id": m.ID.String()})
	var res client.KeptMachineDeleteResult
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &res) != nil || res.Outcome != runnerwire.KeptMachineDeleted || res.Owner != k.owner {
		t.Fatalf("admin delete: %d %s", w.Code, w.Body.String())
	}
	k.requireDeletedBy(t, m, keptMachineByAdmin, runnerwire.KeptMachineDeleted)
}

// TestKeptMachineErasureStep: erasure deletes the person's machine on an online Mac, closes the one on a
// revoked runner record-only, and fails the scope by name for the one whose Mac is offline (its tombstone
// waits for the Mac) and the one a run holds (kept as it was).
func TestKeptMachineErasureStep(t *testing.T) {
	k := newKeptFixture(t)
	online := k.machine(t, k.owner, k.rn.ID, k.run.ID, "online", types.KeptMachineKept)
	away := k.claimedMac(t, k.owner)
	offline := k.machine(t, k.owner, away.ID, k.newRun(t, k.owner, away.ID).ID, "offline", types.KeptMachineKept)
	gone := k.claimedMac(t, k.owner)
	revoked := k.machine(t, k.owner, gone.ID, k.newRun(t, k.owner, gone.ID).ID, "revoked", types.KeptMachineKept)
	k.exec(t, `UPDATE runners SET revoked_at = now() WHERE id = $1`, gone.ID)
	held := k.machine(t, k.owner, k.rn.ID, k.newRun(t, k.owner, k.rn.ID).ID, "held", types.KeptMachineInUse)
	theirs := k.machine(t, "sub-other", k.rn.ID, k.newRun(t, "sub-other", k.rn.ID).ID, "online", types.KeptMachineKept)

	res, err := k.srv.eraseMemberDrivesOf(k.ctx, k.owner)
	if err == nil || !strings.Contains(err.Error(), `"offline"`) || !strings.Contains(err.Error(), `"held"`) {
		t.Fatalf("erasure error %v; want the offline and held machines named", err)
	}
	if n := res.(map[string]any)["kept_machines"]; n != 2 {
		t.Errorf("kept_machines = %v, want 2 (online deleted, revoked record-only)", n)
	}
	k.requireDeletedBy(t, online, keptMachineByErasure, runnerwire.KeptMachineDeleted)
	k.requireDeletedBy(t, revoked, keptMachineByErasure, driveReclaimOutcomeRecordOnly)
	if got, _ := k.row(t, offline.ID); got.State != types.KeptMachineDeleting {
		t.Errorf("offline machine: %s, want deleting until its Mac is back", got.State)
	}
	if got, _ := k.row(t, held.ID); got.State != types.KeptMachineInUse {
		t.Errorf("held machine: %s, want in_use", got.State)
	}
	if _, ok := k.row(t, theirs.ID); !ok {
		t.Error("another person's machine was erased")
	}
}

// TestKeptMachineErasureKeepsAMachineARunHolds: a machine a run holds is not deleted by erasure, which
// fails by its name, and its Mac is never asked.
func TestKeptMachineErasureKeepsAMachineARunHolds(t *testing.T) {
	k := newKeptFixture(t)
	held := k.machine(t, k.owner, k.rn.ID, k.run.ID, "held", types.KeptMachineInUse)
	if _, err := k.srv.eraseMemberDrivesOf(k.ctx, k.owner); err == nil || !strings.Contains(err.Error(), `kept machine "held" was kept: a run holds it`) {
		t.Fatalf("erasure error %v; want the held machine named", err)
	}
	if got, _ := k.row(t, held.ID); got.State != types.KeptMachineInUse || len(k.mac.asked()) != 0 {
		t.Fatalf("held machine: %s, Mac asked %v", got.State, k.mac.asked())
	}
}

// TestKeptMachineErasureFailsWhenTheMacDoesNotDelete: a Mac that does not delete the machine fails the
// scope; erasure never reports it gone.
func TestKeptMachineErasureFailsWhenTheMacDoesNotDelete(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineKept)
	k.mac.fail = keptMacRefuses
	res, err := k.srv.eraseMemberDrivesOf(k.ctx, k.owner)
	if err == nil || !strings.Contains(err.Error(), `kept machine "dev" was kept: its Mac did not delete it`) {
		t.Fatalf("erasure error %v; want the machine named", err)
	}
	if n := res.(map[string]any)["kept_machines"]; n != 0 {
		t.Errorf("kept_machines = %v, want 0", n)
	}
	if _, ok := k.row(t, m.ID); !ok {
		t.Fatal("the machine's record went although its Mac did not delete it")
	}
}

// TestKeptMachineErasureFailsWhileTheDeleteIsUnconfirmed: a Mac whose answer was lost leaves the delete
// pending, and erasure fails by the machine's name rather than report it erased.
func TestKeptMachineErasureFailsWhileTheDeleteIsUnconfirmed(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineKept)
	k.mac.fail = keptMacLosesReply
	res, err := k.srv.eraseMemberDrivesOf(k.ctx, k.owner)
	if err == nil || !strings.Contains(err.Error(), `kept machine "dev" is being deleted: its Mac did not confirm the delete`) {
		t.Fatalf("erasure error %v; want the machine named", err)
	}
	if n := res.(map[string]any)["kept_machines"]; n != 0 {
		t.Errorf("kept_machines = %v, want 0", n)
	}
	if got, ok := k.row(t, m.ID); !ok || got.State != types.KeptMachineDeleting {
		t.Fatalf("machine: recorded %v, state %s; want deleting", ok, got.State)
	}
}
