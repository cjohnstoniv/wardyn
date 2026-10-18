// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/recoveryset"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Recordings and drive bytes of the recovery set (recovery-set.md §2.1, §6). They reach the set as plain
// directories the operator copies in: recordings/ under the fs store, drives/<drive id>/<home>/ for each
// drive object. The manifest records each tree's digest; the rehearsal compares.

const (
	treeStaged   = "staged"
	treeNotInSet = "not_in_set"

	recordingsDir = "recordings"
	drivesDir     = "drives"
)

// recoveryTree is one staged directory's entry: staged with its digest, or not_in_set.
type recoveryTree struct {
	State      string `json:"state"`
	Entries    int    `json:"entries,omitempty"`
	TreeSHA256 string `json:"tree_sha256,omitempty"`
}

// recoveryRecordings is the recordings entry: the store kind, and under fs the staged tree.
type recoveryRecordings struct {
	Store string        `json:"store"`
	Tree  *recoveryTree `json:"tree,omitempty"`
}

// recoveryDrive is one drive object: the drive and home it belongs to, its backend and substrate object
// name, and its staged tree. A drive with no staged home has Home and Object empty and State not_in_set.
// It carries no subject field, but a home can still name the person: a managed home is a hash, while a
// host_path home built from the sub or email_local template is the person's claim.
type recoveryDrive struct {
	DriveID string `json:"drive_id"`
	Home    string `json:"home,omitempty"`
	Backend string `json:"backend,omitempty"`
	Object  string `json:"object,omitempty"`
	recoveryTree
}

// treeOf digests the staged directory rel inside the set; an absent one is not_in_set. The last component
// must be a directory and not a symlink, and every component resolves inside the set root.
func treeOf(root *os.Root, rel string) (recoveryTree, error) {
	info, err := root.Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return recoveryTree{State: treeNotInSet}, nil
	}
	if err != nil {
		return recoveryTree{}, err
	}
	if !info.IsDir() {
		return recoveryTree{}, fmt.Errorf("%s in the set is not a directory", rel)
	}
	d, err := recoveryset.TreeDigestIn(root, rel)
	if err != nil {
		return recoveryTree{}, fmt.Errorf("digest %s: %w", rel, err)
	}
	return recoveryTree{State: treeStaged, Entries: d.Entries, TreeSHA256: d.SHA256}, nil
}

// stageFiles records the recordings and drive entries for the set behind root.
func stageFiles(ctx context.Context, pool *pgxpool.Pool, root *os.Root, recordingStore string) (recoveryRecordings, []recoveryDrive, error) {
	rec := recoveryRecordings{Store: recordingStore}
	if recordingStore != "pg" && recordingStore != "fs" && recordingStore != "off" {
		return rec, nil, fmt.Errorf("WARDYN_RECORDING_STORE is %q; it must be pg, fs or off", recordingStore)
	}
	if recordingStore == "fs" {
		t, err := treeOf(root, recordingsDir)
		if err != nil {
			return rec, nil, err
		}
		rec.Tree = &t
	}
	drives, err := store.PG{Pool: pool}.ListUserDrives(ctx)
	if err != nil {
		return rec, nil, fmt.Errorf("list drives: %w", err)
	}
	out, err := stageDrives(root, drives)
	return rec, out, err
}

// stageDrives digests every drives/<drive id>/<home> directory in the set. A drive of the database with
// none is recorded not_in_set, so the rehearsal cannot pass without saying so.
func stageDrives(root *os.Root, drives []types.UserDriveListItem) ([]recoveryDrive, error) {
	byID := map[string]types.UserDrive{}
	for _, d := range drives {
		byID[d.ID.String()] = d.UserDrive
	}
	objects, err := stagedObjects(root)
	if err != nil {
		return nil, err
	}
	var out []recoveryDrive
	staged := map[string]bool{}
	for _, o := range objects {
		id, home := o[0], o[1]
		t, err := treeOf(root, path.Join(drivesDir, id, home))
		if err != nil {
			return nil, err
		}
		e := recoveryDrive{DriveID: id, Home: home, recoveryTree: t}
		if d, ok := byID[id]; ok {
			e.Backend, e.Object = string(d.Backend), types.DriveObjectName(d, home)
		}
		out = append(out, e)
		staged[id] = true
	}
	for _, d := range drives {
		if !staged[d.ID.String()] {
			out = append(out, recoveryDrive{DriveID: d.ID.String(), Backend: string(d.Backend), recoveryTree: recoveryTree{State: treeNotInSet}})
		}
	}
	return out, nil
}

// stagedObjects lists every drives/<drive id>/<home> directory in the set as {id, home}, sorted. A drive
// directory that is not named by a uuid, or anything on the way that is not a plain directory, is an error.
func stagedObjects(root *os.Root) ([][2]string, error) {
	ids, err := subdirs(root, drivesDir)
	if err != nil {
		return nil, err
	}
	var out [][2]string
	for _, id := range ids {
		if _, err := uuid.Parse(id); err != nil {
			return nil, fmt.Errorf("%s/%s in the set is not a drive id", drivesDir, id)
		}
		homes, err := subdirs(root, path.Join(drivesDir, id))
		if err != nil {
			return nil, err
		}
		for _, home := range homes {
			out = append(out, [2]string{id, home})
		}
	}
	return out, nil
}

// subdirs lists the directories directly under rel in the set, sorted. A missing rel has none. Anything
// that is not a directory (a file, or a symlink, which is never followed) is refused by name.
func subdirs(root *os.Root, rel string) ([]string, error) {
	entries, err := fs.ReadDir(root.FS(), rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			return nil, fmt.Errorf("%s/%s in the set is not a directory", rel, e.Name())
		}
		out = append(out, e.Name())
	}
	return out, nil
}

// fileChecks are the rehearsal's recordings and drives checks.
func (s *rehearseSession) fileChecks(ctx context.Context, set rehearseSet, root *os.Root) ([]rehearseCheck, error) {
	rec, err := s.recordingsCheck(ctx, set, root)
	if err != nil {
		return nil, err
	}
	return []rehearseCheck{rec, drivesCheck(set, root)}, nil
}

func (s *rehearseSession) recordingsCheck(ctx context.Context, set rehearseSet, root *os.Root) (rehearseCheck, error) {
	c := rehearseCheck{Name: "recordings", Result: resultPass}
	switch set.Recordings.Store {
	case "off":
		c.Detail = "recordings are off in this deployment"
	case "pg":
		return s.pgRecordings(ctx)
	case "fs":
		return fsRecordings(ctx, set, root), nil
	default:
		c.Result, c.Detail = resultFail, "the manifest records no recordings store; take the manifest again with this release"
	}
	return c, nil
}

// pgRecordings opens every stored cast over the one target backend and parses it through.
func (s *rehearseSession) pgRecordings(ctx context.Context) (rehearseCheck, error) {
	c := rehearseCheck{Name: "recordings", Result: resultPass, Counts: map[string]int{"checked": 0}}
	tx, err := s.readTx(ctx, pgx.RepeatableRead)
	if err != nil {
		return c, fmt.Errorf("begin the recordings check: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT cast_key, payload FROM recordings ORDER BY cast_key`)
	if err != nil {
		return c, fmt.Errorf("read the recordings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var payload []byte
		if err := rows.Scan(&key, &payload); err != nil {
			return c, fmt.Errorf("read the recordings: %w", err)
		}
		c.Counts["checked"]++
		if err := checkCast(ctx, bytes.NewReader(payload)); err != nil {
			c.Failing = append(c.Failing, "cast "+key+": "+castProblem(err))
		}
	}
	if err := rows.Err(); err != nil {
		return c, fmt.Errorf("read the recordings: %w", err)
	}
	return finish(c), nil
}

// checkCast parses one stored recording. A body that does not begin like an asciicast is a plain log: an
// image without asciinema stores its log as the cast (the upload path tells the two apart the same way,
// by this prefix), and it has no structure to check.
func checkCast(ctx context.Context, r io.Reader) error {
	br := bufio.NewReaderSize(r, 4096)
	head, err := br.Peek(len(castHeadPrefix))
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return err
	}
	if !bytes.HasPrefix(head, []byte(castHeadPrefix)) {
		return nil
	}
	return recording.CopyOutput(ctx, io.Discard, br)
}

// castHeadPrefix opens every asciicast v2 header (the same test internal/api's upload path applies).
const castHeadPrefix = `{"version"`

// castProblem is a cast's parse error without any of its content: the recording package never puts
// sandbox-controlled bytes in ErrInvalidCast, and anything else is reduced to the sentinel.
func castProblem(err error) string {
	if errors.Is(err, recording.ErrInvalidCast) {
		return err.Error()
	}
	return "could not be read"
}

func finish(c rehearseCheck) rehearseCheck {
	if len(c.Failing) > 0 {
		c.Result = resultFail
	}
	return c
}

// fsRecordings compares the staged recordings tree with the manifest's, then parses every cast in it.
// A tree that differs from the manifest is not parsed: it is already a failure, and nothing in it is
// opened.
func fsRecordings(ctx context.Context, set rehearseSet, root *os.Root) rehearseCheck {
	c := rehearseCheck{Name: "recordings", Result: resultPass, Counts: map[string]int{"checked": 0}}
	want := set.Recordings.Tree
	if want == nil {
		c.Result, c.Detail = resultFail, "the manifest records the fs store with no recordings tree; take the manifest again"
		return c
	}
	if want.State == treeNotInSet {
		c.Result, c.Detail = resultIncomplete, "recordings/ was not in the set when the manifest was taken"
		return c
	}
	if f := compareTree(recordingsDir, root, recordingsDir, *want); f != "" {
		c.Failing = append(c.Failing, f)
		return finish(c)
	}
	casts, err := parseCasts(ctx, root)
	c.Counts["checked"] = casts.checked
	c.Failing = append(c.Failing, casts.failing...)
	if err != nil {
		c.Failing = append(c.Failing, "recordings/ could not be read")
	}
	return finish(c)
}

type castRun struct {
	checked int
	failing []string
}

// parseCasts parses every .cast file under recordings/ in the set. It opens only regular files, without
// blocking: anything else named .cast is a failure by name and is never opened.
func parseCasts(ctx context.Context, root *os.Root) (castRun, error) {
	var run castRun
	sub, err := root.OpenRoot(recordingsDir)
	if err != nil {
		return run, err
	}
	defer sub.Close()
	err = fs.WalkDir(sub.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// A dot-directory is the store's own bookkeeping: .erased/ holds the
		// empty fence markers that keep an erasure durable, not recordings.
		if d.IsDir() {
			if p != "." && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".cast") {
			return nil
		}
		run.checked++
		if !d.Type().IsRegular() {
			run.failing = append(run.failing, "cast "+p+": is not a regular file")
			return nil
		}
		f, err := sub.OpenFile(p, os.O_RDONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			run.failing = append(run.failing, "cast "+p+": could not be opened")
			return nil
		}
		defer f.Close()
		if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
			run.failing = append(run.failing, "cast "+p+": is not a regular file")
			return nil
		}
		if err := checkCast(ctx, f); err != nil {
			run.failing = append(run.failing, "cast "+p+": "+castProblem(err))
		}
		return nil
	})
	return run, err
}

// compareTree digests rel in the set and returns what differs from want, "" when nothing does. It names
// the tree, never an entry's content.
func compareTree(name string, root *os.Root, rel string, want recoveryTree) string {
	got, err := treeOf(root, rel)
	switch {
	case err != nil:
		return name + " could not be digested"
	case got.State != treeStaged:
		return name + " is in the manifest but not in the set"
	case got.TreeSHA256 != want.TreeSHA256:
		return fmt.Sprintf("%s differs from the manifest (%d entries, manifest %d)", name, got.Entries, want.Entries)
	}
	return ""
}

// drivesCheck compares each staged drive object's tree with the manifest's, and fails on any drive object
// in the set that the manifest has no entry for. An object that was not in the set when the manifest was
// taken makes the check incomplete, never a pass.
func drivesCheck(set rehearseSet, root *os.Root) rehearseCheck {
	c := rehearseCheck{Name: "drives", Result: resultPass, Counts: map[string]int{"staged": 0, "not_in_set": 0}}
	var missing []string
	known := map[[2]string]bool{}
	for _, d := range set.Drives {
		if d.State == treeNotInSet {
			c.Counts["not_in_set"]++
			missing = append(missing, "drive "+d.DriveID)
			continue
		}
		c.Counts["staged"]++
		known[[2]string{d.DriveID, d.Home}] = true
		ref := "drive " + d.DriveID + " home " + d.Home
		if !validSegment(d.DriveID) || !validSegment(d.Home) {
			c.Failing = append(c.Failing, ref+": the manifest names no usable directory")
			continue
		}
		if f := compareTree(ref, root, path.Join(drivesDir, d.DriveID, d.Home), d.recoveryTree); f != "" {
			c.Failing = append(c.Failing, f)
		}
	}
	c.Failing = append(c.Failing, unlistedObjects(root, known)...)
	slices.Sort(missing)
	switch {
	case len(c.Failing) > 0:
		c.Result = resultFail
	case len(missing) > 0:
		c.Result, c.Detail = resultIncomplete, "not in the set: "+strings.Join(missing, ", ")
	}
	return c
}

// unlistedObjects names every drives/<drive id>/<home> directory in the set that the manifest has no entry
// for: bytes that were added, or moved in, after the manifest was taken.
func unlistedObjects(root *os.Root, known map[[2]string]bool) []string {
	objects, err := stagedObjects(root)
	var out []string
	for _, o := range objects {
		if !known[o] {
			out = append(out, "drive "+o[0]+" home "+o[1]+": in the set but not in the manifest")
		}
	}
	if err != nil {
		out = append(out, drivesDir+"/ could not be listed: "+err.Error())
	}
	return out
}

// validSegment is true for one plain directory name, which is all the manifest may name.
func validSegment(s string) bool {
	return s != "" && s != "." && s != ".." && filepath.Base(s) == s && !strings.ContainsAny(s, `/\`)
}
