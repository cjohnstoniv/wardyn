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
	k.exec(t, `UPDATE agent_runs SET state = 'KILLED', ended_at = now() WHERE id = $1`, k.run.ID)
	past := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	for name, stop := range map[string]string{
		"an older stop's cached idle report": uuid.NewString(),
		"an unversioned idle report":         "",
	} {
		k.inventory(t, runnerwire.MacVMKeptMachine{ID: m.ID.String(), Bytes: 1 << 30, StopRunID: stop, ExpiresAt: past})
		k.sweep(t)
		if got, ok := k.row(t, m.ID); !ok || got.State != types.KeptMachineInUse {
			t.Fatalf("%s: recorded %v, state %s; want the machine still in use", name, ok, got.State)
		}
		if keeps := k.rows(t, "machine.keep", m.ID); len(keeps) != 0 || len(k.mac.asked()) != 0 {
			t.Fatalf("%s: machine.keep %v, Mac asked %v; want neither", name, keeps, k.mac.asked())
		}
	}
	later := time.Now().Add(10 * 24 * time.Hour).UTC().Truncate(time.Second)
	k.inventory(t, runnerwire.MacVMKeptMachine{ID: m.ID.String(), Bytes: 2 << 30, StopRunID: k.run.ID.String(), ExpiresAt: later})
	k.sweep(t)
	got, ok := k.row(t, m.ID)
	if !ok || got.State != types.KeptMachineKept || !got.ExpiresAt.Equal(later) || len(k.mac.asked()) != 0 {
		t.Fatalf("after the current report: recorded %v, %+v, Mac asked %v; want kept until the Mac's %s and not deleted", ok, got, k.mac.asked(), later)
	}
}

// TestKeptIdleReportOfAnEarlierRunDoesNotSettleALaterUse: the machine's current holder is a later run whose
// stop was not reported; an idle report that names the stop of the run before it does not settle that later
// run, whatever its deadline.
func TestKeptIdleReportOfAnEarlierRunDoesNotSettleALaterUse(t *testing.T) {
	k := newKeptFixture(t)
	earlier := uuid.NewString()
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
	k.exec(t, `UPDATE agent_runs SET state = 'KILLED', ended_at = now() WHERE id = $1`, k.run.ID)
	k.inventory(t, runnerwire.MacVMKeptMachine{ID: m.ID.String(), Bytes: 1 << 30, StopRunID: earlier, ExpiresAt: time.Now().Add(-time.Hour).UTC()})
	k.sweep(t)
	if got, _ := k.row(t, m.ID); got.State != types.KeptMachineInUse || len(k.mac.asked()) != 0 {
		t.Fatalf("after an earlier run's idle report: state %s, Mac asked %v; want in use and not asked", got.State, k.mac.asked())
	}
}
