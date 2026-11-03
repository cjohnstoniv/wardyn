// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// setKeptIdleDays is the admin changing the kept-machine idle days while runs are active.
func (k *keptFixture) setKeptIdleDays(t *testing.T, days int) {
	t.Helper()
	on := true
	if _, err := k.st.PutSiteConfig(k.ctx, types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowKeptMachines: &on, KeptMachineIdleDays: days}}); err != nil {
		t.Fatal(err)
	}
}

// stopReport is the stop report of a normal (graceful) stop at k's clock with the expiry the Mac wrote.
func stopReport(k *keptFixture, at, expires time.Time) runner.MacOSVMDisk {
	d := diskReport(k.run.ID, &runner.KeptScrub{When: "stop", Outcome: keptScrubDone})
	d.At, d.KeptExpiresAt = at, expires
	return d
}

// TestKeptStopRecordsTheExpiryTheMacWrote: the setting changes between the run's admission and its
// normal stop, either way; the machine's expires_at is the runner's record (the idle days the run was
// launched with), not the days read at the stop.
func TestKeptStopRecordsTheExpiryTheMacWrote(t *testing.T) {
	for _, tc := range []struct {
		name          string
		admitted, now int
	}{{"1 to 30", 1, 30}, {"30 to 1", 30, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			k := newKeptFixture(t)
			k.setKeptIdleDays(t, tc.admitted)
			m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
			k.setKeptIdleDays(t, tc.now)
			at := time.Now().UTC().Truncate(time.Second)
			macExpiry := at.AddDate(0, 0, tc.admitted) // KeptStopUpdate: the stop time plus the launched spec's days
			k.srv.runnerMacOSVMDisks(k.ctx, k.rn.ID, []runner.MacOSVMDisk{stopReport(k, at, macExpiry)})
			got, _ := k.row(t, m.ID)
			if got.State != types.KeptMachineKept || !got.ExpiresAt.Equal(macExpiry) {
				t.Fatalf("after the stop: %s expires %s, want kept and the Mac's %s", got.State, got.ExpiresAt, macExpiry)
			}
		})
	}
}

// TestKeptStopExpiryIsClamped: a runner value before the stop or past the longest allowance is held to
// the bounds [stop, stop + 90 days].
func TestKeptStopExpiryIsClamped(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name           string
		reported, want time.Time
	}{
		{"too late", at.AddDate(0, 0, 400), at.AddDate(0, 0, types.KeptMachineIdleDaysMax)},
		{"too early", at.AddDate(0, 0, -5), at},
		{"at the bound", at.AddDate(0, 0, types.KeptMachineIdleDaysMax), at.AddDate(0, 0, types.KeptMachineIdleDaysMax)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newKeptFixture(t)
			m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
			k.srv.runnerMacOSVMDisks(k.ctx, k.rn.ID, []runner.MacOSVMDisk{stopReport(k, at, tc.reported)})
			if got, _ := k.row(t, m.ID); !got.ExpiresAt.Equal(tc.want) {
				t.Fatalf("reported %s: expires %s, want %s", tc.reported, got.ExpiresAt, tc.want)
			}
		})
	}
}

// TestKeptStopWithoutAnExpiryCountsTheSitesDays: an older runner reports none, and the machine expires
// the site's idle days after the stop, as before.
func TestKeptStopWithoutAnExpiryCountsTheSitesDays(t *testing.T) {
	k := newKeptFixture(t)
	k.setKeptIdleDays(t, 7)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
	before := time.Now().UTC()
	k.srv.runnerMacOSVMDisks(k.ctx, k.rn.ID, []runner.MacOSVMDisk{stopReport(k, before, time.Time{})})
	got, _ := k.row(t, m.ID)
	if lo, hi := before.AddDate(0, 0, 7), time.Now().UTC().AddDate(0, 0, 7); got.ExpiresAt.Before(lo.Add(-time.Second)) || got.ExpiresAt.After(hi.Add(time.Second)) {
		t.Fatalf("expires %s, want 7 days from the stop (%s to %s)", got.ExpiresAt, lo, hi)
	}
}

// TestKeptStopThatWroteNoRecordWaitsForTheMacsInventory: the runner could not write the record at the
// stop (a lock or write failure) and says the expiry is pending, while the setting went 1 to 30. No stop
// is recorded and no deadline invented; once the Mac recovers the session and lists the machine, the
// sweep keeps it with the Mac's own expiry.
func TestKeptStopThatWroteNoRecordWaitsForTheMacsInventory(t *testing.T) {
	k := newKeptFixture(t)
	k.setKeptIdleDays(t, 1)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
	k.setKeptIdleDays(t, 30)
	pending := stopReport(k, time.Now().UTC(), time.Time{})
	pending.KeptExpiryPending = true
	k.srv.runnerMacOSVMDisks(k.ctx, k.rn.ID, []runner.MacOSVMDisk{pending})
	if got, _ := k.row(t, m.ID); got.State != types.KeptMachineInUse {
		t.Fatalf("after the pending report: %s, want in_use with no deadline invented", got.State)
	}
	if keeps := k.rows(t, "machine.keep", m.ID); len(keeps) != 0 {
		t.Fatalf("machine.keep = %v, want none", keeps)
	}
	k.exec(t, `UPDATE agent_runs SET state = 'KILLED', ended_at = now() WHERE id = $1`, k.run.ID)
	macExpiry := time.Now().UTC().AddDate(0, 0, 1).Truncate(time.Second) // the recovery's: the record's own 1 day
	k.inventory(t, runnerwire.MacVMKeptMachine{ID: m.ID.String(), Bytes: 1 << 30, ExpiresAt: macExpiry})
	k.sweep(t)
	if got, _ := k.row(t, m.ID); got.State != types.KeptMachineKept || !got.ExpiresAt.Equal(macExpiry) {
		t.Fatalf("after the sweep: %s expires %s, want kept and the Mac's %s", got.State, got.ExpiresAt, macExpiry)
	}
}

// TestKeptReconcileExpiryIsCappedAtTheLongestAllowance: the stop report never came, and the Mac lists the
// machine idle with an expiry past the longest allowance; the sweep keeps it no later than now + 90 days.
func TestKeptReconcileExpiryIsCappedAtTheLongestAllowance(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
	k.exec(t, `UPDATE agent_runs SET state = 'KILLED', ended_at = now() WHERE id = $1`, k.run.ID)
	before := time.Now().UTC()
	k.inventory(t, runnerwire.MacVMKeptMachine{ID: m.ID.String(), Bytes: 1 << 30, ExpiresAt: before.AddDate(0, 0, 400)})
	k.sweep(t)
	got, _ := k.row(t, m.ID)
	lo, hi := before.AddDate(0, 0, types.KeptMachineIdleDaysMax), time.Now().UTC().AddDate(0, 0, types.KeptMachineIdleDaysMax)
	if got.State != types.KeptMachineKept || got.ExpiresAt.Before(lo.Add(-time.Second)) || got.ExpiresAt.After(hi.Add(time.Second)) {
		t.Fatalf("after the sweep: %s expires %s, want kept and capped at %s to %s", got.State, got.ExpiresAt, lo, hi)
	}
}

// TestKeptReconcileLeavesAnUnrecoveredRecordInUse: the run is over, but the Mac lists the machine in use
// because its record still names the session (the recovery has not run). The sweep keeps nothing and
// writes no deadline from the boot's old expiry; once the Mac lists it idle, it is kept with the Mac's.
func TestKeptReconcileLeavesAnUnrecoveredRecordInUse(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
	k.exec(t, `UPDATE agent_runs SET state = 'KILLED', ended_at = now() WHERE id = $1`, k.run.ID)
	old := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	k.inventory(t, runnerwire.MacVMKeptMachine{ID: m.ID.String(), Bytes: 1 << 30, InUse: true, RunID: k.run.ID.String(), ExpiresAt: old})
	k.sweep(t)
	if got, _ := k.row(t, m.ID); got.State != types.KeptMachineInUse {
		t.Fatalf("an unrecovered record: %s, want in_use", got.State)
	}
	if keeps := k.rows(t, "machine.keep", m.ID); len(keeps) != 0 {
		t.Fatalf("machine.keep = %v, want none while the Mac lists it in use", keeps)
	}
	macExpiry := time.Now().UTC().AddDate(0, 0, 3).Truncate(time.Second)
	k.inventory(t, runnerwire.MacVMKeptMachine{ID: m.ID.String(), Bytes: 1 << 30, ExpiresAt: macExpiry})
	k.sweep(t)
	if got, _ := k.row(t, m.ID); got.State != types.KeptMachineKept || !got.ExpiresAt.Equal(macExpiry) {
		t.Fatalf("after the recovery: %s expires %s, want kept and the Mac's %s", got.State, got.ExpiresAt, macExpiry)
	}
}
