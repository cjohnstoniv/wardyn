// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// ShortTempDir is a temp dir with a short path: a macOS TMPDIR is about 49
// bytes, which would put a test's socket paths past the 103-byte limit.
func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "wm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// sockRig is an installed sock/ dir (sticky) under a short temp root. Tests
// run as one OS user, so ownership is simulated: everything reads as root's
// except paths under u<uid>, which read as owner (the login unless a test
// plants another owner).
type sockRig struct {
	c     SockConfig
	owner map[string]uint32
}

func newSockRig(t *testing.T) *sockRig {
	t.Helper()
	dir := filepath.Join(shortTempDir(t), "sock")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}
	r := &sockRig{owner: map[string]uint32{}}
	r.c = SockConfig{Dir: dir, UID: 501, Fstat: r.fstat}
	return r
}

func (r *sockRig) fstat(fd int, name string) (unix.Stat_t, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return st, err
	}
	st.Uid = 0
	if name == r.c.UserDir() || strings.HasPrefix(name, r.c.UserDir()+"/") {
		st.Uid = uint32(r.c.UID)
		if u, ok := r.owner[name]; ok {
			st.Uid = u
		}
	}
	return st, nil
}

func dirPerm(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// A new u<uid> and run dir are exactly 0700 even under a umask that would
// strip mkdir's mode (fchmod after the owner check), and a second make of the
// same run key is refused, never reused.
func TestSockDirsAreMadeExactAndNeverReused(t *testing.T) {
	r := newSockRig(t)
	old := syscall.Umask(0o277)
	defer syscall.Umask(old)
	p, err := r.c.MakeRunDir("0123456789ab")
	syscall.Umask(old)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(r.c.Dir, "u501", "0123456789ab"); p != want {
		t.Fatalf("run sock dir %s, want %s", p, want)
	}
	for _, d := range []string{r.c.UserDir(), p} {
		if m := dirPerm(t, d); m != 0o700 {
			t.Fatalf("%s mode %04o, want 0700", d, m)
		}
	}
	if _, err := r.c.CheckRunDir("0123456789ab"); err != nil {
		t.Fatalf("check of the made dir: %v", err)
	}
	if _, err := r.c.MakeRunDir("0123456789ab"); err == nil || !strings.Contains(err.Error(), "already at") {
		t.Fatalf("second make: %v, want a refusal", err)
	}
	if _, err := r.c.MakeRunDir("../x"); err == nil {
		t.Fatal("a key with a path in it was accepted")
	}
}

// Anything planted at u<uid> is refused by name and left as it was: a file,
// a link, another owner's directory, a directory of another mode.
func TestPlantedUserSockDirIsRefusedUntouched(t *testing.T) {
	cases := map[string]func(t *testing.T, r *sockRig){
		"file": func(t *testing.T, r *sockRig) {
			if err := os.WriteFile(r.c.UserDir(), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"link": func(t *testing.T, r *sockRig) {
			target := filepath.Join(filepath.Dir(r.c.Dir), "elsewhere")
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, r.c.UserDir()); err != nil {
				t.Fatal(err)
			}
		},
		"foreign": func(t *testing.T, r *sockRig) {
			if err := os.Mkdir(r.c.UserDir(), 0o700); err != nil {
				t.Fatal(err)
			}
			r.owner[r.c.UserDir()] = 666
		},
		"wrong mode": func(t *testing.T, r *sockRig) {
			if err := os.Mkdir(r.c.UserDir(), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(r.c.UserDir(), 0o755); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			r := newSockRig(t)
			plant(t, r)
			before, _ := os.Lstat(r.c.UserDir())
			_, err := r.c.MakeRunDir("0123456789ab")
			var sr *SockRefusal
			if !errors.As(err, &sr) || sr.Path != r.c.UserDir() {
				t.Fatalf("got %v, want a refusal of %s", err, r.c.UserDir())
			}
			if !strings.Contains(err.Error(), "this login's macOS VMs cannot start") {
				t.Fatalf("refusal %q does not name what it stops", err)
			}
			after, _ := os.Lstat(r.c.UserDir())
			if before.Mode() != after.Mode() {
				t.Fatalf("the planted name changed: %v -> %v", before.Mode(), after.Mode())
			}
			if _, err := os.Lstat(filepath.Join(r.c.UserDir(), "0123456789ab")); err == nil {
				t.Fatal("a run dir was made through the planted name")
			}
		})
	}
}

// A sock dir that is not root's sticky directory refuses like the ledger's.
func TestSockDirMustBeStickyRoot(t *testing.T) {
	r := newSockRig(t)
	if err := os.Chmod(r.c.Dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.MakeRunDir("0123456789ab"); err == nil || !strings.Contains(err.Error(), "reinstall") {
		t.Fatalf("got %v, want the not-installed refusal", err)
	}
}

// The sweeper removes only the run socket dirs whose run is gone.
func TestSweepRunSockDirs(t *testing.T) {
	r := newSockRig(t)
	for _, k := range []string{"aaaaaaaaaaaa", "bbbbbbbbbbbb"} {
		if _, err := r.c.MakeRunDir(k); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := r.c.SweepRunDirs(func(k string) bool { return k == "aaaaaaaaaaaa" })
	if err != nil || len(removed) != 1 || removed[0] != "aaaaaaaaaaaa" {
		t.Fatalf("removed %v, %v", removed, err)
	}
	if _, err := r.c.CheckRunDir("bbbbbbbbbbbb"); err != nil {
		t.Fatalf("a live run's sock dir went: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(r.c.UserDir(), "aaaaaaaaaaaa")); !os.IsNotExist(err) {
		t.Fatalf("the residue is still there: %v", err)
	}
}
