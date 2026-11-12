// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build macvm_live

package macvmlive

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
)

// The kept-machine live rows drive the real helper (`wardyn-macvm run --dir`) the way the runner does: an
// admission entry, a run folder with a run.json that carries macvm.KeptRun, then the helper's own clone,
// lock and kept.json. A row observes the kept folder and kept.json, never a guest: the helper has cloned
// and written kept.json by the time it listens on ctl.sock, and the row stops it there, before any VM
// boots (a boot needs the run's network, which only the runner can hand over). What only the runner or the
// guest agent does (the stop scrub, the stop update, the limits) is not driven here; the rows that touch it
// make the runner's one file change themselves (stopLikeRunner) or say it is untested.
//
// The rows are named TestKeptLive_*, not TestP*: TestSummaryRowsMatchTests pins the TestP list to the
// window's 20 rows. Run them with run.sh's pattern argument: run.sh <bin> <out> '^TestKeptLive_'.

const keptIdleDays = 3

// keptHelper is the helper binary, WARDYN_MACVM_HELPER or wardyn-macvm on PATH.
func keptHelper(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath(env("WARDYN_MACVM_HELPER", "wardyn-macvm"))
	if err != nil {
		t.Skipf("NOT_RUN: no wardyn-macvm helper (set WARDYN_MACVM_HELPER): %v", err)
	}
	return p
}

// keptBoot is one helper process started for a kept machine.
type keptBoot struct {
	t      *testing.T
	runID  string
	dir    string
	led    *admission.Ledger
	entry  admission.Entry
	cmd    *exec.Cmd
	done   chan struct{}
	exited bool
}

// bootKept admits a run, writes its run.json with k and starts the helper. It returns when the helper
// listens on its ctl.sock (it has cloned or checked the kept folder) or has exited (it refused). The boot
// is ended when the test ends.
func bootKept(t *testing.T, k macvm.KeptRun) *keptBoot {
	t.Helper()
	lay := layout(t)
	led, err := admission.Open(admission.Config{Dir: admission.DefaultDir, UID: os.Getuid(), RunnerID: runnerID(t),
		Volume: lay.Root, Host: admission.SystemHost()})
	if err != nil {
		t.Fatalf("admission ledger: %v", err)
	}
	self := os.Getpid()
	start, err := admission.Self(admission.SystemHost(), self)
	if err != nil {
		t.Fatalf("own start time: %v", err)
	}
	b := &keptBoot{t: t, runID: uuid.NewString(), led: led, done: make(chan struct{})}
	b.dir = lay.RunDir(b.runID)
	b.entry, err = led.Admit(admission.Request{Kind: admission.KindRun, RunID: b.runID, GuestCPUs: 2, GuestMemory: 4 << 30,
		DiskReserve: admission.RunDiskReservation(macvm.BaseDiskBytes), OwnerPID: self, OwnerStart: start})
	if err != nil {
		t.Fatalf("admit a run for the kept machine: %v", err)
	}
	t.Cleanup(b.end)
	base := env("WARDYN_MACVM_BASE_DIR", lay.BaseDir(0))
	if err := macvm.WriteRunConfig(b.dir, macvm.RunConfig{RunID: b.runID, BaseDir: base, CPUs: 2, MemoryMiB: 4096,
		Identity: macvm.IdentityFresh, RunnerID: runnerID(t), LedgerEntry: b.entry.ID, Kept: &k}); err != nil {
		t.Fatalf("run.json: %v", err)
	}
	b.cmd = exec.Command(keptHelper(t), "run", "--dir", b.dir)
	b.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := b.cmd.Start(); err != nil {
		t.Fatalf("start the helper: %v", err)
	}
	go func() { _ = b.cmd.Wait(); close(b.done) }()
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-b.done:
			b.exited = true
			return b
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		cc, err := macvm.DialControl(ctx, ctlSock(b.runID))
		cancel()
		if err == nil {
			cc.Close()
			return b
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("the helper for run %s neither listened nor exited within 5 minutes", b.runID)
	return b
}

// refusal is the kept_refusal code the helper left in the run folder, "" when it left none.
func (b *keptBoot) refusal() string {
	b.t.Helper()
	code, err := macvm.ReadKeptRefusal(b.dir)
	if err != nil {
		b.t.Fatalf("kept-refusal.json: %v", err)
	}
	return code
}

// mustListen fails the boot unless the helper is still up (it was not refused).
func (b *keptBoot) mustListen() {
	b.t.Helper()
	if b.exited {
		b.t.Fatalf("the helper for run %s exited (kept_refusal %q); log %s", b.runID, b.refusal(), filepath.Join(b.dir, macvm.HelperLog))
	}
}

// mustRefuse fails unless the helper exited with the kept_refusal code (code "" means it exited with none).
func (b *keptBoot) mustRefuse(code string) {
	b.t.Helper()
	if !b.exited {
		b.t.Fatalf("the helper for run %s is up; want it to refuse with %q", b.runID, code)
	}
	if got := b.refusal(); got != code {
		b.t.Fatalf("kept_refusal = %q, want %q", got, code)
	}
}

// end stops the helper (a hard stop over ctl.sock, then SIGTERM, then SIGKILL), removes the run folder and
// releases the admission entry. It is safe to call twice.
func (b *keptBoot) end() {
	b.t.Helper()
	if b.cmd == nil {
		return
	}
	select {
	case <-b.done:
	default:
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if cc, err := macvm.DialControl(ctx, ctlSock(b.runID)); err == nil {
			_ = cc.Stop(ctx, false, 15*time.Second)
			cc.Close()
		}
		cancel()
		for _, step := range []struct {
			sig  syscall.Signal
			wait time.Duration
		}{{0, 20 * time.Second}, {syscall.SIGTERM, 20 * time.Second}, {syscall.SIGKILL, 20 * time.Second}} {
			if step.sig != 0 {
				_ = b.cmd.Process.Signal(step.sig)
			}
			select {
			case <-b.done:
			case <-time.After(step.wait):
				continue
			}
			break
		}
	}
	b.cmd = nil
	_ = os.RemoveAll(b.dir)
	if err := b.led.Release(b.entry.ID, admission.Destroyed{Stopped: true, RunDir: b.dir}); err != nil {
		b.t.Errorf("release the admission entry %s: %v", b.entry.ID, err)
	}
}

// keptMachine is one kept machine's id, owner and folder.
type keptMachine struct {
	id, dir, owner string
}

// newKeptMachine names a machine that does not exist yet and removes its folder when the test ends.
func newKeptMachine(t *testing.T) keptMachine {
	t.Helper()
	id := uuid.NewString()
	m := keptMachine{id: id, dir: layout(t).KeptMachineDir(id), owner: "macvm-live-" + id[:8]}
	t.Cleanup(func() { _, _ = macvm.RemoveKept(m.dir) })
	return m
}

// run is the KeptRun for mode, pinned to the base unless digest says otherwise.
func (m keptMachine) run(t *testing.T, mode string, idleDays int, digest string) macvm.KeptRun {
	t.Helper()
	if digest == "" {
		d, err := macvm.BaseDigest(env("WARDYN_MACVM_BASE_DIR", layout(t).BaseDir(0)))
		if err != nil {
			t.Fatalf("base digest: %v", err)
		}
		digest = d
	}
	return macvm.KeptRun{ID: m.id, Mode: mode, OwnerHash: m.owner, RunnerID: runnerID(t), Base: "macvm-live", BaseDigest: digest, IdleDays: idleDays}
}

func (m keptMachine) record(t *testing.T) macvm.KeptRecord {
	t.Helper()
	rec, err := macvm.ReadKept(m.dir, m.id)
	if err != nil {
		t.Fatalf("kept.json of %s: %v", m.id, err)
	}
	return rec
}

// disk is the identity of the kept disk: its inode and the machine-id file's bytes.
func (m keptMachine) disk(t *testing.T) (ino uint64, machineID []byte) {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(filepath.Join(m.dir, macvm.DiskFile), &st); err != nil {
		t.Fatalf("kept disk: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(m.dir, macvm.MachineIDFile))
	if err != nil {
		t.Fatalf("kept machine-id: %v", err)
	}
	return st.Ino, b
}

// stopLikeRunner is the runner's stop update of kept.json, under the folder's lock (KeptStopUpdate).
func stopLikeRunner(dir, id, runID string, now time.Time, idleDays int) error {
	unlock, err := macvm.LockKept(dir)
	if err != nil {
		return err
	}
	defer unlock()
	rec, err := macvm.ReadKept(dir, id)
	if err != nil {
		return err
	}
	if !macvm.KeptStopUpdate(&rec, runID, now, idleDays) {
		return errors.New("the stop is not the active run's")
	}
	rec.LastStop, rec.StopKind = now, macvm.KeptStopHard
	return macvm.WriteKept(dir, rec)
}

// near reports whether got is within tol of want.
func near(got, want time.Time, tol time.Duration) bool {
	d := got.Sub(want)
	return d >= -tol && d <= tol
}

func liveKept(t *testing.T) {
	t.Helper()
	live(t)
	keptHelper(t)
}

// TestKeptLive_KeepAndResume: a keep makes the folder and kept.json; a second keep of the same id is
// refused; a resume keeps the disk and refreshes the expiry; an expired or foreign machine is refused.
func TestKeptLive_KeepAndResume(t *testing.T) {
	liveKept(t)
	m := newKeptMachine(t)
	b1 := bootKept(t, m.run(t, macvm.KeptNew, keptIdleDays, ""))
	b1.mustListen()
	rec := m.record(t)
	if rec.ActiveRun != b1.runID || rec.Scrubbed || rec.OwnerHash != m.owner || rec.RunnerID != runnerID(t) {
		t.Fatalf("keep: kept.json %+v", rec)
	}
	if !near(rec.ExpiresAt, time.Now().AddDate(0, 0, keptIdleDays), time.Hour) {
		t.Fatalf("keep: expires_at %s, want about %d days out", rec.ExpiresAt, keptIdleDays)
	}
	ino, mid := m.disk(t)

	t.Run("second_keep_refused", func(t *testing.T) {
		// While the first boot holds the machine: its lock refuses a second boot of the same id.
		b := bootKept(t, m.run(t, macvm.KeptNew, keptIdleDays, ""))
		b.mustRefuse(macvm.KeptRefusalLocked)
		if got := m.record(t); got.ActiveRun != b1.runID {
			t.Fatalf("a refused keep changed kept.json: %+v", got)
		}
	})

	b1.end()
	first := time.Now().UTC()
	if err := stopLikeRunner(m.dir, m.id, b1.runID, first, keptIdleDays); err != nil {
		t.Fatal(err)
	}
	stopped := m.record(t)
	if stopped.ActiveRun != "" || !near(stopped.ExpiresAt, first.AddDate(0, 0, keptIdleDays), time.Minute) {
		t.Fatalf("stop: kept.json %+v", stopped)
	}

	t.Run("second_keep_of_a_stopped_machine_refused", func(t *testing.T) {
		b := bootKept(t, m.run(t, macvm.KeptNew, keptIdleDays, ""))
		b.mustRefuse("")
		if i, _ := m.disk(t); i != ino {
			t.Fatal("a refused keep replaced the kept disk")
		}
	})

	t.Run("resume_keeps_the_disk_and_refreshes_the_expiry", func(t *testing.T) {
		b2 := bootKept(t, m.run(t, macvm.KeptResume, 10, ""))
		b2.mustListen()
		got := m.record(t)
		if got.ActiveRun != b2.runID || got.Scrubbed {
			t.Fatalf("resume: kept.json %+v", got)
		}
		if !got.ExpiresAt.After(stopped.ExpiresAt) || !near(got.ExpiresAt, time.Now().AddDate(0, 0, 10), time.Hour) {
			t.Fatalf("resume did not refresh the expiry: %s then %s", stopped.ExpiresAt, got.ExpiresAt)
		}
		if i, id2 := m.disk(t); i != ino || !bytes.Equal(id2, mid) {
			t.Fatal("resume changed the kept disk or its machine-id")
		}
		b2.end()
		if err := stopLikeRunner(m.dir, m.id, b2.runID, time.Now().UTC(), 10); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("another_owner_refused", func(t *testing.T) {
		k := m.run(t, macvm.KeptResume, keptIdleDays, "")
		k.OwnerHash += "-other"
		bootKept(t, k).mustRefuse(macvm.KeptRefusalOwner)
	})

	t.Run("expired_refused", func(t *testing.T) {
		unlock, err := macvm.LockKept(m.dir)
		if err != nil {
			t.Fatal(err)
		}
		rec := m.record(t)
		rec.ExpiresAt = time.Now().Add(-time.Hour)
		err = macvm.WriteKept(m.dir, rec)
		unlock()
		if err != nil {
			t.Fatal(err)
		}
		bootKept(t, m.run(t, macvm.KeptResume, keptIdleDays, "")).mustRefuse(macvm.KeptRefusalExpired)
	})
}

// TestKeptLive_Scrub: the helper never marks a machine scrubbed: every boot clears the flag, so only the
// runner's stop scrub can set it. The scrub itself is in the guest.
func TestKeptLive_Scrub(t *testing.T) {
	liveKept(t)
	m := newKeptMachine(t)
	b1 := bootKept(t, m.run(t, macvm.KeptNew, keptIdleDays, ""))
	b1.mustListen()
	if m.record(t).Scrubbed {
		t.Fatal("a new keep is marked scrubbed before any scrub")
	}
	b1.end()
	// The runner's stop with a scrub that ran.
	unlock, err := macvm.LockKept(m.dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := m.record(t)
	macvm.KeptStopUpdate(&rec, b1.runID, time.Now().UTC(), keptIdleDays)
	rec.Scrubbed = true
	err = macvm.WriteKept(m.dir, rec)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	b2 := bootKept(t, m.run(t, macvm.KeptResume, keptIdleDays, ""))
	b2.mustListen()
	if m.record(t).Scrubbed {
		t.Fatal("a resume left the machine marked scrubbed while its run writes what a scrub removes")
	}
	untested(t, "the guest scrub (CA trust, routes, MCP tables, managed files) needs the runner and the guest agent")
}

// TestKeptLive_Reset: a fresh boot replaces the disk of the same machine and clears its last stop; a base
// that changed since the start was checked refuses it before the disk is touched.
func TestKeptLive_Reset(t *testing.T) {
	liveKept(t)
	m := newKeptMachine(t)
	b1 := bootKept(t, m.run(t, macvm.KeptNew, keptIdleDays, ""))
	b1.mustListen()
	b1.end()
	if err := stopLikeRunner(m.dir, m.id, b1.runID, time.Now().UTC(), keptIdleDays); err != nil {
		t.Fatal(err)
	}
	ino, _ := m.disk(t)
	before := m.record(t)
	if before.LastStop.IsZero() || before.StopKind == "" {
		t.Fatalf("setup: no stop recorded: %+v", before)
	}

	t.Run("base_changed_refused_before_the_disk", func(t *testing.T) {
		bootKept(t, m.run(t, macvm.KeptFresh, keptIdleDays, "not-the-base")).mustRefuse(macvm.KeptRefusalBaseChanged)
		if i, _ := m.disk(t); i != ino {
			t.Fatal("a refused reset replaced the kept disk")
		}
	})

	t.Run("fresh_replaces_the_disk", func(t *testing.T) {
		b := bootKept(t, m.run(t, macvm.KeptFresh, keptIdleDays, ""))
		b.mustListen()
		got := m.record(t)
		if got.ID != m.id || got.ActiveRun != b.runID || !got.LastStop.IsZero() || got.StopKind != "" || got.Scrubbed {
			t.Fatalf("reset: kept.json %+v", got)
		}
		if i, _ := m.disk(t); i == ino {
			t.Fatal("reset kept the old disk")
		}
	})
}

// TestKeptLive_ResumeAfterBaseChange: a resume boots the machine's own disk, so a base that changed (or
// a digest that no longer matches) does not refuse it.
func TestKeptLive_ResumeAfterBaseChange(t *testing.T) {
	liveKept(t)
	m := newKeptMachine(t)
	b1 := bootKept(t, m.run(t, macvm.KeptNew, keptIdleDays, ""))
	b1.mustListen()
	b1.end()
	if err := stopLikeRunner(m.dir, m.id, b1.runID, time.Now().UTC(), keptIdleDays); err != nil {
		t.Fatal(err)
	}
	ino, mid := m.disk(t)
	b2 := bootKept(t, m.run(t, macvm.KeptResume, keptIdleDays, "a-newer-base"))
	b2.mustListen()
	if got := m.record(t); got.ActiveRun != b2.runID {
		t.Fatalf("resume after a base change: kept.json %+v", got)
	}
	if i, id2 := m.disk(t); i != ino || !bytes.Equal(id2, mid) {
		t.Fatal("resume after a base change changed the kept disk")
	}
}

// TestKeptLive_Delete: a delete is refused while a boot holds the machine, then removes the folder and its
// lock file, and a second delete finds nothing.
func TestKeptLive_Delete(t *testing.T) {
	liveKept(t)
	m := newKeptMachine(t)
	b := bootKept(t, m.run(t, macvm.KeptNew, keptIdleDays, ""))
	b.mustListen()
	if _, err := macvm.RemoveKept(m.dir); !errors.Is(err, macvm.ErrKeptLocked) {
		t.Fatalf("delete of a running machine: %v, want ErrKeptLocked", err)
	}
	if _, err := os.Stat(filepath.Join(m.dir, macvm.KeptFile)); err != nil {
		t.Fatalf("a refused delete touched the folder: %v", err)
	}
	b.end()
	if gone, err := macvm.RemoveKept(m.dir); err != nil || !gone {
		t.Fatalf("delete: gone=%v err=%v", gone, err)
	}
	for _, p := range []string{m.dir, m.dir + ".lock"} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived the delete: %v", p, err)
		}
	}
	if gone, err := macvm.RemoveKept(m.dir); err != nil || gone {
		t.Fatalf("second delete: gone=%v err=%v, want false and nil", gone, err)
	}
}

// ---- self-tests: the suite's own kept-machine logic, run everywhere with no Mac ----

func TestKeptSelf_StopLikeRunnerRefreshesTheExpiry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "kept", "m")
	id := uuid.NewString()
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	if err := macvm.WriteKept(dir, macvm.KeptRecord{ID: id, ActiveRun: "run-a", ExpiresAt: t0.AddDate(0, 0, 3), IdleDays: 3}); err != nil {
		t.Fatal(err)
	}
	if err := stopLikeRunner(dir, id, "run-b", t0, 3); err == nil {
		t.Fatal("a stop by a run that does not hold the machine was applied")
	}
	t1 := t0.Add(48 * time.Hour)
	if err := stopLikeRunner(dir, id, "run-a", t1, 10); err != nil {
		t.Fatal(err)
	}
	rec, err := macvm.ReadKept(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ActiveRun != "" || rec.StopKind != macvm.KeptStopHard || !rec.LastStop.Equal(t1) || !rec.ExpiresAt.Equal(t1.AddDate(0, 0, 10)) {
		t.Fatalf("stop update: %+v", rec)
	}
}

func TestKeptSelf_DeleteWaitsForTheBootLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "kept", "m")
	id := uuid.NewString()
	if err := macvm.WriteKept(dir, macvm.KeptRecord{ID: id}); err != nil {
		t.Fatal(err)
	}
	unlock, err := macvm.LockKept(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := macvm.RemoveKept(dir); !errors.Is(err, macvm.ErrKeptLocked) {
		t.Fatalf("delete under a boot's lock: %v", err)
	}
	if _, err := macvm.ReadKept(dir, id); err != nil {
		t.Fatalf("a refused delete touched the folder: %v", err)
	}
	unlock()
	if gone, err := macvm.RemoveKept(dir); err != nil || !gone {
		t.Fatalf("delete: gone=%v err=%v", gone, err)
	}
	if _, err := os.Lstat(dir + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock file survived: %v", err)
	}
	if gone, err := macvm.RemoveKept(dir); err != nil || gone {
		t.Fatalf("second delete: gone=%v err=%v", gone, err)
	}
}

func TestKeptSelf_RefusalCodesRoundTrip(t *testing.T) {
	for want, err := range map[string]error{
		macvm.KeptRefusalLocked:      macvm.ErrKeptLocked,
		macvm.KeptRefusalOwner:       macvm.ErrKeptOwner,
		macvm.KeptRefusalExpired:     macvm.ErrKeptExpired,
		macvm.KeptRefusalBaseChanged: macvm.ErrKeptBaseChanged,
	} {
		if got := macvm.KeptRefusalCode(err); got != want {
			t.Errorf("KeptRefusalCode(%v) = %q, want %q", err, got, want)
		}
	}
	if got, err := macvm.ReadKeptRefusal(t.TempDir()); got != "" || err != nil {
		t.Errorf("no refusal file: %q, %v", got, err)
	}
	if got := macvm.KeptRefusalCode(errors.New("macvm: kept machine x already exists")); got != "" {
		t.Errorf("an uncoded refusal reads as %q", got)
	}
}

func TestKeptSelf_Near(t *testing.T) {
	now := time.Now()
	if !near(now.Add(time.Minute), now, time.Hour) || near(now.Add(2*time.Hour), now, time.Hour) || near(now.Add(-2*time.Hour), now, time.Hour) {
		t.Fatal("near's tolerance is wrong")
	}
}
