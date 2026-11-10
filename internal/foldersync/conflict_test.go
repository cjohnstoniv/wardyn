// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const testTag = "ab12cd34"

func runnerSyncer(t *testing.T, pull bool) (*Syncer, string, string) {
	t.Helper()
	sy, box, root := pipeSyncer(t, pull)
	sy.Opts.RunnerPass, sy.Opts.RunTag = true, testTag
	return sy, box, root
}

// bothChanged is x.txt sent by a first pass, then changed on both sides: the record is stale.
func bothChanged(t *testing.T, sy *Syncer, box, root string) {
	t.Helper()
	putSyncFile(t, filepath.Join(root, "x.txt"), "base", 0o644)
	if _, err := sy.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Truncate(time.Second)
	for p, body := range map[string]string{filepath.Join(root, "x.txt"): "laptop edit", filepath.Join(box, "x.txt"): "sandbox edit!"} {
		putSyncFile(t, p, body, 0o644)
		at = at.Add(time.Hour)
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
}

func copyName(n int) string { return conflictNames("x.txt", testTag)[n-1] }

func TestConflictNames(t *testing.T) {
	got := conflictNames("dir/report.txt", testTag)
	if len(got) != 9 || got[0] != "dir/report (sandbox conflict ab12cd34).txt" || got[1] != "dir/report (sandbox conflict ab12cd34 2).txt" ||
		got[8] != "dir/report (sandbox conflict ab12cd34 9).txt" {
		t.Errorf("names = %q", got)
	}
	if got := conflictNames(".bashrc", testTag)[0]; got != ".bashrc (sandbox conflict ab12cd34)" {
		t.Errorf("dotfile name = %q", got)
	}
}

func TestPass_ConflictKeepsTheSandboxCopy(t *testing.T) {
	sy, box, root := runnerSyncer(t, true)
	bothChanged(t, sy, box, root)
	rep, err := sy.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := getSyncFile(t, filepath.Join(root, copyName(1))); got != "sandbox edit!" {
		t.Errorf("conflict copy = %q", got)
	}
	if got := getSyncFile(t, filepath.Join(box, "x.txt")); got != "laptop edit" || !slices.Equal(rep.Conflicts, []string{"x.txt"}) {
		t.Errorf("sandbox = %q, conflicts = %v: the laptop's copy should win once the sandbox's is kept", got, rep.Conflicts)
	}
}

// The sandbox copy reaches the disk, and its name, before the push may truncate the sandbox's file.
func TestInstall_Order(t *testing.T) {
	sy, box, root := runnerSyncer(t, true)
	bothChanged(t, sy, box, root)
	var events []string
	sandbox := func() string { return getSyncFile(t, filepath.Join(box, "x.txt")) }
	_, copyErr := os.Stat(filepath.Join(root, copyName(1)))
	onSync(t, func(f *os.File) error {
		if laptopTemp(f) {
			_, copyErr = os.Stat(filepath.Join(root, copyName(1)))
			events = append(events, "file sync, copy absent="+boolStr(os.IsNotExist(copyErr))+", sandbox="+sandbox())
		}
		return nil
	}, func(d *os.File) error {
		if isDir(d, root) {
			_, copyErr = os.Stat(filepath.Join(root, copyName(1)))
			events = append(events, "dir sync, copy present="+boolStr(copyErr == nil)+", sandbox="+sandbox())
		}
		return nil
	})
	if _, err := sy.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"file sync, copy absent=true, sandbox=sandbox edit!", "dir sync, copy present=true, sandbox=sandbox edit!"}
	if !dirSyncs {
		want = want[:1]
	}
	if !slices.Equal(events, want) || sandbox() != "laptop edit" {
		t.Errorf("events = %q, sandbox after = %q", events, sandbox())
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// A copy that cannot be proven kept refuses the push: the sandbox's file is never truncated, the
// file counts as not brought back, and its stale record stays for the next pass.
func TestInstall_ConflictSyncFailuresRefuseThePush(t *testing.T) {
	for name, fail := range map[string][2]func(*os.File) error{
		"file sync": {func(f *os.File) error {
			if laptopTemp(f) {
				return errFault
			}
			return nil
		}, nil},
		"directory sync": {nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			if name == "directory sync" {
				unixDirSync(t)
			}
			sy, box, root := runnerSyncer(t, true)
			bothChanged(t, sy, box, root)
			before := sy.State.Files["x.txt"]
			dir := fail[1]
			if name == "directory sync" {
				dir = func(d *os.File) error {
					if isDir(d, root) {
						return errFault
					}
					return nil
				}
			}
			onSync(t, fail[0], dir)
			rep, err := sy.Pass(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got := getSyncFile(t, filepath.Join(box, "x.txt")); got != "sandbox edit!" || len(rep.Pushed) != 0 {
				t.Errorf("sandbox = %q, pushed = %v: the push was not refused", got, rep.Pushed)
			}
			if !slices.Equal(rep.UnsyncedPaths, []string{"x.txt"}) || rep.Unsynced != 1 || sy.State.Files["x.txt"] != before {
				t.Errorf("unsynced = %v (%d), record changed = %v", rep.UnsyncedPaths, rep.Unsynced, sy.State.Files["x.txt"] != before)
			}
			if got := getSyncFile(t, filepath.Join(root, "x.txt")); got != "laptop edit" {
				t.Errorf("laptop = %q", got)
			}
		})
	}
}

func TestInstall_ConflictCopyNeverReplaces(t *testing.T) {
	t.Run("a taken name moves to the next", func(t *testing.T) {
		sy, box, root := runnerSyncer(t, true)
		bothChanged(t, sy, box, root)
		putSyncFile(t, filepath.Join(root, copyName(1)), "an earlier copy", 0o644)
		if _, err := sy.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
		if a, b := getSyncFile(t, filepath.Join(root, copyName(1))), getSyncFile(t, filepath.Join(root, copyName(2))); a != "an earlier copy" || b != "sandbox edit!" {
			t.Errorf("copy 1 = %q, copy 2 = %q", a, b)
		}
	})
	t.Run("the same bytes count as kept", func(t *testing.T) {
		sy, box, root := runnerSyncer(t, true)
		bothChanged(t, sy, box, root)
		putSyncFile(t, filepath.Join(root, copyName(1)), "sandbox edit!", 0o644)
		rep, err := sy.Pass(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(root, copyName(2))); err == nil || getSyncFile(t, filepath.Join(box, "x.txt")) != "laptop edit" || rep.Unsynced != 0 {
			t.Errorf("a second copy was made or the push refused: report = %+v", rep)
		}
	})
	t.Run("all nine taken refuses the push", func(t *testing.T) {
		sy, box, root := runnerSyncer(t, true)
		bothChanged(t, sy, box, root)
		for i := 1; i <= 9; i++ {
			putSyncFile(t, filepath.Join(root, copyName(i)), "other", 0o644)
		}
		rep, err := sy.Pass(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if getSyncFile(t, filepath.Join(box, "x.txt")) != "sandbox edit!" || !slices.Equal(rep.UnsyncedPaths, []string{"x.txt"}) {
			t.Errorf("sandbox = %q, unsynced = %v", getSyncFile(t, filepath.Join(box, "x.txt")), rep.UnsyncedPaths)
		}
		for i := 1; i <= 9; i++ {
			if getSyncFile(t, filepath.Join(root, copyName(i))) != "other" {
				t.Errorf("copy %d was replaced", i)
			}
		}
		if !slices.ContainsFunc(rep.Refused, func(r Refusal) bool { return r.Path == "x.txt" && strings.Contains(r.Reason, "taken") }) {
			t.Errorf("refused = %+v", rep.Refused)
		}
	})
}

// A pass that does not pull cannot keep the sandbox's copy, so a file both sides changed is left
// alone on both.
func TestPass_ARunnerSeedWithAKnownRecordTouchesNeitherSide(t *testing.T) {
	sy, box, root := runnerSyncer(t, false)
	bothChanged(t, sy, box, root)
	rep, err := sy.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if getSyncFile(t, filepath.Join(box, "x.txt")) != "sandbox edit!" || getSyncFile(t, filepath.Join(root, "x.txt")) != "laptop edit" ||
		len(rep.Pushed) != 0 || !slices.Equal(rep.Conflicts, []string{"x.txt"}) {
		t.Errorf("report = %+v", rep)
	}
}

func TestLoadState_MovesAnUnreadableRecordAsideAndCompares(t *testing.T) {
	sy, box, root := runnerSyncer(t, true)
	file := filepath.Join(t.TempDir(), "run.json")
	putSyncFile(t, file, `{"local":`, 0o600)
	st, err := LoadStateRecover(file, root, box)
	if err != nil || !strings.HasPrefix(st.Recovered, "run.json.unreadable-") || len(st.Files) != 0 {
		t.Fatalf("state = %+v, err = %v", st, err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Error("the unreadable record is still in place")
	}
	if got := getSyncFile(t, filepath.Join(filepath.Dir(file), st.Recovered)); got != `{"local":` {
		t.Errorf("moved-aside record = %q", got)
	}
	sy.State, sy.StateFile, sy.StateRoot = st, file, filepath.Dir(file)
	for p, body := range map[string]string{"same.txt": "equal", "diff.txt": "laptop"} {
		putSyncFile(t, filepath.Join(root, p), body, 0o644)
	}
	for p, body := range map[string]string{"same.txt": "equal", "diff.txt": "sandbox", "only.txt": "agent's"} {
		putSyncFile(t, filepath.Join(box, p), body, 0o644)
	}
	rep, err := sy.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sy.State.Files["same.txt"]; !ok || slices.Contains(rep.Conflicts, "same.txt") || slices.Contains(rep.Pushed, "same.txt") {
		t.Errorf("equal files are not simply recorded: %+v", rep)
	}
	if _, err := os.Stat(filepath.Join(root, conflictNames("same.txt", testTag)[0])); err == nil {
		t.Error("equal files got a conflict copy")
	}
	if got := getSyncFile(t, filepath.Join(root, conflictNames("diff.txt", testTag)[0])); got != "sandbox" || getSyncFile(t, filepath.Join(box, "diff.txt")) != "laptop" {
		t.Errorf("different files: copy = %q, sandbox = %q", got, getSyncFile(t, filepath.Join(box, "diff.txt")))
	}
	if getSyncFile(t, filepath.Join(root, "only.txt")) != "agent's" || getSyncFile(t, filepath.Join(box, "only.txt")) != "agent's" {
		t.Error("a sandbox-only file was not brought back as new")
	}
}

// `wardyn sync` keeps its documented rules: an unreadable record is refused naming the fix, and a
// file both sides changed is pushed (the laptop wins) and listed, with no copy made.
func TestSyncer_WithoutRunnerPassKeepsCLIRules(t *testing.T) {
	file := filepath.Join(t.TempDir(), "run.json")
	putSyncFile(t, file, `{"local":`, 0o600)
	if _, err := LoadState(file, "/l", "/r"); err == nil || !strings.Contains(err.Error(), "delete it to start over") {
		t.Errorf("err = %v", err)
	}
	if got := getSyncFile(t, file); got != `{"local":` {
		t.Error("the CLI moved the unreadable record")
	}
	for _, pull := range []bool{false, true} {
		sy, box, root := pipeSyncer(t, pull)
		bothChanged(t, sy, box, root)
		rep, err := sy.Pass(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if getSyncFile(t, filepath.Join(box, "x.txt")) != "laptop edit" || !slices.Equal(rep.Conflicts, []string{"x.txt"}) || rep.Unsynced != 0 {
			t.Errorf("pull=%v: report = %+v", pull, rep)
		}
		if ents, _ := os.ReadDir(root); len(ents) != 1 {
			t.Errorf("pull=%v: the laptop holds %d entries, want x.txt alone", pull, len(ents))
		}
	}
}

// A conflict copy whose directory is new on the laptop (the developer moved it away after the walk
// and is writing it back): the copy's file is synced, then it is linked, then its directory and every
// one above it are synced, the root last, and only after all of them may the push truncate the
// sandbox's file. A failed directory sync blocks the push.
func TestInstall_ConflictCopyInANewDirectorySyncsEverythingBeforeThePush(t *testing.T) {
	unixDirSync(t)
	for _, failDir := range []bool{false, true} {
		sy, box, root := runnerSyncer(t, true)
		sy.ig = newSyncIgnore(nil)
		rel := "a/b/x.txt"
		putSyncFile(t, filepath.Join(box, rel), "sandbox edit!", 0o644)
		r, err := sy.Client.Stat(filepath.Join(box, rel))
		if err != nil {
			t.Fatal(err)
		}
		copyRel := conflictNames(rel, testTag)[0]
		sandbox := func() string { return getSyncFile(t, filepath.Join(box, rel)) }
		var events []string
		onSync(t, func(f *os.File) error {
			if laptopTemp(f) {
				_, cerr := os.Stat(filepath.Join(root, copyRel))
				events = append(events, "file, copy absent="+boolStr(os.IsNotExist(cerr))+", sandbox="+sandbox())
			}
			return nil
		}, func(d *os.File) error {
			for _, p := range []string{".", "a", "a/b"} {
				if !isDir(d, filepath.Join(root, p)) {
					continue
				}
				_, cerr := os.Stat(filepath.Join(root, copyRel))
				events = append(events, "dir "+p+", copy present="+boolStr(cerr == nil)+", sandbox="+sandbox())
				if p == "a/b" && cerr == nil {
					// The developer's file is back before the push reads it.
					putSyncFile(t, filepath.Join(root, rel), "laptop edit", 0o644)
					if failDir {
						return errFault
					}
				}
			}
			return nil
		})
		l := syncFile{side: Side{Size: int64(len("laptop edit"))}, mode: 0o644}
		rep := NewReport()
		pulled := int64(0)
		if err := sy.conflict(context.Background(), rel, l, r, rep, &pulled); err != nil {
			t.Fatal(err)
		}
		want := []string{
			"file, copy absent=true, sandbox=sandbox edit!",
			"dir a/b, copy present=true, sandbox=sandbox edit!",
		}
		if !failDir {
			want = append(want, "dir a, copy present=true, sandbox=sandbox edit!", "dir ., copy present=true, sandbox=sandbox edit!")
		}
		if !slices.Equal(events, want) {
			t.Errorf("failDir=%v: events = %q\nwant %q", failDir, events, want)
		}
		if failDir {
			if sandbox() != "sandbox edit!" || len(rep.Pushed) != 0 || !slices.Equal(rep.UnsyncedPaths, []string{rel}) {
				t.Errorf("a failed directory sync did not block the push: sandbox = %q, report = %+v", sandbox(), rep)
			}
		} else if sandbox() != "laptop edit" || !slices.Equal(rep.Pushed, []string{rel}) {
			t.Errorf("sandbox = %q, pushed = %v", sandbox(), rep.Pushed)
		}
	}
}

// An identical copy already at a conflict name counts as kept only once that same file is synced:
// bytes an earlier write left visible but not on the disk are not yet kept. The sync comes before
// the push may truncate the sandbox's file, and a failed sync blocks the push without touching the
// existing copy.
//
// Every caller of an install treats any InstallError, Installed or not, as not done: the pull
// (engine.go pull: pathFailed, no record, and a pull never pushes), the conflict copy (conflict.go
// keepConflict: not kept, so conflict pushes nothing and counts the file unsynced) and the state
// save (rules.go Save: the error fails the pass, so nothing settles).
func TestInstall_AnIdenticalExistingCopyIsSyncedBeforeThePush(t *testing.T) {
	for _, fail := range []bool{false, true} {
		sy, box, root := runnerSyncer(t, true)
		bothChanged(t, sy, box, root)
		putSyncFile(t, filepath.Join(root, copyName(1)), "sandbox edit!", 0o644)
		sandbox := func() string { return getSyncFile(t, filepath.Join(box, "x.txt")) }
		var events []string
		onSync(t, func(f *os.File) error {
			if strings.Contains(f.Name(), "sandbox conflict") {
				events = append(events, "existing copy synced, sandbox="+sandbox())
				if fail {
					return errFault
				}
			}
			return nil
		}, nil)
		rep, err := sy.Pass(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"existing copy synced, sandbox=sandbox edit!"}; !slices.Equal(events, want) {
			t.Errorf("fail=%v: events = %q", fail, events)
		}
		if got := getSyncFile(t, filepath.Join(root, copyName(1))); got != "sandbox edit!" {
			t.Errorf("fail=%v: the existing copy = %q", fail, got)
		}
		if _, err := os.Stat(filepath.Join(root, copyName(2))); err == nil {
			t.Errorf("fail=%v: a second copy was made", fail)
		}
		if fail {
			if sandbox() != "sandbox edit!" || slices.Contains(rep.Pushed, "x.txt") || !slices.Equal(rep.UnsyncedPaths, []string{"x.txt"}) {
				t.Errorf("a failed sync of the existing copy did not block the push: sandbox = %q, report = %+v", sandbox(), rep)
			}
		} else if sandbox() != "laptop edit" || rep.Unsynced != 0 {
			t.Errorf("sandbox = %q, report = %+v", sandbox(), rep)
		}
	}
}

// A conflict copy in new directories whose first install could not sync the root: the push is
// refused. The retry finds the directories and the copy already in place and still syncs the root
// before the push may truncate the sandbox's file; a failure that repeats refuses the push again.
func TestInstall_ARetryAfterAFailedRootSyncSyncsTheRootBeforeThePush(t *testing.T) {
	unixDirSync(t)
	for _, again := range []bool{false, true} {
		sy, box, root := runnerSyncer(t, true)
		sy.ig = newSyncIgnore(nil)
		rel := "a/b/x.txt"
		putSyncFile(t, filepath.Join(box, rel), "sandbox edit!", 0o644)
		r, err := sy.Client.Stat(filepath.Join(box, rel))
		if err != nil {
			t.Fatal(err)
		}
		sandbox := func() string { return getSyncFile(t, filepath.Join(box, rel)) }
		l := syncFile{side: Side{Size: int64(len("laptop edit"))}, mode: 0o644}
		attempt := func(failRoot bool) (*Report, []string) {
			var events []string
			onSync(t, nil, func(d *os.File) error {
				if isDir(d, filepath.Join(root, "a", "b")) {
					if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
						// The developer's file is back before the push reads it.
						putSyncFile(t, filepath.Join(root, rel), "laptop edit", 0o644)
					}
				}
				if !isDir(d, root) {
					return nil
				}
				events = append(events, "dir ., sandbox="+sandbox())
				if failRoot {
					return errFault
				}
				return nil
			})
			rep, pulled := NewReport(), int64(0)
			if err := sy.conflict(context.Background(), rel, l, r, rep, &pulled); err != nil {
				t.Fatal(err)
			}
			return rep, events
		}
		refused := func(n int, rep *Report) {
			if sandbox() != "sandbox edit!" || len(rep.Pushed) != 0 || !slices.Equal(rep.UnsyncedPaths, []string{rel}) {
				t.Errorf("again=%v attempt %d: a failed root sync did not block the push: sandbox = %q, report = %+v", again, n, sandbox(), rep)
			}
		}
		want := []string{"dir ., sandbox=sandbox edit!"}
		rep, events := attempt(true)
		if !slices.Equal(events, want) {
			t.Errorf("again=%v attempt 1: events = %q", again, events)
		}
		refused(1, rep)
		rep, events = attempt(again)
		if !slices.Equal(events, want) {
			t.Errorf("again=%v attempt 2: events = %q, want the root synced before the push", again, events)
		}
		if again {
			refused(2, rep)
		} else if sandbox() != "laptop edit" || !slices.Equal(rep.Pushed, []string{rel}) {
			t.Errorf("attempt 2: sandbox = %q, pushed = %v", sandbox(), rep.Pushed)
		}
	}
}

// Copies that both changed, or that have no record, but hold the same bytes are in step: no
// conflict copy is made (a lost save must not make copies that use up the pull budget), nothing is
// pushed, and the record is brought up to date.
func TestPass_IdenticalCopiesAreRecordedNotConflicts(t *testing.T) {
	for _, recorded := range []bool{true, false} {
		sy, box, root := runnerSyncer(t, true)
		if recorded {
			bothChanged(t, sy, box, root)
		}
		at := time.Now().Add(2 * time.Hour).Truncate(time.Second)
		for i, p := range []string{filepath.Join(root, "x.txt"), filepath.Join(box, "x.txt")} {
			putSyncFile(t, p, "the same edit", 0o644)
			if err := os.Chtimes(p, at.Add(time.Duration(i)*time.Hour), at.Add(time.Duration(i)*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		rep, err := sy.Pass(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(root, copyName(1))); err == nil || len(rep.Conflicts) != 0 || len(rep.Pushed) != 0 || len(rep.Pulled) != 0 {
			t.Errorf("recorded=%v: a conflict copy or a transfer for identical copies: copy stat err = %v, report = %+v", recorded, err, rep)
		}
		lfi, _ := os.Stat(filepath.Join(root, "x.txt"))
		seen, ok := sy.State.Files["x.txt"]
		if !ok || seen.Local.Size != lfi.Size() || seen.Local.MTime != lfi.ModTime().UnixNano() {
			t.Errorf("recorded=%v: record = %+v, %v", recorded, seen, ok)
		}
		if again, err := sy.Pass(context.Background()); err != nil || len(again.Conflicts) != 0 || len(again.Pushed) != 0 {
			t.Errorf("recorded=%v: the next pass = %+v, %v", recorded, again, err)
		}
	}
}

// A recovering pass that ends before its save (refused, or stopped) leaves the next pass recovering:
// the record is still missing and the moved-aside one is beside it, so a file both sides hold is
// compared and never pushed over the sandbox's. Once a pass saves, a load is no longer recovering.
func TestLoadStateRecover_StaysRecoveringUntilASave(t *testing.T) {
	sy, box, root := runnerSyncer(t, false)
	file := filepath.Join(t.TempDir(), "run.json")
	putSyncFile(t, file, `{"local":`, 0o600)
	first, err := LoadStateRecover(file, root, box)
	if err != nil || first.Recovered == "" {
		t.Fatalf("state = %+v, err = %v", first, err)
	}
	// The pass is refused: nothing is saved. The next pass loads again.
	st, err := LoadStateRecover(file, root, box)
	if err != nil || st.Recovered != first.Recovered {
		t.Fatalf("after an unsaved pass: Recovered = %q, err = %v, want %q", st.Recovered, err, first.Recovered)
	}
	sy.State, sy.StateFile, sy.StateRoot = st, file, filepath.Dir(file)
	putSyncFile(t, filepath.Join(root, "x.txt"), "laptop", 0o644)
	putSyncFile(t, filepath.Join(box, "x.txt"), "sandbox", 0o644)
	rep, err := sy.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := getSyncFile(t, filepath.Join(box, "x.txt")); got != "sandbox" || slices.Contains(rep.Pushed, "x.txt") || !slices.Contains(rep.Conflicts, "x.txt") {
		t.Errorf("sandbox = %q, report = %+v: want x.txt compared, not pushed", got, rep)
	}
	if st, err := LoadStateRecover(file, root, box); err != nil || st.Recovered != "" {
		t.Errorf("after a saved pass: Recovered = %q, err = %v", st.Recovered, err)
	}
}
