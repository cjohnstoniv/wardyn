// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
)

// keptToolsRig is a runner with a kept machine whose tools record is current (m.version, the shipped pins
// and an empty install list), and a job starter that records the jobs it is given.
type keptToolsRig struct {
	m    *macOSVMs
	u    *keptToolsUpdater
	dir  string
	jobs []helperJob
	err  error
}

func newKeptToolsRig(t *testing.T) *keptToolsRig {
	t.Helper()
	was := keptLockWait
	keptLockWait = 20 * time.Millisecond
	t.Cleanup(func() { keptLockWait = was })
	r := newMacLedgerRig(t)
	m := r.open(t)
	m.helper = "/opt/wardyn/wardyn-macvm"
	k := &keptToolsRig{m: m, dir: m.layout.KeptMachineDir(uuid.NewString())}
	k.u = newKeptToolsUpdater(m)
	k.u.start = func(_ context.Context, j helperJob) error { k.jobs = append(k.jobs, j); return k.err }
	if err := os.MkdirAll(k.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	man := `{"format":2,"state":"ready","guest_os":"macOS 26.1","agent_version":"1.0.0","min_cpus":4,"min_memory_mib":8192,"desktop":"control","harnesses":["claude-code"]}`
	if err := os.WriteFile(filepath.Join(k.dir, macvm.ManifestFile), []byte(man), 0o600); err != nil {
		t.Fatal(err)
	}
	k.writeRecord(t, m.version)
	return k
}

// writeRecord writes the machine's tools.json as an update by a runner of agent build agent left it.
func (k *keptToolsRig) writeRecord(t *testing.T, agent string) {
	t.Helper()
	tooling, err := macvm.ShippedTooling()
	if err != nil {
		t.Fatal(err)
	}
	rec := macvm.ToolsRecord{AgentVersion: agent, AgentRun: true, Pins: map[string]macvm.PinRecord{}, TriedAt: time.Now(), ItemsDigest: macvm.BaseList{}.Digest()}
	for _, p := range tooling.Harnesses {
		rec.Pins[p.ID] = macvm.PinRecord{Version: p.Version, SHA256: p.SHA256}
	}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(k.dir, macvm.ToolsRecordFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A machine behind the Mac's guest agent gets one update-tools job naming its folder and no other, under a
// tools-update entry sized from its own manifest, run to its end.
func TestKeptToolsUpdateRunsTheHelperOnTheKeptFolderOnly(t *testing.T) {
	k := newKeptToolsRig(t)
	k.writeRecord(t, "0.9.9")
	if err := k.u.UpdateKeptTools(context.Background(), k.dir); err != nil {
		t.Fatal(err)
	}
	if len(k.jobs) != 1 {
		t.Fatalf("%d jobs, want 1", len(k.jobs))
	}
	j := k.jobs[0]
	i := slices.Index(j.extra, "--"+macvm.FlagKept)
	if j.kind != admission.KindToolsUpdate || j.sub != macvm.UpdateToolsCommand || i < 0 || j.extra[i+1] != k.dir || j.out == nil ||
		j.cpus != 4 || j.memMiB < 8192 || j.log != filepath.Join(k.m.layout.Root, keptToolsUpdateLog) || j.runner != "runner-a" {
		t.Fatalf("job = %+v", j)
	}
	for _, a := range j.extra {
		if a == "--"+macvm.FlagForce || a == k.m.layout.BaseDir(0) {
			t.Fatalf("an automatic kept update carries %q: %v", a, j.extra)
		}
	}
}

// A current machine starts nothing: no helper, no ledger entry.
func TestKeptToolsUpdateSkipsACurrentMachine(t *testing.T) {
	k := newKeptToolsRig(t)
	if err := k.u.UpdateKeptTools(context.Background(), k.dir); err != nil || len(k.jobs) != 0 {
		t.Fatalf("err %v, jobs %d", err, len(k.jobs))
	}
	counted, _ := k.m.ledger.Counted()
	if len(counted) != 0 {
		t.Fatalf("%d ledger entries", len(counted))
	}
}

// Each thing that makes a machine behind is noticed from its own record: a new agent build, a changed pin,
// a missing record, and a changed install list.
func TestKeptToolsUpdateNoticesEveryWayToBeBehind(t *testing.T) {
	for name, mutate := range map[string]func(*testing.T, *keptToolsRig){
		"agent build": func(t *testing.T, k *keptToolsRig) { k.writeRecord(t, "0.9.9") },
		"no record":   func(t *testing.T, k *keptToolsRig) { _ = os.Remove(filepath.Join(k.dir, macvm.ToolsRecordFile)) },
		"pin": func(t *testing.T, k *keptToolsRig) {
			rec, err := macvm.ReadToolsRecord(k.dir)
			if err != nil {
				t.Fatal(err)
			}
			for id := range rec.Pins {
				rec.Pins[id] = macvm.PinRecord{Version: "0.0.1"}
			}
			b, _ := json.Marshal(rec)
			_ = os.WriteFile(filepath.Join(k.dir, macvm.ToolsRecordFile), b, 0o600)
		},
		"install list": func(t *testing.T, k *keptToolsRig) {
			rec, _ := macvm.ReadToolsRecord(k.dir)
			rec.ItemsDigest = "an older list"
			b, _ := json.Marshal(rec)
			_ = os.WriteFile(filepath.Join(k.dir, macvm.ToolsRecordFile), b, 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			k := newKeptToolsRig(t)
			mutate(t, k)
			if err := k.u.UpdateKeptTools(context.Background(), k.dir); err != nil || len(k.jobs) != 1 {
				t.Fatalf("err %v, jobs %d", err, len(k.jobs))
			}
		})
	}
}

// Force runs a current machine's update and tells the helper to retry every pin.
func TestKeptToolsForceUpdatesACurrentMachine(t *testing.T) {
	k := newKeptToolsRig(t)
	if err := k.u.ForceKeptTools(context.Background(), k.dir); err != nil || len(k.jobs) != 1 {
		t.Fatalf("err %v, jobs %d", err, len(k.jobs))
	}
	if !slices.Contains(k.jobs[0].extra, "--"+macvm.FlagForce) || !slices.Contains(k.jobs[0].extra, k.dir) {
		t.Fatalf("extra = %v", k.jobs[0].extra)
	}
}

// A machine a boot holds is ErrKeptLocked before any job starts, and so is one the helper reports locked.
func TestKeptToolsUpdateReportsALockedMachine(t *testing.T) {
	k := newKeptToolsRig(t)
	k.writeRecord(t, "0.9.9")
	unlock, err := macvm.LockKept(k.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.u.UpdateKeptTools(context.Background(), k.dir); !errors.Is(err, macvm.ErrKeptLocked) || len(k.jobs) != 0 {
		t.Fatalf("err %v, jobs %d", err, len(k.jobs))
	}
	unlock()
	k.err = errors.New("wardyn-macvm: " + macvm.ErrKeptLocked.Error())
	if err := k.u.UpdateKeptTools(context.Background(), k.dir); !errors.Is(err, macvm.ErrKeptLocked) {
		t.Fatalf("helper's lock answer = %v", err)
	}
	k.err = errors.New("power lost")
	if err := k.u.UpdateKeptTools(context.Background(), k.dir); err == nil || errors.Is(err, macvm.ErrKeptLocked) {
		t.Fatalf("another failure = %v", err)
	}
}

// Anything but a kept machine folder of this Mac, the base first, starts no job.
func TestKeptToolsUpdateNeverTakesTheBase(t *testing.T) {
	k := newKeptToolsRig(t)
	for name, dir := range map[string]string{"base": k.m.layout.BaseDir(0), "kept root": k.m.layout.KeptDir(), "another root": filepath.Join(t.TempDir(), "kept", "abc"),
		"a missing machine": k.m.layout.KeptMachineDir(uuid.NewString())} {
		if err := k.u.ForceKeptTools(context.Background(), dir); err == nil {
			t.Fatalf("%s was updated", name)
		}
	}
	if len(k.jobs) != 0 {
		t.Fatalf("%d jobs started", len(k.jobs))
	}
}

// A runner with no ledger starts nothing and says why.
func TestKeptToolsUpdateWithoutALedger(t *testing.T) {
	k := newKeptToolsRig(t)
	k.m.ledger, k.m.refusal = nil, macOSVMLedgerDamaged
	if err := k.u.ForceKeptTools(context.Background(), k.dir); err == nil || err.Error() != macOSVMLedgerDamaged || len(k.jobs) != 0 {
		t.Fatalf("err %v, jobs %d", err, len(k.jobs))
	}
}

// A holder that lets go within the short wait (a stop write) does not refuse an update; one that stays does.
func TestKeptToolsUpdateWaitsBrieflyForAHolder(t *testing.T) {
	k := newKeptToolsRig(t)
	k.writeRecord(t, "0.9.9")
	keptLockWait = 2 * time.Second
	unlock, err := macvm.LockKept(k.dir)
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(100 * time.Millisecond); unlock() }()
	if err := k.u.UpdateKeptTools(context.Background(), k.dir); err != nil || len(k.jobs) != 1 {
		t.Fatalf("a brief holder: err %v, jobs %d", err, len(k.jobs))
	}
}
