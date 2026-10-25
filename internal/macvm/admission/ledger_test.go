// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const gib = 1 << 30

// fakeHost is a Mac with settable numbers and a settable process table.
type fakeHost struct {
	mu    sync.Mutex
	boot  int64
	mem   uint64
	avail uint64
	free  int64
	procs []Proc
}

func (h *fakeHost) BootTime() (int64, error) { h.mu.Lock(); defer h.mu.Unlock(); return h.boot, nil }
func (h *fakeHost) MemoryBytes() (uint64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.mem, nil
}
func (h *fakeHost) AvailableMemoryBytes() (uint64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.avail, nil
}
func (h *fakeHost) FreeDiskBytes(string) (int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.free, nil
}
func (h *fakeHost) Processes() ([]Proc, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Proc(nil), h.procs...), nil
}
func (h *fakeHost) setProcs(ps ...Proc) { h.mu.Lock(); h.procs = ps; h.mu.Unlock() }

// bigMac: 64 GiB, plenty free, boot 1.
func bigMac() *fakeHost { return &fakeHost{boot: 1, mem: 64 * gib, avail: 48 * gib, free: 500 * gib} }

// rig is a ledger dir shared by several simulated logins. Tests run as one
// OS user, so file owners are simulated: a file belongs to the login whose
// ledger created it, and any other file (a forgery) to uid 666.
type rig struct {
	dir    string // the entries dir, <tmp>/Wardyn/macos-vm-admission
	h      *fakeHost
	mu     sync.Mutex
	owners map[string]int
	fs     fakeFS
}

// fakeFS stands in for root ownership: fstat reports every path as owned by
// root, except those in uid, and with the permission bits in perm replaced.
type fakeFS struct {
	mu   sync.Mutex
	uid  map[string]uint32
	perm map[string]uint32
}

func (f *fakeFS) fstat(fd int, name string) (unix.Stat_t, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return st, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	st.Uid = f.uid[name]
	st.Gid = st.Uid // root:wheel, or the login's own group
	if p, ok := f.perm[name]; ok {
		setPerm(&st.Mode, p)
	}
	return st, nil
}

func (f *fakeFS) set(name string, uid uint32, perm ...uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.uid == nil {
		f.uid, f.perm = map[string]uint32{}, map[string]uint32{}
	}
	f.uid[name] = uid
	if len(perm) > 0 {
		f.perm[name] = perm[0]
	}
}

// makeLayout builds the installed layout under a temp dir and returns the
// entries dir.
func makeLayout(t *testing.T) string {
	t.Helper()
	top := filepath.Join(t.TempDir(), "Wardyn")
	dir := filepath.Join(top, "macos-vm-admission")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(top, 0o755)
	if err := os.Chmod(dir, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(top, lockFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(filepath.Join(top, lockFile), 0o644)
	return dir
}

func newRig(t *testing.T, h *fakeHost) *rig {
	t.Helper()
	return &rig{dir: makeLayout(t), h: h, owners: map[string]int{}}
}

func (r *rig) lock() string { return filepath.Join(filepath.Dir(r.dir), lockFile) }

func (r *rig) open(t *testing.T, uid int, runner string, mod ...func(*Config)) *Ledger {
	t.Helper()
	c := Config{Dir: r.dir, UID: uid, RunnerID: runner, DockerMemoryBytes: 8 * gib, Volume: r.dir, Host: r.h, Fstat: r.fs.fstat,
		FileOwner: func(fi os.FileInfo) int {
			r.mu.Lock()
			defer r.mu.Unlock()
			if u, ok := r.owners[fi.Name()]; ok {
				return u
			}
			return 666
		}}
	c.created = func(name string) { r.mu.Lock(); r.owners[name] = uid; r.mu.Unlock() }
	for _, m := range mod {
		m(&c)
	}
	l, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func runReq(mem uint64) Request {
	return Request{Kind: KindRun, GuestCPUs: 4, GuestMemory: mem, DiskReserve: RunDiskReservation(50 * gib), OwnerPID: 10, OwnerStart: 1}
}

func offer(t *testing.T, l *Ledger, mem uint64) Offer {
	t.Helper()
	o, err := l.Offer(mem, RunDiskReservation(50*gib))
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// Host capacity is counted once over every login; a runner's max is
// its own runs plus the headroom. The design's worked case: login A has one
// run, A offers 1+1 and B offers 0+1; B's run is admitted at a host count of
// 2; then both offer their own count and a third VM is refused.
func TestTwoLoginsOneVMEachBothAdmit(t *testing.T) {
	r := newRig(t, bigMac())
	a := r.open(t, 501, "runner-a")
	b := r.open(t, 502, "runner-b")
	if _, err := a.Admit(runReq(8 * gib)); err != nil {
		t.Fatalf("first login: %v", err)
	}
	if o := offer(t, a, 8*gib); o.Max != 2 {
		t.Fatalf("A offers %+v, want 1 own + 1 headroom", o)
	}
	if o := offer(t, b, 8*gib); o.Max != 1 {
		t.Fatalf("B offers %+v, want 0 own + 1 headroom", o)
	}
	if _, err := b.Admit(runReq(8 * gib)); err != nil {
		t.Fatalf("second login, one VM each: %v", err)
	}
	for _, l := range []*Ledger{a, b} {
		if o := offer(t, l, 8*gib); o.Max != 1 || o.Headroom != 0 {
			t.Fatalf("offer with one own run and one other: %+v, want max 1", o)
		}
		if _, err := l.Admit(runReq(8 * gib)); !IsRefusal(err) {
			t.Fatalf("third VM on the Mac: got %v, want a refusal", err)
		}
	}
}

// Prepare plus one run on one runner both admit; a third is refused.
func TestPrepareAndRunOnOneRunnerBothAdmit(t *testing.T) {
	r := newRig(t, bigMac())
	a := r.open(t, 501, "runner-a")
	prep := runReq(8 * gib)
	prep.Kind, prep.DiskReserve = KindPrepare, PrepareDiskReservation(50*gib)
	e, err := a.Admit(prep)
	if err != nil || e.State != StatePreparing {
		t.Fatalf("prepare: %+v %v", e, err)
	}
	if o := offer(t, a, 8*gib); o.Max != 1 {
		t.Fatalf("offer while preparing: %+v, want 0 own + 1", o)
	}
	if _, err := a.Admit(runReq(8 * gib)); err != nil {
		t.Fatalf("run beside prepare: %v", err)
	}
	if o := offer(t, a, 8*gib); o.Max != 1 || o.Headroom != 0 {
		t.Fatalf("offer: %+v, want 1 own + 0", o)
	}
	if _, err := a.Admit(runReq(8 * gib)); !IsRefusal(err) {
		t.Fatalf("third: got %v, want a refusal", err)
	}
	tu := runReq(8 * gib)
	tu.Kind = KindToolsUpdate
	if _, err := a.Admit(tu); !IsRefusal(err) {
		t.Fatalf("tools update beside two: got %v, want a refusal", err)
	}
}

func TestHostCapOne(t *testing.T) {
	r := newRig(t, bigMac())
	a := r.open(t, 501, "runner-a", func(c *Config) { c.HostCap = 1 })
	if o := offer(t, a, 8*gib); o.Max != 1 {
		t.Fatalf("offer at host cap 1: %+v", o)
	}
	if _, err := a.Admit(runReq(8 * gib)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Admit(runReq(8 * gib)); !IsRefusal(err) {
		t.Fatalf("second VM at host cap 1: got %v", err)
	}
}

// Docker's memory decides max. The budget is host memory minus
// Docker's minus 4 GiB, holding (guest + 1 GiB overhead) per VM, capped at 2.
func TestOfferArithmetic(t *testing.T) {
	for _, tc := range []struct {
		name         string
		host, docker uint64
		guest        uint64
		want         int
	}{
		{"rental: 32 GiB, D=12, 6 GiB guests: 16 GiB holds 2 x 7", 32 * gib, 12 * gib, 6 * gib, 2},
		{"16 GiB Mac, D=8: a 4 GiB guest does not fit 4 GiB", 16 * gib, 8 * gib, 4 * gib, 0},
		{"16 GiB Mac, D=8: one 3 GiB guest fits", 16 * gib, 8 * gib, 3 * gib, 1},
		{"16 GiB Mac, D=4, 4 GiB guests: 8 GiB holds one of 5", 16 * gib, 4 * gib, 4 * gib, 1},
		{"32 GiB, D=24: 4 GiB left holds none", 32 * gib, 24 * gib, 6 * gib, 0},
		{"128 GiB caps at the licence's 2", 128 * gib, 8 * gib, 8 * gib, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, &fakeHost{boot: 1, mem: tc.host, avail: tc.host, free: 500 * gib})
			l := r.open(t, 501, "r", func(c *Config) { c.DockerMemoryBytes = tc.docker })
			o := offer(t, l, tc.guest)
			if o.Max != tc.want || (tc.want == 0) != (o.Reason != "") {
				t.Fatalf("offer = %+v, want max %d", o, tc.want)
			}
			if tc.want == 0 && !strings.Contains(o.Reason, "memory") {
				t.Fatalf("reason %q does not name memory", o.Reason)
			}
			got := 0
			for i := 0; i < 3; i++ {
				if _, err := l.Admit(runReq(tc.guest)); err == nil {
					got++
				}
			}
			if got != tc.want {
				t.Fatalf("admitted %d, offer said %d", got, tc.want)
			}
		})
	}
}

// Two logins may each run a Docker engine: the budget subtracts each login's
// Docker memory once.
func TestDockerMemoryPerLogin(t *testing.T) {
	r := newRig(t, &fakeHost{boot: 1, mem: 32 * gib, avail: 32 * gib, free: 500 * gib})
	a := r.open(t, 501, "a")
	b := r.open(t, 502, "b")
	if _, err := a.Admit(runReq(6 * gib)); err != nil { // budget 32-8-4 = 20, uses 7
		t.Fatal(err)
	}
	// B's view: 32 - 8 (A's Docker) - 8 (B's) - 4 = 12; 7 used, 7 more does not fit.
	if _, err := b.Admit(runReq(6 * gib)); !IsRefusal(err) {
		t.Fatalf("second login's Docker memory not subtracted: %v", err)
	}
	if o := offer(t, b, 6*gib); o.Max != 0 {
		t.Fatalf("B offers %+v, want 0", o)
	}
	if _, err := a.Admit(runReq(6 * gib)); err != nil {
		t.Fatalf("same login again (its Docker counted once): %v", err)
	}
}

// Two runs need about 107 GiB free when the second starts.
func TestDiskReservation(t *testing.T) {
	h := &fakeHost{boot: 1, mem: 64 * gib, avail: 48 * gib, free: 106 * gib}
	l := newRig(t, h).open(t, 501, "r")
	if _, err := l.Admit(runReq(8 * gib)); err != nil {
		t.Fatal(err)
	}
	_, err := l.Admit(runReq(8 * gib))
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("second run with 106 GiB free: %v, want a refusal", err)
	}
	// The figures the run page states: the run's reservation plus the floor, against what the first
	// run's reservation leaves of the 106 GiB.
	if need, left := RunDiskReservation(50*gib)+DiskFloorBytes, 106*gib-RunDiskReservation(50*gib); r.DiskNeeded != need || r.DiskFreeLeft != left {
		t.Fatalf("disk refusal figures %d needed, %d free; want %d, %d", r.DiskNeeded, r.DiskFreeLeft, need, left)
	}
	h.free = 107 * gib
	if _, err := l.Admit(runReq(8 * gib)); err != nil {
		t.Fatalf("second run with 107 GiB free: %v", err)
	}
	if got := PrepareDiskReservation(50*gib) + DiskFloorBytes; got != 81*gib {
		t.Fatalf("prepare needs %d GiB, want 81", got/gib)
	}
}

func TestLiveMemoryGuard(t *testing.T) {
	l := newRig(t, &fakeHost{boot: 1, mem: 64 * gib, avail: 9 * gib, free: 500 * gib}).open(t, 501, "r")
	if _, err := l.Admit(runReq(8 * gib)); !IsRefusal(err) {
		t.Fatalf("available 9 GiB < 8 + 2: %v, want a refusal", err)
	}
}

func TestLeakedLowersMaxNeverTombstonedClearsOnNewBoot(t *testing.T) {
	h := bigMac()
	r := newRig(t, h)
	l := r.open(t, 501, "r")
	other := r.open(t, 502, "o")
	if _, err := l.AddLeaked(10, 1); err != nil {
		t.Fatal(err)
	}
	if o := offer(t, l, 8*gib); o.Max != 1 {
		t.Fatalf("offer with a leaked slot: %+v, want 1", o)
	}
	if n, err := other.Sweep(); n != 0 || err != nil { // owner pid 10 is gone, no VZ process: still never
		t.Fatalf("leaked entry tombstoned: %d %v", n, err)
	}
	if err := func() error { es, _ := l.Counted(); return l.ReclaimOwn(es[0].ID, "") }(); err == nil {
		t.Fatal("reclaimed a leaked slot before a restart")
	}
	h.boot = 2
	if o := offer(t, l, 8*gib); o.Max != 2 {
		t.Fatalf("offer after a restart: %+v, want 2", o)
	}
	if _, err := l.Sweep(); err != nil {
		t.Fatal(err)
	}
	if left := entryFiles(t, r.dir); len(left) != 0 {
		t.Fatalf("own entries from an earlier boot left: %v", left)
	}
}

func TestReleaseNeedsVerifiedDestruction(t *testing.T) {
	h := bigMac()
	r := newRig(t, h)
	l := r.open(t, 501, "r")
	e, err := l.Admit(runReq(8 * gib))
	if err != nil {
		t.Fatal(err)
	}
	h.setProcs(Proc{PID: 77, UID: 501, Start: 5, Name: "wardyn-macvm"})
	if _, err := l.Update(e.ID, func(x *Entry) { x.HelperPID, x.HelperStart, x.State = 77, 5, StateRunning }); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(e.ID, Destroyed{Stopped: false}); err == nil {
		t.Fatal("released without a stop report")
	}
	if err := l.Release(e.ID, Destroyed{Stopped: true, RunDir: runDir}); err == nil {
		t.Fatal("released while the helper runs")
	}
	h.setProcs()
	if err := l.Release(e.ID, Destroyed{Stopped: true, RunDir: runDir}); err == nil {
		t.Fatal("released while the run dir exists")
	}
	if err := os.Remove(runDir); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(e.ID, Destroyed{Stopped: true, RunDir: runDir}); err != nil {
		t.Fatalf("release after verified destruction: %v", err)
	}
	if left := entryFiles(t, r.dir); len(left) != 0 {
		t.Fatalf("entry left: %v", left)
	}
	e2, _ := l.Admit(runReq(8 * gib))
	if err := r.open(t, 502, "other").Release(e2.ID, Destroyed{Stopped: true}); err == nil {
		t.Fatal("another login released my entry")
	}
}

func TestReclaimOwnAfterHelperKilled(t *testing.T) {
	h := bigMac()
	l := newRig(t, h).open(t, 501, "r")
	e, _ := l.Admit(runReq(8 * gib))
	h.setProcs(Proc{PID: 77, UID: 501, Start: 5, Name: "wardyn-macvm"}, Proc{PID: 78, UID: 501, Start: 6, Name: "com.apple.Virtua"})
	_, _ = l.Update(e.ID, func(x *Entry) { x.HelperPID, x.HelperStart = 77, 5 })
	if err := l.ReclaimOwn(e.ID, ""); err == nil {
		t.Fatal("reclaimed while the helper runs")
	}
	h.setProcs(Proc{PID: 78, UID: 501, Start: 6, Name: "com.apple.Virtua"})
	if err := l.ReclaimOwn(e.ID, ""); err == nil {
		t.Fatal("reclaimed while a Virtualization process of the uid remains")
	}
	h.setProcs()
	if err := l.ReclaimOwn(e.ID, ""); err != nil {
		t.Fatalf("reclaim once the VM is gone: %v", err)
	}
}

// Any runner may tombstone another uid's entry once its helper is
// gone and no Virtualization process of that uid remains. The tombstone is a
// 0644 file beside the entry; the entry file is never deleted by anyone but
// its owner, who clears it at its next transaction; the writer then clears
// the tombstone.
func TestCrossUIDTombstone(t *testing.T) {
	h := bigMac()
	r := newRig(t, h)
	a := r.open(t, 501, "runner-a")
	b := r.open(t, 502, "runner-b")
	eb, err := b.Admit(runReq(8 * gib))
	if err != nil {
		t.Fatal(err)
	}
	h.setProcs(Proc{PID: 90, UID: 502, Start: 9, Name: "wardyn-macvm"})
	_, _ = b.Update(eb.ID, func(x *Entry) { x.HelperPID, x.HelperStart, x.State = 90, 9, StateRunning })
	if _, err := a.Admit(runReq(8 * gib)); err != nil {
		t.Fatal(err)
	}
	if n, _ := a.Sweep(); n != 0 {
		t.Fatal("tombstoned a live helper's entry")
	}
	// Helper gone, but a Virtualization process of uid 502 remains.
	h.setProcs(Proc{PID: 91, UID: 502, Start: 9, Name: "com.apple.Virtua"})
	if n, _ := a.Sweep(); n != 0 {
		t.Fatal("tombstoned while a Virtualization process of the uid remains")
	}
	if _, err := a.Admit(runReq(8 * gib)); !IsRefusal(err) {
		t.Fatalf("admitted a third VM while the other login's VM may live: %v", err)
	}
	// Same pid, another start time: pid reuse, so the helper is gone.
	h.setProcs(Proc{PID: 90, UID: 502, Start: 99, Name: "bash"})
	if n, err := a.Sweep(); n != 1 || err != nil {
		t.Fatalf("sweep = %d, %v; want 1 tombstone", n, err)
	}
	entry, tombName := "e-"+eb.ID+".json", "e-"+eb.ID+".json.tombstone"
	for _, f := range []string{entry, tombName} {
		if _, err := os.Stat(filepath.Join(r.dir, f)); err != nil {
			t.Fatalf("want the entry kept and a tombstone beside it: %v", err)
		}
	}
	if fi, err := os.Stat(filepath.Join(r.dir, tombName)); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("tombstone mode: %v %v", fi, err)
	}
	if n, _ := a.Sweep(); n != 0 {
		t.Fatal("tombstoned twice")
	}
	if _, err := a.Admit(runReq(8 * gib)); err != nil {
		t.Fatalf("slot of the tombstoned entry not freed: %v", err)
	}
	// The owner reconciles its tombstoned entry when it comes back, then the
	// writer clears its tombstone.
	if _, err := b.Sweep(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.dir, entry)); err != nil {
		t.Fatal("a tombstoned entry was deleted before its owner reconciled it")
	}
	if err := b.ReclaimOwn(eb.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.dir, entry)); !os.IsNotExist(err) {
		t.Fatalf("owner did not clear its tombstoned entry: %v", err)
	}
	if _, err := a.Sweep(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.dir, tombName)); !os.IsNotExist(err) {
		t.Fatalf("writer did not clear its tombstone: %v", err)
	}
}

// Every reader re-checks a tombstone: one for a live VM, forged or early,
// changes nothing, and comes back into force only when the condition holds.
func TestTombstoneRechecked(t *testing.T) {
	h := bigMac()
	r := newRig(t, h)
	a := r.open(t, 501, "a")
	b := r.open(t, 502, "b")
	ea, _ := a.Admit(runReq(8 * gib))
	eb, _ := b.Admit(runReq(8 * gib))
	h.setProcs(Proc{PID: 10, UID: 501, Start: 1}, Proc{PID: 20, UID: 502, Start: 2})
	_, _ = a.Update(ea.ID, func(x *Entry) { x.HelperPID, x.HelperStart = 10, 1 })
	_, _ = b.Update(eb.ID, func(x *Entry) { x.HelperPID, x.HelperStart = 20, 2 })
	for _, id := range []string{ea.ID, eb.ID} {
		if err := os.WriteFile(filepath.Join(r.dir, "e-"+id+".json.tombstone"), []byte(id), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if es, _ := a.Counted(); len(es) != 2 {
		t.Fatalf("forged tombstones hid live entries: %d counted", len(es))
	}
	c := r.open(t, 503, "c")
	if _, err := c.Admit(runReq(8 * gib)); !IsRefusal(err) {
		t.Fatalf("a forged tombstone granted a slot: %v", err)
	}
	// B's helper dies: the (early) tombstone now holds.
	h.setProcs(Proc{PID: 10, UID: 501, Start: 1})
	if _, err := c.Admit(runReq(8 * gib)); err != nil {
		t.Fatalf("re-checked tombstone not honoured: %v", err)
	}
}

// A forged entry (any local user can write one) only refuses.
func TestForgedEntryOnlyRefuses(t *testing.T) {
	r := newRig(t, bigMac())
	a := r.open(t, 501, "a")
	forged := `{"id":"forged","kind":"run","state":"running","runner_id":"a","boot_time":1,"owner_pid":1,"owner_start":1,"guest_memory_bytes":1}`
	if err := os.WriteFile(filepath.Join(r.dir, "e-forged.json"), []byte(forged), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "e-garbage.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Admit(runReq(8 * gib)); !IsRefusal(err) {
		t.Fatalf("two forged entries should fill the cap: %v", err)
	}
	// It names my runner, but its file is not mine: I cannot release it.
	if err := a.Release("forged", Destroyed{Stopped: true}); err == nil {
		t.Fatal("released a forged entry as my own")
	}
	if o := offer(t, a, 8*gib); o.Max != 0 {
		t.Fatalf("forged entries must not raise the offer: %+v", o)
	}
}

// Entries are 0644, created exclusively, and carry no uid: the uid is the
// file's owner.
func TestEntryFileModeAndOwner(t *testing.T) {
	r := newRig(t, bigMac())
	a := r.open(t, 501, "a")
	e, err := a.Admit(runReq(8 * gib))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(r.dir, "e-"+e.ID+".json")
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("entry mode %v, %v; want 0644", fi.Mode(), err)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), `"uid"`) {
		t.Fatalf("entry carries a uid field: %s", b)
	}
	es, _ := a.Counted()
	if len(es) != 1 || es[0].UID != 501 {
		t.Fatalf("uid not read from the file owner: %+v", es)
	}
	// The real owner lookup reads the stat.
	if got := statOwner(fi); got != os.Getuid() {
		t.Fatalf("statOwner = %d, want %d", got, os.Getuid())
	}
}

func TestLockWaitIsBounded(t *testing.T) {
	r := newRig(t, bigMac())
	a := r.open(t, 501, "a", func(c *Config) { c.LockWait = 100 * time.Millisecond })
	f, err := os.Open(r.lock())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := a.Admit(runReq(8 * gib)); !errors.Is(err, ErrBusy) {
		t.Fatalf("held lock: got %v, want ErrBusy", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("waited %v", d)
	}
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	if _, err := a.Admit(runReq(8 * gib)); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRefusesUnstickyDir(t *testing.T) {
	r := newRig(t, bigMac())
	r.fs.set(r.dir, 0, 0o777)
	if _, err := Open(Config{Dir: r.dir, UID: 1, RunnerID: "r", Host: bigMac(), Fstat: r.fs.fstat}); err == nil || !strings.Contains(err.Error(), "reinstall the Wardyn runner package") {
		t.Fatalf("opened a ledger dir without the sticky bit: %v", err)
	}
}

// openErr opens r's ledger as login 501 and returns the error.
func (r *rig) openErr() error {
	_, err := Open(Config{Dir: r.dir, UID: 501, RunnerID: "a", Host: r.h, Fstat: r.fs.fstat})
	return err
}

func wantNotInstalled(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "reinstall the Wardyn runner package") {
		t.Fatalf("%s: got %v, want a refusal naming the fix", what, err)
	}
}

// A lock some login made (or one others can write) is refused.
func TestPlantedLockRefused(t *testing.T) {
	r := newRig(t, bigMac())
	r.fs.set(r.lock(), 501)
	wantNotInstalled(t, r.openErr(), "user-owned lock")
	r.fs.set(r.lock(), 0, 0o664)
	wantNotInstalled(t, r.openErr(), "group-writable lock")
	r.fs.set(r.lock(), 0, 0o444)
	wantNotInstalled(t, r.openErr(), "read-only 0444 lock (not the installer's exact 0644)")
	r.fs.set(r.lock(), 0, 0o644)
	if err := r.openErr(); err != nil {
		t.Fatal(err)
	}
}

// A directory some login made before the install is refused, at the lock's
// own directory, at the entries directory, and at any ancestor.
func TestPlantedDirRefused(t *testing.T) {
	r := newRig(t, bigMac())
	top := filepath.Dir(r.dir)
	r.fs.set(top, 501)
	wantNotInstalled(t, r.openErr(), "user-owned Wardyn dir")
	r.fs.set(top, 0, 0o1777)
	wantNotInstalled(t, r.openErr(), "world-writable sticky lock dir")
	r.fs.set(top, 0, 0o755)
	r.fs.set(r.dir, 501)
	wantNotInstalled(t, r.openErr(), "user-owned entries dir")
	r.fs.set(r.dir, 0)
	anc := filepath.Dir(top)
	r.fs.set(anc, 501)
	wantNotInstalled(t, r.openErr(), "user-owned ancestor")
	r.fs.set(anc, 0, 0o777)
	wantNotInstalled(t, r.openErr(), "writable non-sticky ancestor")
	r.fs.set(anc, 0, 0o1777) // like /Users/Shared: sticky, its child root-owned
	if err := r.openErr(); err != nil {
		t.Fatal(err)
	}
}

// A symlinked lock, or a symlink anywhere on the way, is never followed.
func TestSymlinkedLockRefused(t *testing.T) {
	r := newRig(t, bigMac())
	real := filepath.Join(t.TempDir(), "elsewhere.lock")
	_ = os.WriteFile(real, nil, 0o644)
	_ = os.Remove(r.lock())
	if err := os.Symlink(real, r.lock()); err != nil {
		t.Fatal(err)
	}
	wantNotInstalled(t, r.openErr(), "symlinked lock")

	r2 := newRig(t, bigMac())
	top := filepath.Dir(r2.dir)
	moved := top + ".real"
	_ = os.Rename(top, moved)
	if err := os.Symlink(moved, top); err != nil {
		t.Fatal(err)
	}
	wantNotInstalled(t, r2.openErr(), "symlinked Wardyn dir")
}

// A lock renamed away between a runner's open and its flock: the path no
// longer names the file it locked, so it refuses rather than admit while
// another runner may hold the new file's lock.
func TestLockRenamedUnderHeldFlock(t *testing.T) {
	r := newRig(t, bigMac())
	a := r.open(t, 501, "a")
	swapped := false
	a.c.afterFlock = func() {
		if !swapped {
			swapped = true
			_ = os.Rename(r.lock(), r.lock()+".old")
			_ = os.WriteFile(r.lock(), nil, 0o644)
		}
	}
	if _, err := a.Admit(runReq(8 * gib)); err == nil || !strings.Contains(err.Error(), "changed while this runner locked it") {
		t.Fatalf("admitted on a lock that was renamed away: %v", err)
	}
	if n := len(entryFiles(t, r.dir)); n != 0 {
		t.Fatalf("%d entries written", n)
	}
}

func TestRacingAdmissionsGoroutines(t *testing.T) {
	r := newRig(t, bigMac())
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 12; i++ {
		l := r.open(t, 500+i, fmt.Sprintf("r%d", i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.Admit(runReq(8 * gib)); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			} else if !IsRefusal(err) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if ok != LicenceCap {
		t.Fatalf("%d admitted, want %d", ok, LicenceCap)
	}
}

// TestRacingAdmissionsProcesses races separate processes (the test binary
// re-run as children) on one ledger.
func TestRacingAdmissionsProcesses(t *testing.T) {
	if os.Getenv("ADMISSION_CHILD_DIR") != "" {
		t.Skip("child")
	}
	r := newRig(t, bigMac())
	var wg sync.WaitGroup
	results := make([]string, 6)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestAdmissionChild$")
			cmd.Env = append(os.Environ(), "ADMISSION_CHILD_DIR="+r.dir, "ADMISSION_CHILD_UID="+strconv.Itoa(600+i))
			out, _ := cmd.CombinedOutput()
			results[i] = string(out)
		}(i)
	}
	wg.Wait()
	admitted := 0
	for _, res := range results {
		if strings.Contains(res, "CHILD-ADMITTED") {
			admitted++
		} else if !strings.Contains(res, "CHILD-REFUSED") {
			t.Fatalf("child output: %s", res)
		}
	}
	if admitted != LicenceCap {
		t.Fatalf("%d processes admitted, want %d", admitted, LicenceCap)
	}
}

func TestAdmissionChild(t *testing.T) {
	dir := os.Getenv("ADMISSION_CHILD_DIR")
	if dir == "" {
		t.Skip("run by TestRacingAdmissionsProcesses")
	}
	uid, _ := strconv.Atoi(os.Getenv("ADMISSION_CHILD_UID"))
	r := &rig{dir: dir, h: bigMac(), owners: map[string]int{}}
	_, err := r.open(t, uid, "child").Admit(runReq(8 * gib))
	switch {
	case err == nil:
		fmt.Println("CHILD-ADMITTED")
	case IsRefusal(err):
		fmt.Println("CHILD-REFUSED")
	default:
		t.Fatal(err)
	}
}

func TestParseVMStat(t *testing.T) {
	out := "Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages free:                               10.\nPages active:                             999.\nPages inactive:                            5.\nPages speculative:                         1.\n"
	got, err := parseVMStat(out)
	if err != nil || got != 16*16384 {
		t.Fatalf("parseVMStat = %d, %v", got, err)
	}
	if _, err := parseVMStat("nonsense"); err == nil {
		t.Fatal("parsed nonsense")
	}
}

func entryFiles(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range des {
		if strings.HasPrefix(d.Name(), "e-") {
			out = append(out, d.Name())
		}
	}
	return out
}

// Runnerd dies after spawning the helper and before recording
// its pid. The entry names only the dead owner, and the helper has not yet
// created its VM. Its helper process keeps the entry from being tombstoned,
// and the helper then claims the entry itself before it creates the VM.
func TestOwnerDiesBeforeRecordingHelper(t *testing.T) {
	h := bigMac()
	r := newRig(t, h)
	b := r.open(t, 502, "runner-b")
	a := r.open(t, 501, "runner-a")
	h.setProcs(Proc{PID: 50, UID: 502, Start: 5, Name: "wardyn-runnerd"})
	req := runReq(8 * gib)
	req.OwnerPID, req.OwnerStart = 50, 5
	eb, err := b.Admit(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Admit(runReq(8 * gib)); err != nil {
		t.Fatal(err)
	}
	// runnerd crashed; its orphaned helper runs, with no VM yet.
	h.setProcs(Proc{PID: 60, UID: 502, Start: 6, Name: "wardyn-macvm"})
	if n, _ := a.Sweep(); n != 0 {
		t.Fatal("tombstoned an entry whose helper process still runs")
	}
	if _, err := a.Admit(runReq(8 * gib)); !IsRefusal(err) {
		t.Fatalf("over-admitted while an orphaned helper may create its VM: %v", err)
	}
	// The helper claims its entry under the lock before creating the VM.
	if e, err := b.ClaimHelper(eb.ID, 60, 6); err != nil || e.HelperPID != 60 || e.State != StateStarting {
		t.Fatalf("claim: %+v %v", e, err)
	}
	if n, _ := a.Sweep(); n != 0 {
		t.Fatal("tombstoned a claimed entry with a live helper")
	}
}

// The helper refuses to start when its entry is gone, from an earlier boot,
// tombstoned, or not its own.
func TestClaimHelperRefusals(t *testing.T) {
	h := bigMac()
	r := newRig(t, h)
	b := r.open(t, 502, "runner-b")
	a := r.open(t, 501, "runner-a")
	if _, err := b.ClaimHelper("nope", 1, 1); err == nil {
		t.Fatal("claimed a missing entry")
	}
	eb, _ := b.Admit(runReq(8 * gib)) // owner pid 10 is not in the process table
	if _, err := a.ClaimHelper(eb.ID, 1, 1); err == nil {
		t.Fatal("another login claimed the entry")
	}
	if n, _ := a.Sweep(); n != 1 {
		t.Fatalf("want the dead entry tombstoned, got %d", n)
	}
	if _, err := b.ClaimHelper(eb.ID, 60, 6); err == nil {
		t.Fatal("claimed a tombstoned entry")
	}
	ec, _ := b.Admit(runReq(8 * gib))
	h.boot = 2
	if _, err := b.ClaimHelper(ec.ID, 60, 6); err == nil {
		t.Fatal("claimed an entry from an earlier boot")
	}
}

// An unparsable entry counts as live until the boot changes.
func TestUnparsableEntryCountsUntilBootChanges(t *testing.T) {
	h := &fakeHost{boot: time.Now().Add(-time.Hour).UnixMicro(), mem: 64 * gib, avail: 48 * gib, free: 500 * gib}
	r := newRig(t, h)
	a := r.open(t, 501, "a")
	for _, n := range []string{"e-torn1.json", "e-torn2.json"} {
		if err := os.WriteFile(filepath.Join(r.dir, n), []byte(`{"id":"x","kin`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Admit(runReq(8 * gib)); !IsRefusal(err) {
		t.Fatalf("torn entries not counted: %v", err)
	}
	h.boot = time.Now().Add(time.Second).UnixMicro() // a restart after the files were written
	if _, err := a.Admit(runReq(8 * gib)); err != nil {
		t.Fatalf("torn entries from an earlier boot still counted: %v", err)
	}
	if des, _ := os.ReadDir(r.dir); func() bool {
		for _, d := range des {
			if strings.HasSuffix(d.Name(), ".tmp") {
				return true
			}
		}
		return false
	}() {
		t.Fatal("a temp file was left behind")
	}
}

// A missing lock is refused with a named fix and never created.
func TestMissingLockRefusedNeverCreated(t *testing.T) {
	r := newRig(t, bigMac())
	_ = os.Remove(r.lock())
	wantNotInstalled(t, r.openErr(), "missing lock")
	if _, err := os.Stat(r.lock()); !os.IsNotExist(err) {
		t.Fatal("the lock was created")
	}
}

// Watch recomputes, so another login's entry reaches this
// runner's offer.
func TestWatchSeesOtherLoginsChanges(t *testing.T) {
	r := newRig(t, bigMac())
	a := r.open(t, 501, "a")
	b := r.open(t, 502, "b")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan Offer, 8)
	go a.Watch(ctx, 5*time.Millisecond, 8*gib, RunDiskReservation(50*gib), func(o Offer) { got <- o })
	if o := <-got; o.Max != 2 {
		t.Fatalf("first offer %+v", o)
	}
	if _, err := b.Admit(runReq(8 * gib)); err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-got:
		if o.Max != 1 {
			t.Fatalf("offer after B's entry %+v, want 1", o)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch never saw B's entry")
	}
}

// An honoured tombstone frees the dead entry's slot and memory, not
// its disk, which stays a debt until the owner reconciles; disk admission
// subtracts the debt.
func TestTombstoneDiskDebt(t *testing.T) {
	h := &fakeHost{boot: 1, mem: 64 * gib, avail: 48 * gib, free: 120 * gib}
	r := newRig(t, h)
	a := r.open(t, 501, "a")
	b := r.open(t, 502, "b")
	eb, err := b.Admit(runReq(8 * gib)) // reserves 51 GiB; owner pid 10 is dead
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := a.Sweep(); n != 1 {
		t.Fatal("want B's dead entry tombstoned")
	}
	tb, _ := os.ReadFile(filepath.Join(r.dir, "e-"+eb.ID+".json.tombstone"))
	if !strings.Contains(string(tb), `"capacity":"recovered"`) || !strings.Contains(string(tb), `"owner_cleanup":"pending"`) {
		t.Fatalf("tombstone state %s", tb)
	}
	// (1) Slot and memory are free; the disk is not.
	if es, _ := a.Counted(); len(es) != 0 {
		t.Fatalf("tombstoned entry still holds a slot: %+v", es)
	}
	if ds, _ := a.Debts(); len(ds) != 1 || ds[0].ID != eb.ID {
		t.Fatalf("debts %+v", ds)
	}
	// (3) 120 free - 51 debt = 69: one 51 GiB run + 5 floor fits, a second does not.
	if _, err := a.Admit(runReq(8 * gib)); err != nil {
		t.Fatalf("first run beside the debt: %v", err)
	}
	if _, err := a.Admit(runReq(8 * gib)); !IsRefusal(err) || !strings.Contains(err.Error(), "free") {
		t.Fatalf("second run must be refused for disk (the debt), got %v", err)
	}
	// A tombstoned entry is not reconciled while its run dir remains.
	runDir := filepath.Join(t.TempDir(), "run")
	_ = os.Mkdir(runDir, 0o700)
	if err := b.ReclaimOwn(eb.ID, runDir); err == nil {
		t.Fatal("reconciled while the owner's run files remain")
	}
	// (2) The owner comes back, cleans its run files and reconciles.
	_ = os.Remove(runDir)
	if err := b.ReclaimOwn(eb.ID, runDir); err != nil {
		t.Fatal(err)
	}
	if ds, _ := a.Debts(); len(ds) != 0 {
		t.Fatalf("debt left after reconcile: %+v", ds)
	}
	if _, err := a.Admit(runReq(8 * gib)); err != nil {
		t.Fatalf("disk not freed after reconcile: %v", err)
	}
}

// setPerm replaces the permission bits of a Stat_t mode (uint16 on darwin).
func setPerm[T ~uint16 | ~uint32](m *T, p uint32) { *m = (*m &^ 0o7777) | T(p) }
