// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestKeptStaleIdleReportNeverSettlesOrExpiresAReusedMachine: a person resumed their kept machine before its
// old expiry and the new run ended after that date, but the Mac's stop report has not arrived. An idle
// inventory that is older (a cached one from an earlier stop), a later use's own predecessor, or unversioned
// (an older runner) must neither keep the machine with the old deadline nor, in the same sweep, expire it and
// delete the folder. The delayed current report, which names this run's stop, keeps it with the Mac's later
// deadline.
func TestKeptStaleIdleReportNeverSettlesOrExpiresAReusedMachine(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
	k.exec(t, `UPDATE kept_machines SET expires_at = now() - interval '1 hour' WHERE id = $1`, m.ID)
	k.exec(t, `UPDATE agent_runs SET state = 'KILLED', ended_at = now() WHERE id = $1`, k.run.ID)
	past := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	for name, stop := range map[string]string{
		"an older stop's cached idle report": uuid.NewString(),
		"an unversioned idle report":         "",
	} {
		k.inventory(t, runnerwire.MacVMKeptMachine{ID: m.ID.String(), Bytes: 1 << 30, StopRunID: stop, ExpiresAt: past})
		k.sweep(t)
		if got, ok := k.row(t, m.ID); !ok || got.State != types.KeptMachineInUse {
			t.Fatalf("%s: recorded %v, state %s; want the machine still in use while the run has only just ended", name, ok, got.State)
		}
		if keeps := k.rows(t, "machine.keep", m.ID); len(keeps) != 0 || len(k.mac.opsAsked()) != 0 {
			t.Fatalf("%s: machine.keep %v, Mac asked %v; want neither", name, keeps, k.mac.opsAsked())
		}
	}
	later := time.Now().Add(10 * 24 * time.Hour).UTC().Truncate(time.Second)
	k.inventory(t, runnerwire.MacVMKeptMachine{ID: m.ID.String(), Bytes: 2 << 30, StopRunID: k.run.ID.String(), ExpiresAt: later})
	k.sweep(t)
	got, ok := k.row(t, m.ID)
	if !ok || got.State != types.KeptMachineKept || !got.ExpiresAt.Equal(later) || len(k.mac.opsAsked()) != 0 {
		t.Fatalf("after the current report: recorded %v, %+v, Mac asked %v; want kept until the Mac's %s and not deleted", ok, got, k.mac.opsAsked(), later)
	}
}

// TestKeptUnmatchedIdleReportGivesTheMachineBackAfterTheGrace: the run ended long enough ago that its stop
// report is not coming (an older runner never names the stop, the report was lost, or the create never
// reached the Mac), and the Mac's idle report is an earlier use's or unversioned. The machine is kept and
// unscrubbed, never with the report's past deadline, so the owner can resume or delete it and nothing expires
// in the same sweep; the Mac is not asked to delete. The deadline it had is kept only when the Mac says it runs
// this release (the conditional delete) and names a stop of an earlier run: then it holds no stop of this one.
// Any other Mac gets the site's idle days from now, whether the run's sandbox ref was cleared at its end or
// never set.
func TestKeptUnmatchedIdleReportGivesTheMachineBackAfterTheGrace(t *testing.T) {
	past := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	for name, tc := range map[string]struct {
		legacy   bool
		stop     string
		previous bool
	}{
		"a current Mac, an earlier stop":   {false, uuid.NewString(), true},
		"a current Mac, an unversioned":    {false, "", false},
		"a legacy Mac, any stop id":        {true, uuid.NewString(), false},
		"a legacy Mac, an unversioned one": {true, "", false},
	} {
		k := newKeptFixture(t)
		m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
		k.exec(t, `UPDATE kept_machines SET expires_at = now() + interval '3 days', scrubbed = true WHERE id = $1`, m.ID)
		before, _ := k.row(t, m.ID)
		k.exec(t, `UPDATE agent_runs SET state = 'KILLED', ended_at = now() - interval '1 hour', sandbox_ref = '' WHERE id = $1`, k.run.ID)
		report := runnerwire.MacVMKeptMachine{ID: m.ID.String(), Bytes: 1 << 30, StopRunID: tc.stop, ExpiresAt: past, Scrubbed: true}
		if tc.legacy {
			legacyInventoryOf(t, k.sub, report)
		} else {
			k.inventory(t, report)
		}
		k.sweep(t)
		got, ok := k.row(t, m.ID)
		want := time.Now().AddDate(0, 0, types.KeptMachineIdleDaysDefault)
		if tc.previous {
			want = before.ExpiresAt
		}
		if !ok || got.State != types.KeptMachineKept || got.Scrubbed || got.ExpiresAt.Before(want.Add(-time.Hour)) || got.ExpiresAt.After(want.Add(time.Hour)) {
			t.Fatalf("%s: recorded %v, %+v; want kept, unscrubbed, due near %s", name, ok, got, want)
		}
		if len(k.mac.opsAsked()) != 0 {
			t.Fatalf("%s: Mac asked %v; want nothing", name, k.mac.opsAsked())
		}
		if _, err := k.st.BeginKeptMachineUse(k.ctx, m.ID, k.owner, k.newRun(t, k.owner, k.rn.ID).ID); err != nil {
			t.Fatalf("%s: a resume after the give-back: %v", name, err)
		}
	}
}

// TestKeptIdleReportOfAnEarlierRunDoesNotSettleALaterUse: the machine's current holder is a later run whose
// stop was not reported; an idle report that names the stop of the run before it does not settle that later
// run while it has only just ended, whatever its deadline.
func TestKeptIdleReportOfAnEarlierRunDoesNotSettleALaterUse(t *testing.T) {
	k := newKeptFixture(t)
	earlier := uuid.NewString()
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
	k.exec(t, `UPDATE agent_runs SET state = 'KILLED', ended_at = now() WHERE id = $1`, k.run.ID)
	k.inventory(t, runnerwire.MacVMKeptMachine{ID: m.ID.String(), Bytes: 1 << 30, StopRunID: earlier, ExpiresAt: time.Now().Add(-time.Hour).UTC()})
	k.sweep(t)
	if got, _ := k.row(t, m.ID); got.State != types.KeptMachineInUse || len(k.mac.opsAsked()) != 0 {
		t.Fatalf("after an earlier run's idle report: state %s, Mac asked %v; want in use and not asked", got.State, k.mac.opsAsked())
	}
}

// expiredKept is a kept machine of k's run whose organisation row holds a deadline that has passed.
func (k *keptFixture) expiredKept(t *testing.T) types.KeptMachine {
	t.Helper()
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineKept)
	k.exec(t, `UPDATE kept_machines SET expires_at = now() - interval '1 hour' WHERE id = $1`, m.ID)
	return m
}

// TestKeptExpiryDeleteNeedsTheMacsConditionalDelete: a Mac of the earlier release removes the folder of any
// delete without reading its own deadline, so the organisation sends it no expiry-driven delete, new or
// resumed from an old tombstone; the machine stays kept and the Mac's own sweep removes it if it has expired.
// A Mac that says it runs the conditional delete is sent that op, and only that op.
func TestKeptExpiryDeleteNeedsTheMacsConditionalDelete(t *testing.T) {
	k := newKeptFixture(t)
	m := k.expiredKept(t)

	// A stale kept row, a legacy Mac that lists the machine.
	listed := runnerwire.MacVMKeptMachine{ID: m.ID.String(), Bytes: 1 << 30}
	legacyInventoryOf(t, k.sub, listed)
	k.sweep(t)
	if got, ok := k.row(t, m.ID); !ok || got.State != types.KeptMachineKept || len(k.mac.opsAsked()) != 0 {
		t.Fatalf("a legacy Mac: recorded %v, state %s, calls %v; want kept and no call", ok, got.State, k.mac.opsAsked())
	}
	// An old deferred tombstone of an earlier control plane, a legacy Mac: put back, not executed.
	if _, err := k.st.BeginKeptMachineDelete(k.ctx, "", m.ID, keptMachineByExpiry); err != nil {
		t.Fatal(err)
	}
	k.exec(t, `UPDATE kept_machines SET delete_started_at = now() - interval '1 hour' WHERE id = $1`, m.ID)
	k.sweep(t)
	if got, ok := k.row(t, m.ID); !ok || got.State != types.KeptMachineKept || len(k.mac.opsAsked()) != 0 {
		t.Fatalf("an old tombstone, a legacy Mac: recorded %v, state %s, calls %v; want kept again and no call", ok, got.State, k.mac.opsAsked())
	}
	if rows := k.rows(t, "machine.delete", m.ID); len(rows) != 0 {
		t.Fatalf("a tombstone put back wrote machine.delete rows %v; want none", rows)
	}
	// An owner's delete that takes over such a tombstone (older than a minute) is still an expiry for a Mac
	// that cannot recheck: nothing is sent and the machine stays; asked again, it is the plain delete.
	if _, err := k.st.BeginKeptMachineDelete(k.ctx, "", m.ID, keptMachineByExpiry); err != nil {
		t.Fatal(err)
	}
	k.exec(t, `UPDATE kept_machines SET delete_started_at = now() - interval '1 hour' WHERE id = $1`, m.ID)
	k.deleteMine(k.owner, m.ID)
	if got, ok := k.row(t, m.ID); !ok || got.State != types.KeptMachineKept || len(k.mac.opsAsked()) != 0 {
		t.Fatalf("an owner's delete over an expiry tombstone, a legacy Mac: recorded %v, state %s, calls %v; want kept and no call", ok, got.State, k.mac.opsAsked())
	}
	if w := k.deleteMine(k.owner, m.ID); w.Code >= 300 {
		t.Fatalf("the owner's delete after that = %d %s", w.Code, w.Body)
	}
	if ops := k.mac.opsAsked(); len(ops) != 1 || ops[0] != runnerwire.KeptMachineOpDelete {
		t.Fatalf("the owner's own delete sent %v, want one plain delete", ops)
	}
	k.mac.mu.Lock()
	k.mac.ops, k.mac.deleted = nil, nil
	k.mac.mu.Unlock()
	m = k.expiredKept(t)
	// The same tombstone, a Mac that runs the conditional delete: finished with the conditional op.
	if _, err := k.st.BeginKeptMachineDelete(k.ctx, "", m.ID, keptMachineByExpiry); err != nil {
		t.Fatal(err)
	}
	k.exec(t, `UPDATE kept_machines SET delete_started_at = now() - interval '1 hour' WHERE id = $1`, m.ID)
	k.inventory(t)
	k.sweep(t)
	if ops := k.mac.opsAsked(); len(ops) != 1 || ops[0] != runnerwire.KeptMachineOpDeleteExpired {
		t.Fatalf("a current Mac: calls %v, want one delete_expired", ops)
	}
	k.requireDeletedBy(t, m, keptMachineByExpiry, runnerwire.KeptMachineDeleted)

	// A person's delete is not conditional, on either generation of Mac.
	for name, legacy := range map[string]bool{"current": false, "legacy": true} {
		k2 := newKeptFixture(t)
		if legacy {
			legacyInventoryOf(t, k2.sub)
		}
		own := k2.machine(t, k2.owner, k2.rn.ID, k2.run.ID, "mine", types.KeptMachineKept)
		if w := k2.deleteMine(k2.owner, own.ID); w.Code >= 300 {
			t.Fatalf("%s Mac: the owner's delete = %d %s", name, w.Code, w.Body)
		}
		if ops := k2.mac.opsAsked(); len(ops) != 1 || ops[0] != runnerwire.KeptMachineOpDelete {
			t.Fatalf("%s Mac: the owner's delete sent %v, want one plain delete", name, ops)
		}
	}
}

// TestKeptStaleDeadlineIsRepairedFromTheMatchingStop: a kept row an older report left with a passed deadline,
// whose last holder's stop the Mac now reports with a later deadline, takes the Mac's deadline, so the
// machine is not expired early and a later resume is not refused as expired. A report that does not name that
// stop (an older runner's, a cached one) changes nothing.
func TestKeptStaleDeadlineIsRepairedFromTheMatchingStop(t *testing.T) {
	k := newKeptFixture(t)
	// The reports that name no matching stop meet a deadline that has not passed: a passed one would be the expiry
	// sweep's, which sends this Mac the conditional delete, and the question here is only the repair.
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineKept)
	later := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	k.inventory(t, runnerwire.MacVMKeptMachine{ID: m.ID.String(), Bytes: 1 << 30, StopRunID: uuid.NewString(), ExpiresAt: later})
	k.sweep(t)
	if got, _ := k.row(t, m.ID); got.ExpiresAt.Equal(later) {
		t.Fatalf("a report of another run's stop moved the deadline to %s", got.ExpiresAt)
	}
	k.inventory(t, runnerwire.MacVMKeptMachine{ID: m.ID.String(), Bytes: 1 << 30, ExpiresAt: later})
	k.sweep(t)
	if got, _ := k.row(t, m.ID); got.ExpiresAt.Equal(later) {
		t.Fatalf("an unversioned report moved the deadline to %s", got.ExpiresAt)
	}
	k.exec(t, `UPDATE kept_machines SET expires_at = now() - interval '1 hour' WHERE id = $1`, m.ID)
	k.inventory(t, runnerwire.MacVMKeptMachine{ID: m.ID.String(), Bytes: 1 << 30, StopRunID: k.run.ID.String(), ExpiresAt: later})
	k.sweep(t)
	got, ok := k.row(t, m.ID)
	if !ok || got.State != types.KeptMachineKept || !got.ExpiresAt.Equal(later) || len(k.mac.opsAsked()) != 0 {
		t.Fatalf("after the matching stop: recorded %v, %+v, calls %v; want kept until the Mac's %s and not deleted", ok, got, k.mac.opsAsked(), later)
	}
}

// TestKeptClaimThatNeverReachedTheMacIsGivenBack: the control plane stopped after it claimed a resumed
// machine for a run and before it sent the create; the owner then ended the run. The Mac reports the previous
// run's stop and never will report this one. Once the run has been over a while the sweep gives the machine
// back with the deadline it had when claimed (the run never made a sandbox), and the owner can resume it.
// A run that has only just ended still waits for its own stop.
func TestKeptClaimThatNeverReachedTheMacIsGivenBack(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
	before, _ := k.row(t, m.ID)
	earlier := runnerwire.MacVMKeptMachine{ID: m.ID.String(), Bytes: 1 << 30, StopRunID: uuid.NewString(), ExpiresAt: before.ExpiresAt}

	k.kill(t, k.run, types.ActorHuman, k.owner)
	k.inventory(t, earlier)
	k.sweep(t)
	if got, _ := k.row(t, m.ID); got.State != types.KeptMachineInUse {
		t.Fatalf("a run that ended just now: state %s, want in use", got.State)
	}
	k.exec(t, `UPDATE agent_runs SET ended_at = now() - interval '1 day' WHERE id = $1`, k.run.ID)
	k.sweep(t)
	got, ok := k.row(t, m.ID)
	if !ok || got.State != types.KeptMachineKept || !got.ExpiresAt.Equal(before.ExpiresAt) || got.Scrubbed || len(k.mac.opsAsked()) != 0 {
		t.Fatalf("after the sweep: recorded %v, %+v, Mac asked %v; want kept, unscrubbed, with the deadline it had (%s)", ok, got, k.mac.opsAsked(), before.ExpiresAt)
	}
	if _, err := k.st.BeginKeptMachineUse(k.ctx, m.ID, k.owner, k.newRun(t, k.owner, k.rn.ID).ID); err != nil {
		t.Fatalf("a later resume: %v", err)
	}
}

// TestKeptExpiryRefusedByTheMacMovesTheDeadlineForward: the organisation's deadline is behind the Mac's (a
// failed resume that the Mac recorded as a stop), so the Mac refuses the expiry delete. The row's deadline
// moves to the Mac's, and the next passes ask nothing and write no further audit rows.
func TestKeptExpiryRefusedByTheMacMovesTheDeadlineForward(t *testing.T) {
	k := newKeptFixture(t)
	m := k.expiredKept(t)
	macs := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	k.inventory(t, runnerwire.MacVMKeptMachine{ID: m.ID.String(), Bytes: 1 << 30, ExpiresAt: macs})
	k.mac.fail = keptMacRefuses
	k.sweep(t)
	if ops := k.mac.opsAsked(); len(ops) != 1 {
		t.Fatalf("first pass asked %v, want one delete_expired", ops)
	}
	got, ok := k.row(t, m.ID)
	if !ok || got.State != types.KeptMachineKept || !got.ExpiresAt.Equal(macs) {
		t.Fatalf("after the refusal: recorded %v, %+v; want kept until the Mac's %s", ok, got, macs)
	}
	rows := len(k.rows(t, "machine.delete", m.ID))
	k.sweep(t)
	k.sweep(t)
	if ops := k.mac.opsAsked(); len(ops) != 1 || len(k.rows(t, "machine.delete", m.ID)) != rows {
		t.Fatalf("later passes asked %v and wrote %d audit rows (was %d); want nothing more", ops, len(k.rows(t, "machine.delete", m.ID)), rows)
	}
	// The Mac names the row's run with a deadline that is already past (its clock is behind, or the machine is
	// locked by an update): the repair does not move the row back, so three passes ask once and audit once.
	k3 := newKeptFixture(t)
	m3 := k3.expiredKept(t)
	k3.mac.fail = keptMacRefuses
	k3.inventory(t, runnerwire.MacVMKeptMachine{ID: m3.ID.String(), Bytes: 1 << 30, StopRunID: k3.run.ID.String(), ExpiresAt: time.Now().Add(-time.Minute).UTC()})
	k3.sweep(t)
	k3.sweep(t)
	k3.sweep(t)
	if ops, rows := k3.mac.opsAsked(), k3.rows(t, "machine.delete", m3.ID); len(ops) != 1 || len(rows) != 1 {
		t.Fatalf("a named past deadline: calls %v, machine.delete rows %d; want one of each", ops, len(rows))
	}
	if got, _ := k3.row(t, m3.ID); got.ExpiresAt.Before(time.Now().Add(23 * time.Hour)) {
		t.Fatalf("a named past deadline left the row due at %s; want about a day on", got.ExpiresAt)
	}
	// Without the Mac's deadline in the inventory the row backs off a day.
	k2 := newKeptFixture(t)
	m2 := k2.expiredKept(t)
	k2.mac.fail = keptMacRefuses
	k2.inventory(t)
	k2.sweep(t)
	if got, _ := k2.row(t, m2.ID); got.ExpiresAt.Before(time.Now().Add(23 * time.Hour)) {
		t.Fatalf("a refusal with no deadline from the Mac left the row due at %s; want about a day on", got.ExpiresAt)
	}
}

// TestKeptRowOfAFolderTheMacSweptIsRemovedByTheOwner: a Mac of the earlier release removed an expired folder
// itself and never tells the organisation, so the row stays, counts against the owner's limit and refuses a
// replacement. The owner's own delete is the recovery: the Mac answers already_absent, the row goes, and a replacement
// can be created.
func TestKeptRowOfAFolderTheMacSweptIsRemovedByTheOwner(t *testing.T) {
	k := newKeptFixture(t)
	m := k.expiredKept(t)
	legacyInventoryOf(t, k.sub, runnerwire.MacVMKeptMachine{ID: m.ID.String(), Bytes: 1 << 30}) // listed: not yet swept away
	k.sweep(t)
	if _, ok := k.row(t, m.ID); !ok || len(k.mac.opsAsked()) != 0 {
		t.Fatalf("a listed machine of a legacy Mac: recorded %v, calls %v; want the row kept and nothing sent", ok, k.mac.opsAsked())
	}
	replacement := types.KeptMachine{Name: "again", OwnerSubject: k.owner, RunnerID: k.rn.ID, Base: "base-1",
		BasePreparedAt: time.Now().Add(-time.Hour).UTC(), RunID: &k.run.ID, ExpiresAt: time.Now().Add(time.Hour).UTC()}
	if _, err := k.st.CreateKeptMachine(k.ctx, replacement, 1); !errors.Is(err, store.ErrKeptMachineLimit) {
		t.Fatalf("a replacement at the limit: %v; want ErrKeptMachineLimit", err)
	}
	k.mac.mu.Lock()
	k.mac.deleted = append(k.mac.deleted, m.ID.String()) // the folder is already gone on the Mac
	k.mac.mu.Unlock()
	if w := k.deleteMine(k.owner, m.ID); w.Code >= 300 {
		t.Fatalf("the owner's delete = %d %s", w.Code, w.Body)
	}
	k.requireDeletedBy(t, m, "owner", runnerwire.KeptMachineAlreadyAbsent)
	if _, err := k.st.CreateKeptMachine(k.ctx, replacement, 1); err != nil {
		t.Fatalf("a replacement after the row was removed: %v", err)
	}
}

// TestKeptExpiredRowOfALegacyMacIsNeverClosedFromItsInventory: a Mac that cannot recheck its deadline removes its
// own expired folders, but an inventory that omits a machine is no proof the disk is gone (it skips an
// unreadable or malformed record, and may be a cached list). On a connected legacy runner the omitted, due row
// is kept, nothing is sent, and no successful machine.delete is written, however many passes run; the owner's
// delete is the recovery. Held elsewhere or with no inventory it is the same.
func TestKeptExpiredRowOfALegacyMacIsNeverClosedFromItsInventory(t *testing.T) {
	k := newKeptFixture(t)
	m := k.expiredKept(t)
	kept := func(when string) {
		t.Helper()
		got, ok := k.row(t, m.ID)
		if !ok || got.State != types.KeptMachineKept || len(k.mac.opsAsked()) != 0 || len(k.rows(t, "machine.delete", m.ID)) != 0 {
			t.Fatalf("%s: recorded %v, state %s, calls %v, machine.delete rows %v; want kept, none, none",
				when, ok, got.State, k.mac.opsAsked(), k.rows(t, "machine.delete", m.ID))
		}
	}
	// Caps with no inventory.
	data, err := json.Marshal(runnerwire.Caps{MacOSVM: &runnerwire.MacVMCaps{KeptMachines: true}})
	if err != nil {
		t.Fatal(err)
	}
	k.sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
	k.sweep(t)
	kept("no inventory")
	// Connected and held here, with an inventory that omits the machine.
	legacyInventoryOf(t, k.sub)
	for range 3 {
		k.sweep(t)
	}
	kept("an inventory that omits the machine")
	// Another replica's view: this one does not hold the Mac.
	k.setOnline(k.rn.ID, false)
	for range 3 {
		k.sweep(t)
	}
	kept("a Mac held elsewhere")
	// A registered Mac that never connected to this replica: its caps are not known here either.
	far := k.claimedMac(t, k.owner)
	farMachine := k.machine(t, k.owner, far.ID, k.newRun(t, k.owner, far.ID).ID, "far", types.KeptMachineKept)
	k.exec(t, `UPDATE kept_machines SET expires_at = now() - interval '1 hour' WHERE id = $1`, farMachine.ID)
	k.sweep(t)
	if got, ok := k.row(t, farMachine.ID); !ok || got.State != types.KeptMachineKept || len(k.rows(t, "machine.delete", farMachine.ID)) != 0 {
		t.Fatalf("a Mac never connected here: recorded %v, state %s, machine.delete rows %v; want kept and none", ok, got.State, k.rows(t, "machine.delete", farMachine.ID))
	}
	// The owner's delete is the recovery: the plain op, answered already_absent when the folder is gone.
	k.setOnline(k.rn.ID, true)
	k.mac.mu.Lock()
	k.mac.deleted = append(k.mac.deleted, m.ID.String())
	k.mac.mu.Unlock()
	if w := k.deleteMine(k.owner, m.ID); w.Code >= 300 {
		t.Fatalf("the owner's delete = %d %s", w.Code, w.Body)
	}
	k.requireDeletedBy(t, m, keptMachineByOwner, runnerwire.KeptMachineAlreadyAbsent)
}
