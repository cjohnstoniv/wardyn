// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

// The tests' directories are the test user's, not root's.
func TestMain(m *testing.M) {
	trustedOwnerUID = uint32(os.Getuid())
	os.Exit(m.Run())
}

func writeNew(f *os.File) error { _, err := f.WriteString("new"); return err }

func mkdirMode(t *testing.T, p string, mode os.FileMode) string {
	t.Helper()
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func setVar[T any](t *testing.T, v *T, to T) {
	old := *v
	*v = to
	t.Cleanup(func() { *v = old })
}

// A directory anyone may change, on the way to dst, refuses the write: no temp file, dst unchanged.
func TestReplaceFileRefusesAWorldWritableParent(t *testing.T) {
	ww := mkdirMode(t, filepath.Join(t.TempDir(), "ww"), 0o777)
	inner := mkdirMode(t, filepath.Join(ww, "inner"), 0o755)
	dst := filepath.Join(inner, "dst")
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(dst, 0o644, writeNew); !errors.Is(err, ErrUnsafeParent) {
		t.Fatalf("replaceFile through a 0777 dir = %v, want ErrUnsafeParent", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "old" || !slices.Equal(entries(t, inner), []string{"dst"}) {
		t.Errorf("dst = %q, dir holds %q", got, entries(t, inner))
	}
}

// A link among dst's parents is not followed.
func TestReplaceFileRefusesALinkedParent(t *testing.T) {
	dir := t.TempDir()
	denied := mkdirMode(t, filepath.Join(dir, "denied"), 0o755)
	if err := os.Symlink(denied, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(filepath.Join(dir, "link", "f"), 0o644, writeNew); !errors.Is(err, ErrUnsafeParent) {
		t.Fatalf("replaceFile through a link = %v, want ErrUnsafeParent", err)
	}
	if got := entries(t, denied); len(got) != 0 {
		t.Errorf("the link's target holds %q", got)
	}
}

// A sticky world-writable parent, a group-writable one of a trusted group, and an owner the walk
// trusts are allowed; another group's write, or another owner, is refused.
func TestReplaceFileParentOwnershipAndModes(t *testing.T) {
	dir := t.TempDir()
	sticky := mkdirMode(t, filepath.Join(dir, "sticky"), 0o1777)
	if err := replaceFile(filepath.Join(sticky, "f"), 0o644, writeNew); err != nil {
		t.Errorf("a 1777 parent: %v", err)
	}
	group := mkdirMode(t, filepath.Join(dir, "group"), 0o775)
	fi, err := os.Stat(group)
	if err != nil {
		t.Fatal(err)
	}
	gid := fi.Sys().(*syscall.Stat_t).Gid
	setVar(t, &trustedGroupGIDs, []uint32{0, gid})
	if err := replaceFile(filepath.Join(group, "f"), 0o644, writeNew); err != nil {
		t.Errorf("a 0775 parent of a trusted group: %v", err)
	}
	if gid != 0 && gid != 80 {
		trustedGroupGIDs = []uint32{0, 80}
		if err := replaceFile(filepath.Join(group, "g"), 0o644, writeNew); !errors.Is(err, ErrUnsafeParent) {
			t.Errorf("a 0775 parent of gid %d = %v, want ErrUnsafeParent", gid, err)
		}
	}
	setVar(t, &trustedOwnerUID, uint32(os.Getuid())+1)
	if err := replaceFile(filepath.Join(dir, "f"), 0o644, writeNew); !errors.Is(err, ErrUnsafeParent) {
		t.Errorf("a parent another user owns = %v, want ErrUnsafeParent", err)
	}
}

// A refused path is refused by what it is, not how dst spells it: a walk through a refused directory
// named another way, or a dst that is another name of a refused file.
func TestReplaceFileOutsideRefusesARefusedInode(t *testing.T) {
	dir := t.TempDir()
	r := mkdirMode(t, filepath.Join(dir, "R"), 0o755)
	sub := mkdirMode(t, filepath.Join(r, "sub"), 0o755)
	if err := os.Symlink(r, filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := replaceFileOutside(filepath.Join(sub, "f"), 0o644, []string{filepath.Join(dir, "alias")}, writeNew); !errors.Is(err, ErrItemDest) {
		t.Errorf("a write inside a refused dir = %v, want ErrItemDest", err)
	}
	if got := entries(t, sub); len(got) != 0 {
		t.Errorf("the refused dir holds %q", got)
	}
	other := mkdirMode(t, filepath.Join(dir, "other"), 0o755)
	keep := filepath.Join(other, "keep")
	if err := os.WriteFile(keep, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(keep, filepath.Join(other, "hl")); err != nil {
		t.Fatal(err)
	}
	refused := []string{keep, filepath.Join(dir, "missing")}
	if err := replaceFileOutside(filepath.Join(other, "hl"), 0o644, refused, writeNew); !errors.Is(err, ErrItemDest) {
		t.Errorf("a dst that is a refused file's other name = %v, want ErrItemDest", err)
	}
	if got, _ := os.ReadFile(keep); string(got) != "keep" {
		t.Errorf("the refused file changed: %q", got)
	}
	if err := replaceFileOutside(filepath.Join(other, "x"), 0o644, refused, writeNew); err != nil {
		t.Errorf("a write beside them: %v", err)
	}
}

// A file item's dest is ASCII only: a spelling APFS folds onto a refused path is never compared.
func TestItemDestRefusesNonASCII(t *testing.T) {
	for _, d := range []string{
		"/Library/Keychainſ/System.keychain",           // LATIN SMALL LETTER LONG S
		"/Library/Keychains/System.keychain",           // KELVIN SIGN
		"/Library/Application Support/Café/conf.json", // NFD accent
		"/Library/Application Support/Café/conf.json",  // NFC accent
		"/LIBRARY/KEYCHAINS/System.keychain",           // ASCII case still folds
	} {
		if err := ItemDestAllowed(d); !errors.Is(err, ErrItemDest) {
			t.Errorf("ItemDestAllowed(%q) = %v, want ErrItemDest", d, err)
		}
		if err := ToolsExecAllowed([]string{"/bin/mkdir", "-p", "-m", "0755", filepath.Dir(d)}, t.TempDir()); err == nil {
			t.Errorf("mkdir of %q allowed", filepath.Dir(d))
		}
	}
	for _, d := range []string{"/Library/Application Support/Org/conf.json", "/usr/local/etc/org.conf", "/opt/org/x"} {
		if err := ItemDestAllowed(d); err != nil {
			t.Errorf("ItemDestAllowed(%q) = %v", d, err)
		}
	}
}

// A file item's directory is made by the walk: missing components 0755 whatever the umask, and no
// link or world-writable directory on the way.
func TestMakeDirAll(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	dir := t.TempDir()
	if err := makeDirAll(filepath.Join(dir, "a", "b", "c"), nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a", "a/b", "a/b/c"} {
		if fi, err := os.Lstat(filepath.Join(dir, p)); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o755 {
			t.Errorf("%s: %v %v, want a 0755 dir", p, fi.Mode(), err)
		}
	}
	if err := makeDirAll(filepath.Join(dir, "a", "b"), nil); err != nil {
		t.Errorf("an existing dir: %v", err)
	}
	target := mkdirMode(t, filepath.Join(dir, "real"), 0o755)
	if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := makeDirAll(filepath.Join(dir, "link", "new"), nil); !errors.Is(err, ErrUnsafeParent) {
		t.Errorf("mkdir through a link = %v, want ErrUnsafeParent", err)
	}
	ww := mkdirMode(t, filepath.Join(dir, "ww"), 0o777)
	if err := makeDirAll(filepath.Join(ww, "new"), nil); !errors.Is(err, ErrUnsafeParent) {
		t.Errorf("mkdir in a 0777 dir = %v, want ErrUnsafeParent", err)
	}
	if len(entries(t, target)) != 0 || len(entries(t, ww)) != 0 {
		t.Errorf("a refused mkdir made %q, %q", entries(t, target), entries(t, ww))
	}
}

// The agent makes a file item's directory itself; it never runs mkdir for it.
func TestToolsExecMakesAnItemDirItself(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("as root the walk would make /Library on this host")
	}
	rig := newToolsRig(t)
	rig.exec("/bin/mkdir", "-p", "-m", "0755", "/Library/Application Support/Org")
	rig.exec("/bin/mkdir", "-p", "-m", "0700", MacItemStageDir)
	if !slices.EqualFunc(rig.ran, [][]string{{"/bin/mkdir", "-p", "-m", "0700", MacItemStageDir}}, slices.Equal[[]string]) {
		t.Errorf("ran %q, want only the stage dir's mkdir", rig.ran)
	}
}
