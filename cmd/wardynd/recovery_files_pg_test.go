// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/recoveryset"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	goodCast      = "{\"version\":2,\"width\":80,\"height\":24}\n[0.1,\"o\",\"hi\"]\n"
	truncatedCast = "{\"version\":2,\"width\":80,\"height\":24}\n[0.1,\"o\",\"h"
)

// addDrive registers a drive in the rig's target database, as the dump carries it.
func (rg *rehearseRig) addDrive() types.UserDrive {
	rg.t.Helper()
	pool, err := db.Connect(context.Background(), rg.targetDSN)
	if err != nil {
		rg.t.Fatal(err)
	}
	defer pool.Close() // the rehearsal refuses a target that has another client
	d, err := store.PG{Pool: pool}.UpsertUserDrive(context.Background(), types.UserDrive{
		Name: "team-share", Backend: types.DriveBackendDockerVolume, HomeTemplate: types.HomeTemplateHash,
		ObjectScheme: types.DriveObjectSchemeID, Reclaim: types.DriveReclaimRetain,
	}, false)
	if err != nil {
		rg.t.Fatal(err)
	}
	return d
}

// addCasts stores recordings in the rig's target database.
func (rg *rehearseRig) addCasts(casts map[string]string) {
	rg.t.Helper()
	pool, err := db.Connect(context.Background(), rg.targetDSN)
	if err != nil {
		rg.t.Fatal(err)
	}
	defer pool.Close()
	for key, body := range casts {
		if _, err := pool.Exec(context.Background(), `INSERT INTO recordings (cast_key, payload) VALUES ($1, $2)`, key, []byte(body)); err != nil {
			rg.t.Fatal(err)
		}
	}
}

// runSet rehearses the set at dir with the rig's key, reading the set from its manifest as main does.
func (rg *rehearseRig) runSet(dir string) rehearsal {
	rg.t.Helper()
	m, err := readRecoveryManifest(filepath.Join(dir, recoveryManifestFile))
	if err != nil {
		rg.t.Fatal(err)
	}
	return rg.runWith(rg.flags(rg.id.String()), rehearseSetOf(dir, m), rg.targetDSN)
}

func stage(t *testing.T, dir, rel, body string) {
	t.Helper()
	write := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(write), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(write, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// readOnly makes the tree under dir unwritable, as a mounted backup is, and restores it for cleanup.
func readOnly(t *testing.T, dir string) {
	t.Helper()
	chmodTree := func(file, d os.FileMode) {
		_ = filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if e.IsDir() {
				return os.Chmod(p, d)
			}
			return os.Chmod(p, file)
		})
	}
	chmodTree(0o444, 0o555)
	t.Cleanup(func() { chmodTree(0o644, 0o755) })
}

func TestRecoveryFiles_PGRecordingsParseAndATruncatedCastFailsNamingIt(t *testing.T) {
	rg := newRehearseRig(t)
	rg.addCasts(map[string]string{"run-good": goodCast, "run-cut": truncatedCast})
	r := rg.runSet(rg.stagedSet())
	requireExit(t, r.err, exitRehearseFail)
	c, _ := r.check("recordings")
	if c.Result != resultFail || c.Counts["checked"] != 2 || !strings.Contains(strings.Join(c.Failing, "\n"), "run-cut") || strings.Contains(strings.Join(c.Failing, "\n"), "run-good") {
		t.Errorf("recordings = %+v; want 2 checked and only run-cut failing", c)
	}
}

func TestRecoveryFiles_GoodPGAndFSRecordingsPass(t *testing.T) {
	rg := newRehearseRig(t)
	rg.addCasts(map[string]string{"run-good": goodCast})
	r := rg.runSet(rg.stagedSet())
	if r.err != nil {
		t.Fatalf("pg store: %v\n%s", r.err, r.out)
	}
	if c, _ := r.check("recordings"); c.Result != resultPass || c.Counts["checked"] != 1 {
		t.Errorf("pg recordings = %+v", c)
	}

	dir := rg.stagedSetWith("fs", func(dir string) {
		stage(t, dir, "recordings/run-a.cast", goodCast)
		readOnly(t, filepath.Join(dir, "recordings")) // staged from a read-only mount, as the digest records it
	})
	r = rg.runSet(dir)
	if r.err != nil {
		t.Fatalf("fs store: %v\n%s", r.err, r.out)
	}
	if c, _ := r.check("recordings"); c.Result != resultPass || c.Counts["checked"] != 1 {
		t.Errorf("fs recordings = %+v", c)
	}
}

func TestRecoveryFiles_ATruncatedFSCastFailsNamingIt(t *testing.T) {
	rg := newRehearseRig(t)
	dir := rg.stagedSetWith("fs", func(dir string) {
		stage(t, dir, "recordings/run-good.cast", goodCast)
		stage(t, dir, "recordings/run-cut.cast", truncatedCast)
	})
	r := rg.runSet(dir)
	requireExit(t, r.err, exitRehearseFail)
	c, _ := r.check("recordings")
	if got := strings.Join(c.Failing, "\n"); !strings.Contains(got, "run-cut.cast") || strings.Contains(got, "run-good") {
		t.Errorf("failing = %v; want only run-cut.cast", c.Failing)
	}
}

func TestRecoveryFiles_AnFSRecordingChangedAfterTheManifestFailsOnItsDigest(t *testing.T) {
	rg := newRehearseRig(t)
	dir := rg.stagedSetWith("fs", func(dir string) { stage(t, dir, "recordings/run-a.cast", goodCast) })
	stage(t, dir, "recordings/run-a.cast", strings.Replace(goodCast, "hi", "ho", 1)) // still a valid cast
	r := rg.runSet(dir)
	requireExit(t, r.err, exitRehearseFail)
	c, _ := r.check("recordings")
	if !strings.Contains(strings.Join(c.Failing, "\n"), "differs from the manifest") {
		t.Errorf("recordings = %+v; want the digest difference named", c)
	}
}

func TestRecoveryFiles_ADriveOneByteFlipFailsNamingTheDriveAndHome(t *testing.T) {
	rg := newRehearseRig(t)
	d := rg.addDrive()
	home := "d-0123456789abcdef"
	rel := filepath.Join("drives", d.ID.String(), home, "notes.txt")
	dir := rg.stagedSetWith("pg", func(dir string) { stage(t, dir, rel, "hello world") })
	if r := rg.runSet(dir); r.err != nil {
		t.Fatalf("an intact drive: %v\n%s", r.err, r.out)
	}
	stage(t, dir, rel, "hello worle") // one byte, same size, same entry count
	r := rg.runSet(dir)
	requireExit(t, r.err, exitRehearseFail)
	c, _ := r.check("drives")
	if got := strings.Join(c.Failing, "\n"); c.Result != resultFail || !strings.Contains(got, d.ID.String()) || !strings.Contains(got, home) {
		t.Errorf("drives = %+v; want the drive and home named", c)
	}
}

func TestRecoveryFiles_ADriveMissingFromTheSetGivesIncompleteExitFive(t *testing.T) {
	rg := newRehearseRig(t)
	d := rg.addDrive()
	dir := rg.stagedSet()
	r := rg.runSet(dir)
	requireExit(t, r.err, exitRehearseIncomplete)
	c, _ := r.check("drives")
	if c.Result != resultIncomplete || !strings.Contains(c.Detail, d.ID.String()) {
		t.Errorf("drives = %+v; want incomplete naming the drive", c)
	}
	if r.report.Verdict != resultIncomplete {
		t.Errorf("verdict = %q", r.report.Verdict)
	}
}

func TestRecoveryFiles_AStagedDriveHomeRemovedAfterTheManifestFails(t *testing.T) {
	rg := newRehearseRig(t)
	d := rg.addDrive()
	rel := filepath.Join("drives", d.ID.String(), "d-0123456789abcdef", "f")
	dir := rg.stagedSetWith("pg", func(dir string) { stage(t, dir, rel, "x") })
	if err := os.RemoveAll(filepath.Join(dir, "drives")); err != nil {
		t.Fatal(err)
	}
	r := rg.runSet(dir)
	requireExit(t, r.err, exitRehearseFail)
	if c, _ := r.check("drives"); !strings.Contains(strings.Join(c.Failing, "\n"), "not in the set") {
		t.Errorf("drives = %+v", c)
	}
}

func TestRecoveryFiles_AManifestNamingADirectoryOutsideTheSetFails(t *testing.T) {
	rg := newRehearseRig(t)
	dir := rg.stagedSet()
	stage(t, dir, "x/f", "outside the drives tree") // what drives/../x would read
	d, err := recoveryset.TreeDigest(filepath.Join(dir, "x"))
	if err != nil {
		t.Fatal(err)
	}
	rg.editManifest(dir, func(m *recoveryManifest) {
		m.Drives = []recoveryDrive{{DriveID: "..", Home: "x", recoveryTree: recoveryTree{State: treeStaged, Entries: d.Entries, TreeSHA256: d.SHA256}}}
	})
	r := rg.runSet(dir)
	requireExit(t, r.err, exitRehearseFail)
	if c, _ := r.check("drives"); !strings.Contains(strings.Join(c.Failing, "\n"), "no usable directory") {
		t.Errorf("drives = %+v; want the unusable directory named", c)
	}
}

func TestRecoveryFiles_AManifestWithNoRecordingsStoreFails(t *testing.T) {
	rg := newRehearseRig(t)
	dir := rg.stagedSet()
	rg.editManifest(dir, func(m *recoveryManifest) { m.Recordings = recoveryRecordings{} })
	r := rg.runSet(dir)
	requireExit(t, r.err, exitRehearseFail)
}

func TestRecoveryFiles_StagingRefusesWhatIsNotAPlainTree(t *testing.T) {
	rg := newRehearseRig(t)
	for name, fill := range map[string]func(dir string){
		"a file where a drive id belongs": func(dir string) { stage(t, dir, "drives/loose-file", "x") },
		"a directory that is no drive id": func(dir string) { stage(t, dir, "drives/not-a-uuid/home/f", "x") },
		"a symlinked recordings dir": func(dir string) {
			if err := os.Symlink(t.TempDir(), filepath.Join(dir, "recordings")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "audit"), 0o700); err != nil {
			t.Fatal(err)
		}
		fill(dir)
		recStore := "pg"
		if strings.Contains(name, "recordings") {
			recStore = "fs"
		}
		err := createRecoveryManifest(context.Background(), manifestSources{DSN: rg.targetDSN, Set: dir, SpoolBase: "spool.jsonl", KEKMode: "local", RecordingStore: recStore})
		if err == nil {
			t.Errorf("%s: the manifest was written", name)
		}
		if _, statErr := os.Stat(filepath.Join(dir, recoveryManifestFile)); statErr == nil {
			t.Errorf("%s: a manifest exists after the refusal", name)
		}
	}
}
