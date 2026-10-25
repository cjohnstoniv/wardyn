// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	c := SetupConfig{Base: filepath.Dir(r.dir), Fstat: r.fs.fstat, Chown: func(*os.File) error { return nil }, LockWait: 5 * time.Second,
		Logf: func(string, ...any) {}}
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

// Two entries of one name, a staging leftover's and the entries directory's.
// Into an exact entries directory: dst's copy stays (it counts) and the
// leftover goes to a root-only quarantine. Into one that is not exact: Setup
// refuses. Neither is ever overwritten.
func TestSetupNeverOverwritesAnEntry(t *testing.T) {
	for _, exactDst := range []bool{true, false} {
		r := newRig(t, bigMac())
		base := filepath.Dir(r.dir)
		stage := filepath.Join(base, "."+entriesName+".1")
		if err := os.Mkdir(stage, 0o700); err != nil {
			t.Fatal(err)
		}
		for d, body := range map[string]string{stage: "a", r.dir: "b"} {
			if err := os.WriteFile(filepath.Join(d, "e-x.json"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if !exactDst {
			_ = os.Chmod(r.dir, 0o755)
			wantNotInstalled(t, Setup(r.setupCfg()), "two entries of one name")
		} else if err := Setup(r.setupCfg()); err != nil {
			t.Fatalf("two entries of one name, exact dst: %v", err)
		} else {
			q, _ := filepath.Glob(filepath.Join(base, ".wardyn-quarantine.*", "e-x.json"))
			if readFile(t, filepath.Join(r.dir, "e-x.json")) != "b" || len(q) != 1 || readFile(t, q[0]) != "a" || perm(t, filepath.Dir(q[0])) != 0o700 {
				t.Fatalf("exact dst: dst's copy was not kept, or the other copy is not in a root-only quarantine (%v)", q)
			}
		}
		bodies := map[string]bool{}
		_ = filepath.Walk(base, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.Name() == "e-x.json" {
				bodies[readFile(t, p)] = true
			}
			return nil
		})
		if !bodies["a"] || !bodies["b"] {
			t.Fatalf("exact dst %v: an entry was overwritten: bodies left %v", exactDst, bodies)
		}
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

// Review R4 L1 and R5 L3: a lock open failing for any reason but ENOENT fails
// closed: EACCES on a mode-0 lock (not as root, which opens it), and a socket
// at the lock's name, whose open fails for root too (ENXIO; EOPNOTSUPP on
// macOS). The macOS CI job runs this test under sudo.
func TestSetupLockOpenErrorFailsClosed(t *testing.T) {
	for _, c := range []struct {
		name  string
		plant func(lock string) error
	}{
		{"mode0", func(lock string) error {
			if os.Geteuid() == 0 {
				return errSkip
			}
			return os.Chmod(lock, 0)
		}},
		{"socket", func(lock string) error {
			if err := os.Remove(lock); err != nil {
				return err
			}
			fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
			if err != nil {
				return err
			}
			defer unix.Close(fd)
			return unix.Bind(fd, &unix.SockaddrUnix{Name: lock})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, bigMac())
			if err := c.plant(r.lock()); errors.Is(err, errSkip) {
				t.Skip("root opens a mode-0 file")
			} else if err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(r.lock(), 0o644)
			was := ino(t, r.lock())
			if err := Setup(r.setupCfg()); err == nil || !strings.Contains(err.Error(), lockFile+" cannot be opened") {
				t.Fatalf("a lock that cannot be opened: got %v, want a refusal naming the fix", err)
			}
			if ino(t, r.lock()) != was {
				t.Fatal("a lock that could not be locked was replaced")
			}
		})
	}
}

var errSkip = errors.New("skip")

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

// counted is a fresh helper's Counted() on r's ledger.
func counted(t *testing.T, r *rig) int {
	t.Helper()
	es, err := r.open(t, 900, "probe").Counted()
	if err != nil {
		t.Fatalf("Counted: %v", err)
	}
	return len(es)
}

// leftover moves names out of r's entries directory into dir, made 0700, as
// an earlier run left them in staging or a quarantine.
func leftover(t *testing.T, r *rig, dir string, names ...string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := os.Rename(filepath.Join(r.dir, n), filepath.Join(dir, n)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// clone copies r's entry n into dir as name, with n's owner.
func clone(t *testing.T, r *rig, n, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(readFile(t, filepath.Join(r.dir, n))), 0o644); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.owners[name] = r.owners[n]
	r.mu.Unlock()
}

func stageDir(r *rig, i int) string {
	return filepath.Join(filepath.Dir(r.dir), "."+entriesName+"."+strconv.Itoa(i))
}

// Review R5 M1 (ERRATA-r6 E13 e): a login's link .wardyn-quarantine.x -> the
// base's parent would alias the live entries directory as a source. The link
// is never followed but skipped and logged: Setup succeeds every time, and
// every entry still counts.
func TestSetupQuarantineLinkToParent(t *testing.T) {
	r := newRig(t, bigMac())
	admitN(t, r, 2)
	if err := Setup(r.setupCfg()); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(filepath.Dir(r.dir))
	link := filepath.Join(parent, ".wardyn-quarantine.x")
	if err := os.Symlink(parent, link); err != nil {
		t.Fatal(err)
	}
	var logged []string
	logf := func(c *SetupConfig) {
		c.Logf = func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }
	}
	for i := 1; i <= 3; i++ {
		if err := Setup(r.setupCfg(logf)); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if n := counted(t, r); n != 2 {
			t.Fatalf("run %d: a helper counts %d of 2 entries", i, n)
		}
	}
	if !strings.Contains(strings.Join(logged, "\n"), "skipped "+link) {
		t.Fatalf("the link was not logged as skipped: %q", logged)
	}
}

// ERRATA-r6 E13 (b): a source that is the same directory as the entries
// directory by its fstat (device and inode) is never read as a source.
func TestSetupDedupesSourcesByIdentity(t *testing.T) {
	r := newRig(t, bigMac())
	names := admitN(t, r, 1)
	stage := stageDir(r, 1)
	clone(t, r, names[0], stage, names[0])
	var dst unix.Stat_t
	if err := unix.Lstat(r.dir, &dst); err != nil {
		t.Fatal(err)
	}
	alias := func(c *SetupConfig) {
		c.Fstat = func(fd int, name string) (unix.Stat_t, error) {
			st, err := r.fs.fstat(fd, name)
			if name == stage {
				st.Dev, st.Ino = dst.Dev, dst.Ino
			}
			return st, err
		}
	}
	if err := Setup(r.setupCfg(alias)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(stage, names[0])); err != nil {
		t.Fatalf("an alias of the entries directory was read as a source: %v", err)
	}
	if n := counted(t, r); n != 1 {
		t.Fatalf("a helper counts %d of 1 entries", n)
	}
}

// ERRATA-r6 E13 (a): the merge runs under the old lock's flock, taken and
// rechecked (dev and inode) before anything moves: a helper admitting
// mid-merge waits and gets ErrBusy, and admits once Setup is done; a lock
// replaced between the open and the flock refuses with nothing moved.
func TestSetupMergeHoldsTheFlock(t *testing.T) {
	r := newRig(t, bigMac())
	names := admitN(t, r, 1)
	stage := leftover(t, r, stageDir(r, 1), names...)
	h := r.open(t, 502, "b", func(c *Config) { c.LockWait = 50 * time.Millisecond })
	var midErr error
	mid := func(c *SetupConfig) {
		c.beforeMove = func(string) error { _, midErr = h.Admit(runReq(8 * gib)); return nil }
	}
	if err := Setup(r.setupCfg(mid)); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(midErr, ErrBusy) {
		t.Fatalf("a helper admitting mid-merge: got %v, want ErrBusy", midErr)
	}
	if _, err := h.Admit(runReq(8 * gib)); err != nil {
		t.Fatalf("the helper after Setup: %v", err)
	}

	leftover(t, r, stage, names...)
	swap := func(c *SetupConfig) {
		c.afterFlock = func() {
			if err := os.WriteFile(r.lock()+".new", nil, 0o644); err != nil {
				t.Error(err)
			}
			if err := os.Rename(r.lock()+".new", r.lock()); err != nil {
				t.Error(err)
			}
		}
	}
	wantNotInstalled(t, Setup(r.setupCfg(swap)), "a lock replaced before its flock")
	if _, err := os.Lstat(filepath.Join(stage, names[0])); err != nil {
		t.Fatalf("an entry moved after the lock recheck failed: %v", err)
	}
}

// ERRATA-r6 E13 (d): an entry gone when its move comes (its owner removed it)
// is skipped: Setup succeeds and every other entry counts.
func TestSetupEntryGoneMidMerge(t *testing.T) {
	r := newRig(t, bigMac())
	names := admitN(t, r, 2)
	stage := stageDir(r, 1)
	clone(t, r, names[1], stage, "e-gone.json")
	leftover(t, r, stage, names[1])
	gone := func(c *SetupConfig) {
		c.beforeMove = func(to string) error {
			if filepath.Base(to) == "e-gone.json" {
				return os.Remove(filepath.Join(stage, "e-gone.json"))
			}
			return nil
		}
	}
	if err := Setup(r.setupCfg(gone)); err != nil {
		t.Fatalf("an entry gone mid-merge: %v", err)
	}
	wantEntries(t, r, names, "an entry gone mid-merge")
	if n := counted(t, r); n != 2 {
		t.Fatalf("a helper counts %d of 2 entries", n)
	}
}

// ERRATA-r6 E13 (e) and (c): after any failed Setup a helper on the exact
// ledger counts at least what it counted before, and every entry is on disk
// once. Each run fails at a different point: either move, or after the sweep.
func TestSetupFailedSetupNeverLowersTheCount(t *testing.T) {
	for k := 1; k <= 3; k++ {
		r := newRig(t, bigMac())
		names := admitN(t, r, 2)
		clone(t, r, names[0], r.dir, "e-3.json")
		clone(t, r, names[0], r.dir, "e-4.json")
		leftover(t, r, stageDir(r, 1), "e-3.json", "e-4.json")
		start := counted(t, r)
		calls := 0
		fail := func(c *SetupConfig) {
			c.beforeMove = func(string) error {
				if calls++; calls == k {
					return errors.New("planted failure")
				}
				return nil
			}
			c.afterSock = func() {
				if k == 3 {
					_ = os.Chmod(filepath.Join(filepath.Dir(r.dir), sockName), 0o755)
				}
			}
		}
		if err := Setup(r.setupCfg(fail)); err == nil {
			t.Fatalf("failure point %d: Setup succeeded", k)
		}
		if n := counted(t, r); n < start {
			t.Fatalf("failure point %d: a helper counts %d, down from %d", k, n, start)
		}
		all := append([]string{"e-3.json", "e-4.json"}, names...)
		if n := everyEntry(t, r, all); n != len(all) {
			t.Fatalf("failure point %d: %d copies of %d entries on disk", k, n, len(all))
		}
	}
}

// ERRATA-r6 E13 (e): after a successful Setup a helper counts every distinct
// entry name found in the entries directory and every source.
func TestSetupCountsEveryDistinctName(t *testing.T) {
	r := newRig(t, bigMac())
	names := admitN(t, r, 2)
	base := filepath.Dir(r.dir)
	stage := leftover(t, r, stageDir(r, 1), names[1])
	clone(t, r, names[0], stage, names[0]) // a second copy of a name dst holds
	clone(t, r, names[0], filepath.Join(base, ".wardyn-quarantine.a", entriesName), "e-3.json")
	clone(t, r, names[0], filepath.Join(filepath.Dir(base), ".wardyn-quarantine.b", "Wardyn", entriesName), "e-4.json")
	if err := Setup(r.setupCfg()); err != nil {
		t.Fatal(err)
	}
	if n := counted(t, r); n != 4 {
		t.Fatalf("a helper counts %d of 4 distinct names", n)
	}
	wantEntries(t, r, append([]string{"e-3.json", "e-4.json"}, names...), "every distinct name")
	wantLayout(t, r)
}

// Review R5 L1: a child a login makes in sock/ after the sweep is left for the
// next sweep. It neither fails the install nor passes as sound.
func TestSetupSockChildAfterSweep(t *testing.T) {
	r := newRig(t, bigMac())
	if err := Setup(r.setupCfg()); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(filepath.Dir(r.dir), sockName)
	late := func(c *SetupConfig) {
		c.afterSock = func() {
			_ = os.Mkdir(filepath.Join(sock, "x"), 0o755)
			_ = os.Mkdir(filepath.Join(sock, "u77"), 0o755)
		}
	}
	if err := Setup(r.setupCfg(late)); err != nil {
		t.Fatalf("a child made after the sweep failed the install: %v", err)
	}
	if err := Setup(r.setupCfg()); err != nil {
		t.Fatal(err)
	}
	if got, _ := readNames(sock); len(got) != 0 {
		t.Fatalf("the next sweep left %v in sock", got)
	}
}

// Review R5 L2: an empty staging directory an earlier run left is removed.
func TestSetupRemovesEmptyStaging(t *testing.T) {
	r := newRig(t, bigMac())
	if err := os.Mkdir(stageDir(r, 1), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Setup(r.setupCfg()); err != nil {
		t.Fatal(err)
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(r.dir), "."+entriesName+".*")); len(m) != 0 {
		t.Fatalf("staging directories left: %v", m)
	}
}

// drift makes r's entries directory one a helper accepts (root-owned, sticky)
// that is not exact: root:wheel 03777, or root 01777 with group staff. It
// returns the fstat Setup runs with.
func drift(t *testing.T, r *rig, kind string) func(int, string) (unix.Stat_t, error) {
	t.Helper()
	was := ino(t, r.dir)
	if kind == "03777" {
		if err := os.Chmod(r.dir, os.ModeSticky|os.ModeSetgid|0o777); err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Lstat(r.dir); err != nil || fi.Mode()&os.ModeSetgid == 0 {
			t.Fatalf("setgid did not stick on %s (%v)", r.dir, err)
		}
	}
	return func(fd int, name string) (unix.Stat_t, error) {
		st, err := r.fs.fstat(fd, name)
		if kind == "group" && name == r.dir && st.Ino == was {
			st.Gid = 20
		}
		return st, err
	}
}

// setupDies runs Setup, which may end its goroutine mid-run (runtime.Goexit,
// a process death: no error path runs, and closing its files drops the flock).
func setupDies(c SetupConfig) (err error, died bool) {
	done := make(chan struct{})
	died = true
	go func() {
		defer close(done)
		err = Setup(c)
		died = false
	}()
	<-done
	return err, died
}

// DR-0431 residual: a migration of a helper-accepted entries directory that is
// not exact fails, or the installer dies, after one entry is moved. Before
// any retry a fresh helper still counts both entries and refuses a third VM;
// the retry then ends with each entry exactly once.
func TestSetupFailedMigrationKeepsEveryEntryCounted(t *testing.T) {
	for _, kind := range []string{"03777", "group"} {
		for _, death := range []bool{false, true} {
			what := fmt.Sprintf("%s, died %v", kind, death)
			t.Run(what, func(t *testing.T) {
				r := newRig(t, bigMac())
				names := admitN(t, r, 2)
				fstat := drift(t, r, kind)
				calls := 0
				cut := func(c *SetupConfig) {
					c.Fstat = fstat
					c.beforeMove = func(string) error {
						if calls++; calls < 2 {
							return nil
						}
						if death {
							runtime.Goexit()
						}
						return errors.New("planted failure")
					}
				}
				err, died := setupDies(r.setupCfg(cut))
				if calls != 2 || died != death || (!death && err == nil) {
					t.Fatalf("%s: Setup stopped after %d moves (died %v, err %v)", what, calls, died, err)
				}
				if n := counted(t, r); n != 2 {
					t.Fatalf("%s: before the retry a fresh helper counts %d of 2 entries", what, n)
				}
				if _, err := r.open(t, 503, "fresh").Admit(runReq(8 * gib)); !IsRefusal(err) {
					t.Fatalf("%s: before the retry a fresh helper admitted a third VM (%v)", what, err)
				}
				if err := Setup(r.setupCfg(func(c *SetupConfig) { c.Fstat = fstat })); err != nil {
					t.Fatalf("%s: the retry: %v", what, err)
				}
				if n, m := counted(t, r), everyEntry(t, r, names); n != 2 || m != 2 {
					t.Fatalf("%s: after the retry a helper counts %d, and %d copies of 2 entries are on disk", what, n, m)
				}
				if m, _ := filepath.Glob(filepath.Join(filepath.Dir(r.dir), "."+entriesName+"-link.*")); len(m) != 0 {
					t.Fatalf("%s: link directories left: %v", what, m)
				}
				wantEntries(t, r, names, what)
				wantLayout(t, r)
			})
		}
	}
}

// DR-0431 residual: an entry its owner releases between a failed migration and
// the retry stays released.
func TestSetupRetryNeverResurrectsAReleasedEntry(t *testing.T) {
	r := newRig(t, bigMac())
	var ls []*Ledger
	var es []Entry
	for i := 0; i < 2; i++ {
		l := r.open(t, 501+i, fmt.Sprintf("r%d", i))
		e, err := l.Admit(runReq(8 * gib))
		if err != nil {
			t.Fatal(err)
		}
		ls, es = append(ls, l), append(es, e)
	}
	fstat := drift(t, r, "03777")
	first, calls := "", 0
	fail := func(c *SetupConfig) {
		c.Fstat = fstat
		c.beforeMove = func(dst string) error {
			if calls++; calls == 1 {
				first = filepath.Base(dst)
				return nil
			}
			return errors.New("planted failure")
		}
	}
	if err := Setup(r.setupCfg(fail)); err == nil {
		t.Fatal("Setup succeeded")
	}
	if n := counted(t, r); n != 2 {
		t.Fatalf("before the release a helper counts %d of 2 entries", n)
	}
	gone, kept := 0, 1
	if es[1].file == first {
		gone, kept = 1, 0
	}
	if es[gone].file != first {
		t.Fatalf("the first entry moved, %s, is neither admitted one", first)
	}
	if err := ls[gone].Release(es[gone].ID, Destroyed{Stopped: true}); err != nil {
		t.Fatal(err)
	}
	if err := Setup(r.setupCfg(func(c *SetupConfig) { c.Fstat = fstat })); err != nil {
		t.Fatal(err)
	}
	if n := counted(t, r); n != 1 {
		t.Fatalf("after the retry a helper counts %d, not the 1 entry left", n)
	}
	if n := everyEntry(t, r, []string{first}); n != 0 {
		t.Fatalf("the released entry %s is back: %d copies on disk", first, n)
	}
	wantEntries(t, r, []string{es[kept].file}, "the entry still held")
	wantLayout(t, r)
}

// Review DR-0431 note: an fstat of the lock that fails is an error, before
// anything is replaced.
func TestSetupLockFstatErrorFailsClosed(t *testing.T) {
	r := newRig(t, bigMac())
	was := ino(t, r.lock())
	fail := func(c *SetupConfig) {
		c.Fstat = func(fd int, name string) (unix.Stat_t, error) {
			if name == r.lock() {
				return unix.Stat_t{}, unix.EIO
			}
			return r.fs.fstat(fd, name)
		}
	}
	if err := Setup(r.setupCfg(fail)); !errors.Is(err, unix.EIO) {
		t.Fatalf("Setup with the lock's fstat failing: %v", err)
	}
	if ino(t, r.lock()) != was {
		t.Fatal("Setup replaced the lock it could not fstat")
	}
}

// DR-0440: an entries directory whose fstat fails once (EIO) may hold
// entries, so Setup refuses before it changes anything; a fresh helper still
// counts both entries, and the retry keeps them.
func TestSetupEntriesInspectErrorFailsClosed(t *testing.T) {
	r := newRig(t, bigMac())
	names := admitN(t, r, 2)
	dirIno, lockIno := ino(t, r.dir), ino(t, r.lock())
	failed := false
	once := func(c *SetupConfig) {
		c.Fstat = func(fd int, name string) (unix.Stat_t, error) {
			if name == r.dir && !failed {
				failed = true
				return unix.Stat_t{}, unix.EIO
			}
			return r.fs.fstat(fd, name)
		}
	}
	if err := Setup(r.setupCfg(once)); err == nil || !failed {
		t.Fatalf("Setup with the entries directory's fstat failing: %v (failed %v)", err, failed)
	}
	if ino(t, r.dir) != dirIno || ino(t, r.lock()) != lockIno {
		t.Fatal("a refused Setup replaced the entries directory or the lock")
	}
	if got := entryFiles(t, r.dir); len(got) != len(names) {
		t.Fatalf("the entries directory holds %v, not %v", got, names)
	}
	wantEntries(t, r, names, "a refused Setup")
	if n := counted(t, r); n != 2 {
		t.Fatalf("before the retry a fresh helper counts %d of 2 entries", n)
	}
	if _, err := r.open(t, 503, "fresh").Admit(runReq(8 * gib)); !IsRefusal(err) {
		t.Fatalf("before the retry a fresh helper admitted a third VM (%v)", err)
	}
	if err := Setup(r.setupCfg()); err != nil {
		t.Fatal(err)
	}
	if n := counted(t, r); n != 2 {
		t.Fatalf("after the retry a helper counts %d of 2 entries", n)
	}
	wantEntries(t, r, names, "the retry after a failed inspection")
	wantLayout(t, r)
}

// DR-0440: what is at the entries name and is not a directory (a file, a
// link) is still replaced: it moves to a quarantine, nothing it names is
// touched, and the layout is made.
func TestSetupReplacesEntriesThatAreNotADirectory(t *testing.T) {
	for _, kind := range []string{"file", "link"} {
		r := newRig(t, bigMac())
		target := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.RemoveAll(r.dir)
		var err error
		if kind == "file" {
			err = os.WriteFile(r.dir, []byte("planted"), 0o644)
		} else {
			err = os.Symlink(target, r.dir)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := Setup(r.setupCfg()); err != nil {
			t.Fatalf("%s at the entries name: %v", kind, err)
		}
		q, _ := filepath.Glob(filepath.Join(filepath.Dir(r.dir), ".wardyn-quarantine.*", entriesName))
		if len(q) != 1 || readFile(t, target) != "keep" {
			t.Fatalf("%s at the entries name: quarantined %v, target %q", kind, q, readFile(t, target))
		}
		wantLayout(t, r)
	}
}

// A migration source that cannot be inspected (EIO on its fstat) may hold
// entries, so Setup refuses rather than skip it. The witness: a base that is
// not exact is rebuilt, its entries directory now in a quarantine beside it.
// Before the retry a fresh helper refuses (the new base has no lock yet), never
// counting 0; the retry carries both entries.
func TestSetupSourceInspectErrorFailsClosed(t *testing.T) {
	r := newRig(t, bigMac())
	names := admitN(t, r, 2)
	base := filepath.Dir(r.dir)
	oldBase := ino(t, base)
	failed := false
	cfg := func(eio bool) func(*SetupConfig) {
		return func(c *SetupConfig) {
			c.Fstat = func(fd int, name string) (unix.Stat_t, error) {
				st, err := r.fs.fstat(fd, name)
				if name == base && st.Ino == oldBase {
					setPerm(&st.Mode, 0o700) // root-only, but not the exact 0755
				}
				q := filepath.Base(filepath.Dir(filepath.Dir(name)))
				if eio && !failed && filepath.Base(name) == entriesName && strings.HasPrefix(q, quarantinePrefix) {
					failed = true
					return unix.Stat_t{}, unix.EIO
				}
				return st, err
			}
		}
	}
	if err := Setup(r.setupCfg(cfg(true))); err == nil || !failed {
		t.Fatalf("Setup with a source's fstat failing: %v (failed %v)", err, failed)
	}
	if err := r.openErr(); err == nil {
		if n := counted(t, r); n != 2 {
			t.Fatalf("before the retry a fresh helper counts %d of 2 entries", n)
		}
	}
	if n := everyEntry(t, r, names); n != 2 {
		t.Fatalf("after the refused Setup %d copies of 2 entries are on disk", n)
	}
	if err := Setup(r.setupCfg(cfg(false))); err != nil {
		t.Fatal(err)
	}
	if n := counted(t, r); n != 2 {
		t.Fatalf("after the retry a helper counts %d of 2 entries", n)
	}
	wantEntries(t, r, names, "the retry after a failed source inspection")
	wantLayout(t, r)
}

// Untrusted preplants are still skipped, not errors: a link and a file at a
// quarantine's entries name, and a login's quarantine beside the base.
func TestSetupSkipsUntrustedSources(t *testing.T) {
	r := newRig(t, bigMac())
	base := filepath.Dir(r.dir)
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, "e-link.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{".wardyn-quarantine.a", ".wardyn-quarantine.b"} {
		if err := os.Mkdir(filepath.Join(base, q), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(elsewhere, filepath.Join(base, ".wardyn-quarantine.a", entriesName)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, ".wardyn-quarantine.b", entriesName), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	login := filepath.Join(filepath.Dir(base), ".wardyn-quarantine.c", "Wardyn", entriesName)
	if err := os.MkdirAll(login, 0o755); err != nil {
		t.Fatal(err)
	}
	r.fs.set(filepath.Dir(filepath.Dir(login)), 501)
	if err := os.WriteFile(filepath.Join(login, "e-login.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Setup(r.setupCfg()); err != nil {
		t.Fatalf("untrusted sources failed the install: %v", err)
	}
	if got := entryFiles(t, r.dir); len(got) != 0 {
		t.Fatalf("entries carried from untrusted sources: %v", got)
	}
	wantLayout(t, r)
}
