// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// setupCfg runs Setup on r's base as a root that cannot chown (tests run as one user).
func (r *rig) setupCfg(mod ...func(*SetupConfig)) SetupConfig {
	c := SetupConfig{Base: filepath.Dir(r.dir), Fstat: r.fs.fstat, Chown: func(*os.File) error { return nil }, LockWait: 5 * time.Second}
	for _, m := range mod {
		m(&c)
	}
	return c
}

func ino(t *testing.T, path string) uint64 {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Ino
}

func perm(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode() & (os.ModePerm | os.ModeSticky)
}

// tree is every path under base with its inode, mode and content.
func tree(t *testing.T, base string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(base, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		b, _ := os.ReadFile(p)
		out[p] = fi.Mode().String() + " " + strconv.FormatUint(ino(t, p), 10) + " " + string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func wantLayout(t *testing.T, r *rig) {
	t.Helper()
	base := filepath.Dir(r.dir)
	if perm(t, base) != 0o755 || perm(t, r.lock()) != 0o644 || perm(t, r.dir) != os.ModeSticky|0o777 {
		t.Fatalf("layout: base %v, lock %v, entries %v", perm(t, base), perm(t, r.lock()), perm(t, r.dir))
	}
}

// From a base the shell just made, Setup makes the lock and the entries directory.
func TestSetupFromBase(t *testing.T) {
	r := newRig(t, bigMac())
	_ = os.Remove(r.lock())
	_ = os.Remove(r.dir)
	if err := Setup(r.setupCfg()); err != nil {
		t.Fatal(err)
	}
	wantLayout(t, r)
	r.open(t, 501, "a") // the helper accepts it
}

// A second run on an exact ledger changes nothing: the lock keeps its inode
// (a live helper may hold it) and every entry stays.
func TestSetupIdempotent(t *testing.T) {
	r := newRig(t, bigMac())
	if _, err := r.open(t, 501, "a").Admit(runReq(8 * gib)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.lock(), []byte("held"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := tree(t, filepath.Dir(r.dir))
	if err := Setup(r.setupCfg()); err != nil {
		t.Fatal(err)
	}
	after := tree(t, filepath.Dir(r.dir))
	if len(before) != len(after) {
		t.Fatalf("Setup changed an exact ledger: %d paths -> %d", len(before), len(after))
	}
	for p, v := range before {
		if after[p] != v {
			t.Fatalf("Setup changed %s on an exact ledger", p)
		}
	}
}

// A lock that is not exactly root:wheel 0644 (here 0444, which the old helper rule
// accepted) is replaced by a new inode; the entries stay where they are.
func TestSetupReplacesLockThatIsNotExact(t *testing.T) {
	r := newRig(t, bigMac())
	if _, err := r.open(t, 501, "a").Admit(runReq(8 * gib)); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(r.lock(), 0o444)
	old, ents := ino(t, r.lock()), entryFiles(t, r.dir)
	if err := Setup(r.setupCfg()); err != nil {
		t.Fatal(err)
	}
	wantLayout(t, r)
	if ino(t, r.lock()) == old {
		t.Fatal("a 0444 lock was kept")
	}
	if got := entryFiles(t, r.dir); len(got) != len(ents) || got[0] != ents[0] {
		t.Fatalf("entries %v -> %v", ents, got)
	}
}

// An entries directory that is not exact is replaced, and every entry moves
// into the new one (same inode, same content): no reservation is lost.
func TestSetupMovesEntries(t *testing.T) {
	r := newRig(t, bigMac())
	if _, err := r.open(t, 501, "a").Admit(runReq(8 * gib)); err != nil {
		t.Fatal(err)
	}
	e := entryFiles(t, r.dir)[0]
	was, body := ino(t, filepath.Join(r.dir, e)), readFile(t, filepath.Join(r.dir, e))
	_ = os.Chmod(r.dir, 0o755)
	if err := Setup(r.setupCfg()); err != nil {
		t.Fatal(err)
	}
	wantLayout(t, r)
	p := filepath.Join(r.dir, e)
	if ino(t, p) != was || readFile(t, p) != body {
		t.Fatal("an entry was not moved into the new entries directory")
	}
	if _, err := r.open(t, 502, "b").Offer(8*gib, RunDiskReservation(50*gib)); err != nil {
		t.Fatal(err)
	}
}

// A root-owned base that is not exact is never chmodded: it moves into a
// quarantine beside it, a new base is made, and its entries move across.
func TestSetupRebuildsBase(t *testing.T) {
	r := newRig(t, bigMac())
	if _, err := r.open(t, 501, "a").Admit(runReq(8 * gib)); err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(r.dir)
	e := entryFiles(t, r.dir)[0]
	was := ino(t, filepath.Join(r.dir, e))
	oldBase := ino(t, base)
	_ = os.Chmod(base, 0o711)
	if err := Setup(r.setupCfg()); err != nil {
		t.Fatal(err)
	}
	wantLayout(t, r)
	if ino(t, base) == oldBase || ino(t, filepath.Join(r.dir, e)) != was {
		t.Fatal("the base was not made again, or its entry did not move")
	}
	q, _ := filepath.Glob(filepath.Join(filepath.Dir(base), ".wardyn-quarantine.*", "Wardyn"))
	if len(q) != 1 || perm(t, q[0]) != 0o711 {
		t.Fatalf("the old base is not in a quarantine with its mode untouched: %v", q)
	}
}

// Children planted or damaged inside a sound base (from an older install): a
// login's lock, a symlinked lock, a lock that is a directory, a symlinked
// entries directory. Each is replaced, and no link is followed.
func TestSetupReplacesPlantedChildren(t *testing.T) {
	r := newRig(t, bigMac())
	old := ino(t, r.lock())
	loginsLock := func(c *SetupConfig) { // the planted inode reads as a login's, whatever its name
		c.Fstat = func(fd int, name string) (unix.Stat_t, error) {
			st, err := r.fs.fstat(fd, name)
			if st.Ino == old {
				st.Uid, st.Gid = 501, 501
			}
			return st, err
		}
	}
	if err := Setup(r.setupCfg(loginsLock)); err != nil {
		t.Fatal(err)
	}
	if ino(t, r.lock()) == old {
		t.Fatal("a login's lock was kept")
	}

	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0o666); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(target, 0o666)
	_ = os.Remove(r.lock())
	if err := os.Symlink(target, r.lock()); err != nil {
		t.Fatal(err)
	}
	if err := Setup(r.setupCfg()); err != nil {
		t.Fatal(err)
	}
	if readFile(t, target) != "keep" || perm(t, target) != 0o666 {
		t.Fatal("the lock symlink's target was changed")
	}

	_ = os.Remove(r.lock())
	if err := os.Mkdir(r.lock(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Setup(r.setupCfg()); err != nil {
		t.Fatal(err)
	}

	elsewhere := t.TempDir()
	was := perm(t, elsewhere)
	_ = os.RemoveAll(r.dir)
	if err := os.Symlink(elsewhere, r.dir); err != nil {
		t.Fatal(err)
	}
	if err := Setup(r.setupCfg()); err != nil {
		t.Fatal(err)
	}
	if des, _ := os.ReadDir(elsewhere); len(des) != 0 || perm(t, elsewhere) != was {
		t.Fatalf("the entries symlink's target was changed: %v %v", des, perm(t, elsewhere))
	}
	wantLayout(t, r)
	r.open(t, 501, "a")
}

// A base others can write is refused, and nothing in it is touched.
func TestSetupRefusesWritableBase(t *testing.T) {
	r := newRig(t, bigMac())
	base := filepath.Dir(r.dir)
	_ = os.Chmod(base, 0o777)
	before := tree(t, base)
	wantNotInstalled(t, Setup(r.setupCfg()), "a 0777 base")
	if len(tree(t, base)) != len(before) {
		t.Fatal("Setup changed a refused base")
	}
}

// A lock held past LockWait: Setup changes nothing and says so.
func TestSetupWaitIsBounded(t *testing.T) {
	r := newRig(t, bigMac())
	_ = os.Chmod(r.lock(), 0o444)
	f, err := os.Open(r.lock())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	before := tree(t, filepath.Dir(r.dir))
	if err := Setup(r.setupCfg(func(c *SetupConfig) { c.LockWait = 100 * time.Millisecond })); !errors.Is(err, ErrBusy) {
		t.Fatalf("held lock: got %v, want ErrBusy", err)
	}
	after := tree(t, filepath.Dir(r.dir))
	for p, v := range before {
		if after[p] != v {
			t.Fatalf("a timed-out Setup changed %s", p)
		}
	}
}

// The witness: helper A, admitted on a 0444 lock under the old rule, is inside
// its admission, and the installer waits for it. While the installer holds the
// lock, new helper B refuses the 0444 lock, and old helper C opens it and waits
// on its flock (an offer, under the same flock); once the installer has
// replaced it, C refuses at its recheck and B admits on the new lock. No
// admission overlaps the installer, and no reservation is lost.
func TestSetupWitness(t *testing.T) {
	r := newRig(t, bigMac())
	_ = os.Chmod(r.lock(), 0o444)
	var installing atomic.Bool
	var overlaps atomic.Int32
	oldRule := func(c *Config) { // an old helper accepted any root-owned lock nobody else could write
		c.Fstat = func(fd int, name string) (unix.Stat_t, error) {
			st, err := r.fs.fstat(fd, name)
			if name == r.lock() && uint32(st.Mode)&0o7777 == 0o444 {
				setPerm(&st.Mode, 0o644)
			}
			return st, err
		}
	}
	watch := func(c *Config) { // an entry is created only inside an admission
		next := c.created
		c.created = func(n string) {
			if installing.Load() {
				overlaps.Add(1)
			}
			next(n)
		}
	}
	inA, releaseA := make(chan struct{}), make(chan struct{})
	a := r.open(t, 501, "a", oldRule, watch, func(c *Config) {
		c.afterFlock = func() { close(inA); <-releaseA }
	})
	cl := r.open(t, 502, "c", oldRule, watch)

	var wg sync.WaitGroup
	var aErr, cErr, setupErr error
	wg.Add(1)
	go func() { defer wg.Done(); _, aErr = a.Admit(runReq(8 * gib)) }()
	<-inA
	inSetup, releaseSetup := make(chan struct{}), make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		setupErr = Setup(r.setupCfg(func(c *SetupConfig) {
			c.afterFlock = func() { installing.Store(true); close(inSetup); <-releaseSetup }
		}))
		installing.Store(false)
	}()
	select {
	case <-inSetup:
		t.Fatal("the installer took the lock while helper A was inside its admission")
	case <-time.After(300 * time.Millisecond):
	}
	close(releaseA)
	select {
	case <-inSetup:
	case <-time.After(5 * time.Second):
		t.Fatal("the installer never got the lock")
	}
	if _, err := Open(Config{Dir: r.dir, UID: 503, RunnerID: "b", Host: r.h, Fstat: r.fs.fstat}); err == nil {
		t.Fatal("new helper B accepted the 0444 lock while the installer held it")
	}
	wg.Add(1)
	go func() { defer wg.Done(); _, cErr = cl.Offer(8*gib, RunDiskReservation(50*gib)) }()
	time.Sleep(200 * time.Millisecond) // C has opened the old lock and waits on its flock
	close(releaseSetup)
	wg.Wait()
	if aErr != nil || setupErr != nil {
		t.Fatalf("A: %v, installer: %v", aErr, setupErr)
	}
	if cErr == nil || !strings.Contains(cErr.Error(), "changed while this runner locked it") {
		t.Fatalf("C on the old inode: got %v, want its recheck to refuse", cErr)
	}
	if _, err := r.open(t, 503, "b", watch).Admit(runReq(8 * gib)); err != nil {
		t.Fatalf("B on the new lock: %v", err)
	}
	if n := overlaps.Load(); n != 0 {
		t.Fatalf("%d admissions overlapped the installer", n)
	}
	if n := len(entryFiles(t, r.dir)); n != 2 {
		t.Fatalf("%d entries, want A's and B's: a reservation was lost", n)
	}
	wantLayout(t, r)
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
