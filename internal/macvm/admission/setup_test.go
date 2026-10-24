// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"errors"
	"fmt"
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
	if err := Setup(r.setupCfg()); err != nil { // the rig has no sock/ yet
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
// login's lock, a lock that is a directory, a symlinked entries directory are
// replaced; a symlinked lock is refused (only ENOENT means no lock). No link
// is followed.
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
	wantNotInstalled(t, Setup(r.setupCfg()), "a symlinked lock")
	if readFile(t, target) != "keep" || perm(t, target) != 0o666 {
		t.Fatal("the lock symlink's target was changed")
	}
	if fi, err := os.Lstat(r.lock()); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("a refused Setup replaced the symlinked lock")
	}
	_ = os.Remove(r.lock())

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

// admitN admits n logins (uids 501...) and returns the entry names.
func admitN(t *testing.T, r *rig, n int) []string {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := r.open(t, 501+i, fmt.Sprintf("r%d", i)).Admit(runReq(8 * gib)); err != nil {
			t.Fatal(err)
		}
	}
	return entryFiles(t, r.dir)
}

// wantEntries: dst holds every one of names.
func wantEntries(t *testing.T, r *rig, names []string, what string) {
	t.Helper()
	for _, n := range names {
		if _, err := os.Lstat(filepath.Join(r.dir, n)); err != nil {
			t.Fatalf("%s: entry %s lost (%v)", what, n, err)
		}
	}
}

// everyEntry counts each of names wherever it is under base's parent.
func everyEntry(t *testing.T, r *rig, names []string) int {
	t.Helper()
	found := 0
	_ = filepath.Walk(filepath.Dir(filepath.Dir(r.dir)), func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			for _, n := range names {
				if filepath.Base(p) == n {
					found++
				}
			}
		}
		return nil
	})
	return found
}

// Fable R4 M1: the staging directory stays root-only while entries move into
// it, so a name planted in it as soon as it can be reached (its chown) is too
// late to block a move. Setup, retried once if it fails, ends with every entry.
func TestSetupStagingRootOnlyDuringMove(t *testing.T) {
	r := newRig(t, bigMac())
	names := admitN(t, r, 2)
	_ = os.Chmod(r.dir, 0o755)
	plant := func(c *SetupConfig) {
		c.Chown = func(f *os.File) error {
			if strings.HasPrefix(filepath.Base(f.Name()), "."+entriesName+".") {
				for _, n := range names {
					_ = os.Mkdir(filepath.Join(f.Name(), n), 0o755) // a login planting another's entry name
				}
			}
			return nil
		}
	}
	if err := Setup(r.setupCfg(plant)); err != nil {
		if err := Setup(r.setupCfg()); err != nil {
			t.Fatalf("the retry: %v", err)
		}
	}
	wantEntries(t, r, names, "a name planted in the staging directory")
	wantLayout(t, r)
}

// The witness for a name planted mid-move: the move fails; a retry refuses
// (two things share that name) with every entry still on disk; once root
// clears the planted name, a retry carries every entry forward.
func TestSetupRetryCarriesEveryEntry(t *testing.T) {
	r := newRig(t, bigMac())
	names := admitN(t, r, 2)
	_ = os.Chmod(r.dir, 0o755)
	calls, planted := 0, ""
	mid := func(c *SetupConfig) {
		c.beforeMove = func(dst string) error {
			if calls++; calls == 2 {
				planted = dst
				return os.Mkdir(dst, 0o755)
			}
			return nil
		}
	}
	if err := Setup(r.setupCfg(mid)); err == nil {
		t.Fatal("a move onto a planted name succeeded")
	}
	if n := everyEntry(t, r, names); n != len(names) {
		t.Fatalf("after the failed move %d of %d entries are on disk", n, len(names))
	}
	if err := Setup(r.setupCfg()); err == nil {
		t.Fatal("a retry succeeded while a planted name shadows an entry")
	} else if n := everyEntry(t, r, names); n != len(names) {
		t.Fatalf("after the refused retry %d of %d entries are on disk", n, len(names))
	}
	if err := os.Remove(planted); err != nil {
		t.Fatal(err)
	}
	if err := Setup(r.setupCfg()); err != nil {
		t.Fatal(err)
	}
	wantEntries(t, r, names, "the retry after a failed move")
	wantLayout(t, r)
}

// Two entries of one name (a staging leftover and the entries directory):
// Setup refuses and overwrites neither.
func TestSetupNeverOverwritesAnEntry(t *testing.T) {
	r := newRig(t, bigMac())
	stage := filepath.Join(filepath.Dir(r.dir), "."+entriesName+".1")
	if err := os.Mkdir(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	for d, body := range map[string]string{stage: "a", r.dir: "b"} {
		if err := os.WriteFile(filepath.Join(d, "e-x.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wantNotInstalled(t, Setup(r.setupCfg()), "two entries of one name")
	bodies := map[string]bool{}
	_ = filepath.Walk(filepath.Dir(r.dir), func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Name() == "e-x.json" {
			bodies[readFile(t, p)] = true
		}
		return nil
	})
	if !bodies["a"] || !bodies["b"] {
		t.Fatalf("an entry was overwritten: bodies left %v", bodies)
	}
}

// Entries in an earlier quarantine (in the base, or a rebuilt base's beside it)
// are carried forward; a quarantined login's base is not entries.
func TestSetupCarriesQuarantinedEntries(t *testing.T) {
	r := newRig(t, bigMac())
	base := filepath.Dir(r.dir)
	mk := func(dir string, uid uint32) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		r.fs.set(dir, uid)
		r.fs.set(filepath.Dir(dir), uid)
	}
	inBase := filepath.Join(base, ".wardyn-quarantine.a", entriesName)
	beside := filepath.Join(filepath.Dir(base), ".wardyn-quarantine.b", "Wardyn", entriesName)
	planted := filepath.Join(filepath.Dir(base), ".wardyn-quarantine.c", "Wardyn", entriesName)
	mk(inBase, 0)
	mk(beside, 0)
	mk(planted, 501)
	for d, n := range map[string]string{inBase: "e-1.json", beside: "e-2.json", planted: "e-forged.json"} {
		if err := os.WriteFile(filepath.Join(d, n), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := Setup(r.setupCfg()); err != nil {
		t.Fatal(err)
	}
	wantEntries(t, r, []string{"e-1.json", "e-2.json"}, "quarantined entries")
	if _, err := os.Lstat(filepath.Join(r.dir, "e-forged.json")); err == nil {
		t.Fatal("an entry from a login's quarantined base was carried")
	}
	wantLayout(t, r)
}

// Fable R4 L1: a lock open failing for any reason but ENOENT fails closed.
func TestSetupLockOpenErrorFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a mode-0 file")
	}
	r := newRig(t, bigMac())
	_ = os.Chmod(r.lock(), 0)
	defer os.Chmod(r.lock(), 0o644)
	was := ino(t, r.lock())
	wantNotInstalled(t, Setup(r.setupCfg()), "a lock that cannot be opened")
	if ino(t, r.lock()) != was {
		t.Fatal("a lock that could not be locked was replaced")
	}
}

// ERRATA-r6 E1: sock/ is made exact; its children must each be u<uid>, a real
// 0700 directory owned by that uid, or they go to a quarantine.
func TestSetupSockChildren(t *testing.T) {
	r := newRig(t, bigMac())
	if err := Setup(r.setupCfg()); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(filepath.Dir(r.dir), sockName)
	if perm(t, sock) != os.ModeSticky|0o777 {
		t.Fatalf("sock is %v", perm(t, sock))
	}
	dir := func(n string, uid uint32, mode os.FileMode) string {
		p := filepath.Join(sock, n)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(p, mode)
		r.fs.set(p, uid)
		return p
	}
	good := dir("u501", 501, 0o700)
	goodIno := ino(t, good)
	dir("u502", 501, 0o700)  // another uid's name
	dir("u503", 503, 0o755)  // not 0700
	dir("u0504", 504, 0o700) // not canonical
	if err := os.Symlink(t.TempDir(), filepath.Join(sock, "u505")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sock, "junk"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Setup(r.setupCfg()); err != nil {
		t.Fatal(err)
	}
	got, _ := readNames(sock)
	if len(got) != 1 || got[0] != "u501" || ino(t, good) != goodIno {
		t.Fatalf("sock holds %v after Setup, want only the sound u501, kept", got)
	}
	// A sock that is not exact is made again, its sound children moved across.
	_ = os.Chmod(sock, 0o755)
	if err := Setup(r.setupCfg()); err != nil {
		t.Fatal(err)
	}
	if perm(t, sock) != os.ModeSticky|0o777 || ino(t, filepath.Join(sock, "u501")) != goodIno {
		t.Fatal("a sock that was not exact was not rebuilt with its sound child")
	}
}
