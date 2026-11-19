// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// identicalOnBoth sets x.txt to the same bytes on both sides, with different times.
func identicalOnBoth(t *testing.T, box, root string) {
	t.Helper()
	at := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	for i, p := range []string{filepath.Join(root, "x.txt"), filepath.Join(box, "x.txt")} {
		putSyncFile(t, p, "the same edit", 0o644)
		if err := os.Chtimes(p, at.Add(time.Duration(i)*time.Hour), at.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
}

// Copies found identical are recorded only once that exact laptop file is synced, on both ways in:
// a recovered record and a file both sides changed. A failed sync of it, EINVAL included (only a
// directory sync may be passed over), is not preserved: nothing is recorded, no copy is made and
// the sandbox's file is left alone.
func TestPass_IdenticalCopiesAreRecordedOnlyOnceTheLaptopFileIsSynced(t *testing.T) {
	entries := map[string]func(t *testing.T) (*Syncer, string, string){
		"recovered record": func(t *testing.T) (*Syncer, string, string) {
			sy, box, root := runnerSyncer(t, true)
			file := filepath.Join(t.TempDir(), "run.json")
			putSyncFile(t, file, `{"local":`, 0o600)
			st, err := LoadStateRecover(file, root, box)
			if err != nil {
				t.Fatal(err)
			}
			sy.State, sy.StateFile, sy.StateRoot = st, file, filepath.Dir(file)
			return sy, box, root
		},
		"both changed": func(t *testing.T) (*Syncer, string, string) {
			sy, box, root := runnerSyncer(t, true)
			bothChanged(t, sy, box, root)
			return sy, box, root
		},
	}
	for name, entry := range entries {
		for _, fail := range []error{nil, syscall.EIO, syscall.EINVAL} {
			t.Run(name+"/"+errName(fail), func(t *testing.T) {
				sy, box, root := entry(t)
				identicalOnBoth(t, box, root)
				before := sy.State.Files["x.txt"]
				laptop := filepath.Join(root, "x.txt")
				recorded := func() bool {
					fi, err := os.Stat(laptop)
					return err == nil && sy.State.Files["x.txt"].Local == localSide(fi)
				}
				var events []string
				onSync(t, func(f *os.File) error {
					if a, err := f.Stat(); err == nil {
						if b, err := os.Stat(laptop); err == nil && os.SameFile(a, b) {
							events = append(events, "x.txt synced, recorded="+boolStr(recorded()))
							if fail != nil {
								return &os.PathError{Op: "sync", Path: f.Name(), Err: fail}
							}
						}
					}
					return nil
				}, nil)
				rep := pass(t, sy)
				if want := []string{"x.txt synced, recorded=false"}; !slices.Equal(events, want) {
					t.Errorf("events = %q, want %q", events, want)
				}
				if _, err := os.Stat(filepath.Join(root, copyName(1))); err == nil || getSyncFile(t, filepath.Join(box, "x.txt")) != "the same edit" {
					t.Errorf("a conflict copy, or the sandbox's file changed")
				}
				if fail == nil {
					if !recorded() || rep.NotPreserved != "" {
						t.Errorf("recorded = %v, report = %+v", recorded(), rep)
					}
					return
				}
				if recorded() || sy.State.Files["x.txt"] != before || !strings.Contains(rep.NotPreserved, "keep the local file x.txt") || !slices.Equal(rep.UnsyncedPaths, []string{"x.txt"}) {
					t.Errorf("recorded = %v, report = %+v", recorded(), rep)
				}
			})
		}
	}
}

func errName(err error) string {
	if err == nil {
		return "synced"
	}
	return strings.ReplaceAll(err.Error(), " ", "_")
}

// recordSites are the functions that may write State.Files directly, each with why. Any other is a
// record of a laptop file that skipped recordKept's syncs.
var recordSites = map[string]string{
	"recordKept":   "the one place a laptop file is recorded in step: it syncs the file and its directories first",
	"push":         "the laptop's file is the source: the record follows a write of the sandbox's copy",
	"renameRemote": "the laptop's file is the source: the record follows a rename of the sandbox's copy to match it",
}

// The guard: no other code in this package writes State.Files, by index or whole.
func TestGuard_LaptopFilesAreRecordedOnlyThroughRecordKept(t *testing.T) {
	found := map[string]bool{}
	for _, bad := range scanRecordWrites(t, ".") {
		if _, ok := recordSites[bad.fn]; ok {
			found[bad.fn] = true
			continue
		}
		t.Errorf("%s: %s writes State.Files outside recordKept; record a laptop file through recordKept", bad.pos, bad.fn)
	}
	for fn := range recordSites {
		if !found[fn] {
			t.Errorf("recordSites names %s, which no longer writes State.Files: drop it", fn)
		}
	}
}

type recordWrite struct{ pos, fn string }

// scanRecordWrites lists every assignment to a .Files index or to .Files itself in the package's
// non-test files under dir, with its enclosing function.
func scanRecordWrites(t *testing.T, dir string) []recordWrite {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var out []recordWrite
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, lhs := range as.Lhs {
					if ix, ok := lhs.(*ast.IndexExpr); ok {
						lhs = ix.X
					}
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Files" {
						out = append(out, recordWrite{fset.Position(as.Pos()).String(), fn.Name.Name})
					}
				}
				return true
			})
		}
	}
	return out
}

// The guard finds a stray record: a scan of a package with one fails it.
func TestGuard_FindsAStrayRecord(t *testing.T) {
	dir := t.TempDir()
	putSyncFile(t, filepath.Join(dir, "x.go"), "package foldersync\n\nfunc stray(sy *Syncer) { sy.State.Files[\"a\"] = FileSeen{} }\n", 0o644)
	if got := scanRecordWrites(t, dir); len(got) != 1 || got[0].fn != "stray" {
		t.Errorf("scan = %+v, want the stray write", got)
	}
}
