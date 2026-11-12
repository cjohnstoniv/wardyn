// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// keptFixture is a prepared base and a kept machine cloned from it, with the person's work on its disk
// and a stop record that says the scrub ran.
type keptFixture struct {
	f    *prepFixture
	tf   *toolsFixture
	id   uuid.UUID
	base string
	dir  string
}

func newKeptFixture(t *testing.T) *keptFixture {
	t.Helper()
	f, tf, _ := stage2Prep(t)
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	k := &keptFixture{f: f, tf: tf, id: uuid.New(), base: f.opts.Layout.BaseDir(0)}
	k.dir = f.opts.Layout.KeptMachineDir(k.id.String())
	now := time.Now().UTC().Truncate(time.Second)
	if err := openKept(t, keptCfg(k.base, macvm.KeptNew, k.id), k.dir, now); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(k.dir, macvm.DiskFile), []byte("persons-work"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, err := macvm.ReadKept(k.dir, k.id.String())
	if err != nil {
		t.Fatal(err)
	}
	rec.Scrubbed, rec.StopKind, rec.LastStop = true, macvm.KeptStopGraceful, now
	if err := macvm.WriteKept(k.dir, rec); err != nil {
		t.Fatal(err)
	}
	return k
}

// newPins moves the shipped tooling to a new harness pin and guest agent build, as a new Mac release does,
// and returns the guest that serves it.
func (k *keptFixture) newPins(t *testing.T) *guest {
	t.Helper()
	tf := k.tf
	tf.claude = []byte("claude-binary-2.1.311")
	tf.tooling.Harnesses[0].Version, tf.tooling.Harnesses[0].SHA256, tf.tooling.Harnesses[0].Size = "2.1.311", shaHex(tf.claude), int64(len(tf.claude))
	tf.opts.Tooling = tf.tooling
	bin := filepath.Join(t.TempDir(), "agent")
	_ = os.WriteFile(bin, []byte("agent 0.10.1"), 0o600)
	tf.opts.Agent = &macvm.AgentPin{Version: "0.10.1", Path: bin, Dest: "/usr/local/libexec/wardyn-guest-agent"}
	g := newGuest()
	g.version["/usr/local/bin/claude"] = "claude 2.1.311"
	return g
}

func (k *keptFixture) opts(g *guest, agent string) macvm.UpdateOptions {
	o := updateOpts(k.f, k.tf, g, agent)
	o.KeptDir = k.dir
	return o
}

// A kept machine's update runs on its own disk and tools record, keeps its identity, record and the
// person's work, and leaves the base as it was.
func TestUpdateToolsKeptUpdatesTheKeptFolderAndNotTheBase(t *testing.T) {
	k := newKeptFixture(t)
	if macvm.NeedsToolsUpdate(k.dir, k.tf.tooling, "0.10.0", time.Now()) {
		t.Fatal("a machine cloned from a current base wants an update")
	}
	idBefore, _ := os.ReadFile(filepath.Join(k.dir, macvm.MachineIDFile))
	baseDisk, _ := os.ReadFile(filepath.Join(k.base, macvm.DiskFile))
	baseRec, _ := macvm.ReadToolsRecord(k.base)

	g := k.newPins(t)
	if !macvm.NeedsToolsUpdate(k.dir, k.tf.tooling, "0.10.1", time.Now()) {
		t.Fatal("a kept machine behind the Mac's pins does not want an update")
	}
	m, changed, err := macvm.UpdateTools(context.Background(), k.opts(g, "0.10.1"))
	if err != nil || !changed {
		t.Fatalf("update: changed %v err %v", changed, err)
	}
	if m.AgentVersion != "0.10.1" {
		t.Fatalf("manifest = %+v", m)
	}
	if got, err := macvm.ReadyBase(k.dir); err != nil || got.AgentVersion != "0.10.1" {
		t.Fatalf("kept manifest after the swap: %+v %v", got, err)
	}
	if macvm.NeedsToolsUpdate(k.dir, k.tf.tooling, "0.10.1", time.Now()) {
		t.Fatal("an updated kept machine still wants an update")
	}
	if got, _ := os.ReadFile(filepath.Join(k.dir, macvm.DiskFile)); string(got) != "persons-work" {
		t.Fatalf("the person's disk = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(k.dir, macvm.MachineIDFile)); string(got) != string(idBefore) {
		t.Fatalf("machine id %q became %q", idBefore, got)
	}
	rec, err := macvm.ReadKept(k.dir, k.id.String())
	if err != nil || rec.OwnerHash != keptOwner || rec.RunnerID != "runner-1" || rec.Scrubbed {
		t.Fatalf("record after the update: %+v %v (the stage-2 boot wrote to the disk, so scrubbed is false)", rec, err)
	}
	// The base is as it was: same manifest, disk and tools record, still behind the new pins.
	if bm, err := macvm.ReadyBase(k.base); err != nil || bm.AgentVersion != "0.10.0" {
		t.Fatalf("base manifest: %+v %v", bm, err)
	}
	if got, _ := os.ReadFile(filepath.Join(k.base, macvm.DiskFile)); string(got) != string(baseDisk) {
		t.Fatal("the base's disk changed")
	}
	if got, _ := macvm.ReadToolsRecord(k.base); got.AgentVersion != baseRec.AgentVersion || got.Pins["claude-code"] != baseRec.Pins["claude-code"] {
		t.Fatalf("the base's tools record changed: %+v", got)
	}
	if !macvm.NeedsToolsUpdate(k.base, k.tf.tooling, "0.10.1", time.Now()) {
		t.Fatal("the base was updated")
	}
	// Nothing is left beside the folder but its lock.
	ents, _ := os.ReadDir(filepath.Dir(k.dir))
	for _, e := range ents {
		if n := e.Name(); n != filepath.Base(k.dir) && n != filepath.Base(k.dir)+".lock" && n != filepath.Base(k.dir)+".update.lock" {
			t.Fatalf("%s left in kept/", n)
		}
	}
	files, _ := os.ReadDir(k.dir)
	for _, e := range files {
		if !e.Type().IsRegular() {
			t.Fatalf("%s is not a regular file", e.Name())
		}
	}
}

// A current kept machine runs nothing, and its folder is not rewritten.
func TestUpdateToolsKeptWhenCurrentRunsNothing(t *testing.T) {
	k := newKeptFixture(t)
	g := newGuest()
	_, changed, err := macvm.UpdateTools(context.Background(), k.opts(g, "0.10.0"))
	if err != nil || changed || len(g.runs) != 0 {
		t.Fatalf("no-op update: changed %v err %v runs %d", changed, err, len(g.runs))
	}
	if rec, _ := macvm.ReadKept(k.dir, k.id.String()); !rec.Scrubbed {
		t.Fatal("a no-op update rewrote kept.json")
	}
}

// A machine another process holds is ErrKeptLocked, nothing runs, and the lock is not taken from its holder.
func TestUpdateToolsKeptIsLockedOutByABoot(t *testing.T) {
	k := newKeptFixture(t)
	unlock, err := macvm.LockKept(k.dir)
	if err != nil {
		t.Fatal(err)
	}
	g := k.newPins(t)
	_, changed, err := macvm.UpdateTools(context.Background(), k.opts(g, "0.10.1"))
	if !errors.Is(err, macvm.ErrKeptLocked) || changed || len(g.runs) != 0 {
		t.Fatalf("update of a held machine: changed %v err %v runs %d", changed, err, len(g.runs))
	}
	if locked, _ := macvm.KeptLocked(k.dir); !locked {
		t.Fatal("the refused update released its holder's lock")
	}
	unlock()
	// The update holds the same lock for its whole run: a boot meanwhile is refused.
	var during error
	o := k.opts(g, "0.10.1")
	o.Stage = &probeStage2{next: o.Stage, on: func() { during = openKept(t, keptCfg(k.base, macvm.KeptResume, k.id), k.dir, time.Now()) }}
	if _, changed, err := macvm.UpdateTools(context.Background(), o); err != nil || !changed {
		t.Fatalf("update: changed %v err %v", changed, err)
	}
	if !errors.Is(during, macvm.ErrKeptLocked) {
		t.Fatalf("a boot during the update: %v, want ErrKeptLocked", during)
	}
	if locked, _ := macvm.KeptLocked(k.dir); locked {
		t.Fatal("the update left the machine locked")
	}
}

// probeStage2 calls on while the update's stage 2 runs.
type probeStage2 struct {
	next macvm.Stage2
	on   func()
}

func (s *probeStage2) Run(ctx context.Context, in macvm.Stage2Input) (macvm.ToolsResult, error) {
	s.on()
	return s.next.Run(ctx, in)
}

// A failed update leaves the kept machine as it was, and no staging copy.
func TestUpdateToolsKeptFailureKeepsTheOldMachine(t *testing.T) {
	k := newKeptFixture(t)
	o := k.opts(k.newPins(t), "0.10.1")
	o.Stage = &failingStage2{next: o.Stage}
	if _, changed, err := macvm.UpdateTools(context.Background(), o); err == nil || changed {
		t.Fatalf("update: changed %v err %v", changed, err)
	}
	if m, err := macvm.ReadyBase(k.dir); err != nil || m.AgentVersion != "0.10.0" {
		t.Fatalf("old machine damaged: %+v %v", m, err)
	}
	if rec, err := macvm.ReadKept(k.dir, k.id.String()); err != nil || !rec.Scrubbed {
		t.Fatalf("old record damaged: %+v %v", rec, err)
	}
	if got, _ := os.ReadFile(filepath.Join(k.dir, macvm.DiskFile)); string(got) != "persons-work" {
		t.Fatalf("disk = %q", got)
	}
	if _, err := os.Stat(k.dir + ".update"); !os.IsNotExist(err) {
		t.Fatal("the failed clone was left behind")
	}
}

// A failed exchange, or a new folder holding more than a kept machine's files, leaves the old machine.
func TestUpdateToolsKeptSwapBackWhenTheNewFolderIsWrong(t *testing.T) {
	k := newKeptFixture(t)
	o := k.opts(k.newPins(t), "0.10.1")
	o.Stage = &probeStage2{next: o.Stage, on: func() {
		// What a stage-2 boot must never leave: a socket-like extra file in the clone.
		_ = os.WriteFile(k.dir+".update/ctl.sock", nil, 0o600)
	}}
	if _, changed, err := macvm.UpdateTools(context.Background(), o); err == nil || changed {
		t.Fatalf("update: changed %v err %v", changed, err)
	}
	if m, err := macvm.ReadyBase(k.dir); err != nil || m.AgentVersion != "0.10.0" {
		t.Fatalf("old machine not back: %+v %v", m, err)
	}
	if _, err := os.Stat(filepath.Join(k.dir, "ctl.sock")); !os.IsNotExist(err) {
		t.Fatal("the extra file reached the kept folder")
	}
}

// A folder that is not a kept machine's of this Mac is refused before any lock is made: the base, a
// missing machine, another Mac's layout, and a folder whose record names another id.
func TestUpdateToolsKeptRefusesWhatIsNotAKeptFolder(t *testing.T) {
	k := newKeptFixture(t)
	g := k.newPins(t)
	missing := k.f.opts.Layout.KeptMachineDir(uuid.NewString())
	other := t.TempDir()
	wrong := k.f.opts.Layout.KeptMachineDir(uuid.NewString())
	if err := macvm.WriteKept(wrong, macvm.KeptRecord{ID: k.id.String()}); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"the base": k.base, "a missing machine": missing, "another root": filepath.Join(other, "kept", "x"),
		"the kept folder": filepath.Dir(k.dir), "a record naming another id": wrong} {
		o := k.opts(g, "0.10.1")
		o.KeptDir = dir
		if _, changed, err := macvm.UpdateTools(context.Background(), o); err == nil || changed {
			t.Fatalf("%s: changed %v err %v", name, changed, err)
		}
	}
	if len(g.runs) != 0 {
		t.Fatal("a refused update ran stage 2")
	}
	if _, err := os.Lstat(missing + ".lock"); !os.IsNotExist(err) {
		t.Fatal("the refused update made a lock file for a machine that is not there")
	}
	if bm, err := macvm.ReadyBase(k.base); err != nil || bm.AgentVersion != "0.10.0" {
		t.Fatalf("base touched: %+v %v", bm, err)
	}
}
