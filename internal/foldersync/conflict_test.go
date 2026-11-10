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
	sy.State, sy.StateFile = st, file
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
// and is writing it back): the new directories' parents are synced as they are made, then the copy's
// file, then its link, then its directory, and only after all of them may the push truncate the
// sandbox's file. A failed directory sync blocks the push.
func TestInstall_ConflictCopyInANewDirectorySyncsEverythingBeforeThePush(t *testing.T) {
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
			"dir a, copy present=false, sandbox=sandbox edit!",
			"dir ., copy present=false, sandbox=sandbox edit!",
			"file, copy absent=true, sandbox=sandbox edit!",
			"dir a/b, copy present=true, sandbox=sandbox edit!",
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
