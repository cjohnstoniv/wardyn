// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
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
	k.exec(t, `UPDATE agent_runs SET state = 'KILLED', ended_at = now(), sandbox_ref = 'sb-1' WHERE id = $1`, k.run.ID)
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
// report is not coming (an older runner never names the stop, or the report was lost), and the Mac's idle
// report is an earlier use's or unversioned. The machine is kept and unscrubbed with a fresh deadline of the
// site's idle days, never the report's past one, so the owner can resume or delete it and nothing expires in
// the same sweep; the Mac is not asked to delete.
func TestKeptUnmatchedIdleReportGivesTheMachineBackAfterTheGrace(t *testing.T) {
	past := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	for name, stop := range map[string]string{
		"an earlier stop's idle report": uuid.NewString(),
		"an unversioned idle report":    "",
	} {
		k := newKeptFixture(t)
		m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
		k.exec(t, `UPDATE kept_machines SET expires_at = now() - interval '1 hour', scrubbed = true WHERE id = $1`, m.ID)
		k.exec(t, `UPDATE agent_runs SET state = 'KILLED', ended_at = now() - interval '1 hour', sandbox_ref = 'sb-1' WHERE id = $1`, k.run.ID)
		k.inventory(t, runnerwire.MacVMKeptMachine{ID: m.ID.String(), Bytes: 1 << 30, StopRunID: stop, ExpiresAt: past, Scrubbed: true})
		k.sweep(t)
		got, ok := k.row(t, m.ID)
		fresh := time.Now().AddDate(0, 0, types.KeptMachineIdleDaysDefault)
		if !ok || got.State != types.KeptMachineKept || got.Scrubbed || got.ExpiresAt.Before(fresh.Add(-time.Hour)) || got.ExpiresAt.After(fresh.Add(time.Hour)) {
			t.Fatalf("%s: recorded %v, %+v; want kept, unscrubbed, due near %s", name, ok, got, fresh)
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

	// A stale kept row, a legacy Mac.
	legacyInventoryOf(t, k.sub)
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
	m := k.expiredKept(t)
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
