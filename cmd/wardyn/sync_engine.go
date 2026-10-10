// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"time"

	"github.com/pkg/sftp"
)

const (
	// maxSyncEntries bounds one side's listing: the sandbox chooses what is in it.
	maxSyncEntries = 200_000
	// maxPullFile is the largest single file a pull accepts.
	maxPullFile = 256 << 20
	// defaultMaxPullBytes is --max-pull-bytes: what one pass may pull in total.
	defaultMaxPullBytes = 1 << 30
)

// syncReport is one pass's outcome. Every list is non-nil so --json prints [].
type syncReport struct {
	Pushed    []string      `json:"pushed"`
	Pulled    []string      `json:"pulled"`
	Conflicts []string      `json:"conflicts"`
	Refused   []syncRefusal `json:"refused"`
	BytesOut  int64         `json:"bytes_to_sandbox"`
	BytesIn   int64         `json:"bytes_from_sandbox"`
	seenSet   map[string]bool
}

type syncRefusal struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

func newSyncReport() *syncReport {
	return &syncReport{Pushed: []string{}, Pulled: []string{}, Conflicts: []string{}, Refused: []syncRefusal{}, seenSet: map[string]bool{}}
}

func (r *syncReport) refuse(rel, reason string) {
	if k := rel + "\x00" + reason; !r.seenSet[k] {
		r.seenSet[k] = true
		r.Refused = append(r.Refused, syncRefusal{rel, reason})
	}
}

func (r *syncReport) empty() bool {
	return len(r.Pushed)+len(r.Pulled)+len(r.Conflicts)+len(r.Refused) == 0
}

type syncOptions struct {
	remote       string // absolute, validated sandbox directory
	pull         bool
	maxPullBytes int64
}

// syncer holds one sync's connections. Every read and write on the laptop goes
// through root, so a link swapped in under the local directory cannot lead one
// outside it.
type syncer struct {
	c         *sftp.Client
	root      *os.Root
	o         syncOptions
	st        *syncState
	stateFile string
}

type syncFile struct {
	side syncSide
	mode fs.FileMode
}

func localSide(fi fs.FileInfo) syncSide { return syncSide{fi.Size(), fi.ModTime().UnixNano()} }

// remoteSide has second resolution: that is all the sftp attributes carry.
// ponytail: a sandbox edit of the same size inside the recorded second, or with
// its mtime set back, is not seen; the laptop's copy stays safe. A content hash
// would close it at the price of reading every file on each pass.
func remoteSide(fi fs.FileInfo) syncSide {
	return syncSide{fi.Size(), fi.ModTime().Unix() * int64(time.Second)}
}

// syncAbort reports whether err ends the whole pass: the connection is gone or
// the user interrupted. A failure on one path (a permission, a type clash, a
// name too long) is a refusal of that path instead, so the sandbox cannot wedge
// every later pass with one bad entry.
func syncAbort(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	var se *sftp.StatusError
	var pe *fs.PathError
	var le *os.LinkError
	switch {
	case errors.As(err, &se), errors.As(err, &pe), errors.As(err, &le):
		return false
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission), errors.Is(err, fs.ErrExist):
		return false
	}
	return true
}

// pathFailed turns a failure on one path into a refusal, or returns it when it
// ends the pass.
func (sy *syncer) pathFailed(ctx context.Context, rep *syncReport, rel, what string, err error) error {
	if syncAbort(ctx, err) {
		return fmt.Errorf("%s %s: %w", what, rel, err)
	}
	rep.refuse(rel, what+": "+err.Error())
	return nil
}

// remoteView is what the sandbox side looked like at the start of a pass.
type remoteView struct {
	files   map[string]os.FileInfo
	dirs    []string
	blocked map[string]bool // refused entries (symlinks and the like): nothing is written there either
}

// pass syncs once: it lists both sides, then pushes, and under --pull pulls.
// Nothing is ever deleted on either side.
func (sy *syncer) pass(ctx context.Context) (*syncReport, error) {
	rep := newSyncReport()
	local, localDirs, err := sy.walkLocal(rep)
	if err != nil {
		return nil, err
	}
	remote, err := sy.walkRemote(ctx, rep)
	if err != nil {
		return nil, err
	}
	all := map[string]bool{}
	for rel := range local {
		all[rel] = true
	}
	for rel := range remote.files {
		all[rel] = true
	}
	rels := make([]string, 0, len(all))
	for rel := range all {
		rels = append(rels, rel)
	}
	slices.Sort(rels)
	everything := slices.Concat(rels, localDirs, remote.dirs)
	collide := syncCollisions(everything)

	pulled := int64(0)
	for _, rel := range rels {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch {
		case syncCollides(collide, rel):
			rep.refuse(rel, "case collision")
		case remote.blocked[rel]:
		default:
			if err := sy.decide(ctx, rel, local, remote.files, rep, &pulled); err != nil {
				return nil, err
			}
		}
	}
	if err := sy.st.save(sy.stateFile); err != nil {
		return nil, fmt.Errorf("save sync state: %w", err)
	}
	return rep, nil
}

func syncCollides(collide map[string]bool, rel string) bool {
	for p := rel; p != "." && p != ""; p = path.Dir(p) {
		if collide[p] {
			return true
		}
	}
	return false
}

// decide applies the direction and conflict rules to one path. The laptop
// wins: a file changed on both sides is pushed, never pulled, and reported.
func (sy *syncer) decide(ctx context.Context, rel string, local map[string]syncFile, remote map[string]os.FileInfo, rep *syncReport, pulled *int64) error {
	l, hasL := local[rel]
	r, hasR := remote[rel]
	seen, known := sy.st.Files[rel]
	var rSide syncSide
	if hasR {
		rSide = remoteSide(r)
	}
	lChanged := hasL && (!known || l.side != seen.Local)
	rChanged := hasR && (!known || rSide != seen.Remote)
	switch {
	case hasL && hasR && lChanged && rChanged && (known || sy.o.pull):
		rep.Conflicts = append(rep.Conflicts, rel)
		return sy.push(ctx, rel, l, rep)
	case hasL && lChanged:
		return sy.push(ctx, rel, l, rep)
	case hasL && !lChanged && rChanged && sy.o.pull:
		return sy.pull(ctx, rel, r, &l, rep, pulled)
	case !hasL && hasR && sy.o.pull && rChanged:
		if known {
			rep.Conflicts = append(rep.Conflicts, rel)
			return nil
		}
		return sy.pull(ctx, rel, r, nil, rep, pulled)
	}
	return nil
}

func (sy *syncer) walkLocal(rep *syncReport) (files map[string]syncFile, dirs []string, err error) {
	files = map[string]syncFile{}
	count := 0
	err = fs.WalkDir(sy.root.FS(), ".", func(rel string, d fs.DirEntry, werr error) error {
		if werr != nil {
			if rel == "." {
				return werr
			}
			rep.refuse(rel, "unreadable: "+werr.Error())
			return nil
		}
		if rel == "." {
			return nil
		}
		if count++; count > maxSyncEntries {
			return fmt.Errorf("the local directory holds more than %d entries", maxSyncEntries)
		}
		switch {
		case syncDenied(rel):
			rep.refuse(rel, "denylist")
			return skipIf(d.IsDir())
		case syncInvisible(d.Name()):
			rep.refuse(rel, "invisible character in the name")
			return skipIf(d.IsDir())
		case d.Type()&fs.ModeSymlink != 0:
			rep.refuse(rel, "symlink")
		case d.IsDir():
			dirs = append(dirs, rel)
		case d.Type().IsRegular():
			fi, ierr := d.Info()
			if ierr != nil {
				rep.refuse(rel, "unreadable: "+ierr.Error())
				return nil
			}
			files[rel] = syncFile{localSide(fi), fi.Mode()}
		default:
			rep.refuse(rel, "not a regular file")
		}
		return nil
	})
	return files, dirs, err
}

func skipIf(dir bool) error {
	if dir {
		return fs.SkipDir
	}
	return nil
}

func (sy *syncer) walkRemote(ctx context.Context, rep *syncReport) (*remoteView, error) {
	v := &remoteView{files: map[string]os.FileInfo{}, blocked: map[string]bool{}}
	count := 0
	var walk func(abs, rel string) error
	walk = func(abs, rel string) error {
		entries, err := sy.c.ReadDirContext(ctx, abs)
		if err != nil {
			if rel != "" && !syncAbort(ctx, err) {
				rep.refuse(rel, "unreadable: "+err.Error())
				return nil
			}
			return fmt.Errorf("list %s in the sandbox: %w", abs, err)
		}
		for _, fi := range entries {
			if count++; count > maxSyncEntries {
				return fmt.Errorf("the sandbox directory holds more than %d entries", maxSyncEntries)
			}
			name := fi.Name()
			if err := syncEntryName(name); err != nil {
				rep.refuse(path.Join(rel, "?"), err.Error())
				continue
			}
			childRel := path.Join(rel, name)
			switch {
			case syncDenied(childRel):
				rep.refuse(childRel, "denylist")
			case fi.Mode()&fs.ModeSymlink != 0:
				rep.refuse(childRel, "symlink")
				v.blocked[childRel] = true
			case fi.IsDir():
				v.dirs = append(v.dirs, childRel)
				if err := walk(path.Join(abs, name), childRel); err != nil {
					return err
				}
			case fi.Mode().IsRegular():
				v.files[childRel] = fi
			default:
				rep.refuse(childRel, "not a regular file")
				v.blocked[childRel] = true
			}
		}
		return nil
	}
	if err := walk(sy.o.remote, ""); err != nil {
		return nil, err
	}
	return v, nil
}

func (sy *syncer) push(ctx context.Context, rel string, l syncFile, rep *syncReport) error {
	f, err := sy.root.Open(rel)
	if err != nil {
		rep.refuse(rel, "unreadable: "+err.Error())
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		rep.refuse(rel, "changed while syncing")
		return nil
	}
	if lstat, err := sy.root.Lstat(rel); err != nil || !os.SameFile(lstat, fi) {
		rep.refuse(rel, "symlink")
		return nil
	}
	dst := path.Join(sy.o.remote, rel)
	if err := sy.c.MkdirAll(path.Dir(dst)); err != nil {
		return sy.pathFailed(ctx, rep, rel, "create the directory in the sandbox", err)
	}
	out, err := sy.c.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return sy.pathFailed(ctx, rep, rel, "write in the sandbox", err)
	}
	n, err := io.Copy(out, io.LimitReader(f, l.side.Size))
	rep.BytesOut += n
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return sy.pathFailed(ctx, rep, rel, "write in the sandbox", err)
	}
	mt := time.Unix(0, l.side.MTime)
	_ = sy.c.Chmod(dst, l.mode.Perm())
	_ = sy.c.Chtimes(dst, mt, mt)
	rfi, err := sy.c.Stat(dst)
	if err != nil {
		return sy.pathFailed(ctx, rep, rel, "stat in the sandbox", err)
	}
	sy.st.Files[rel] = syncFileSeen{Local: l.side, Remote: remoteSide(rfi)}
	rep.Pushed = append(rep.Pushed, rel)
	return nil
}

// pull writes a sandbox file onto the laptop: never through a symlink, never
// over anything but the regular file the walk listed, and never with an exec
// bit. The bytes counted against the budget are the bytes read, not the size
// the sandbox listed.
func (sy *syncer) pull(ctx context.Context, rel string, r os.FileInfo, walked *syncFile, rep *syncReport, pulled *int64) error {
	if err := syncCheckLocal(sy.root, rel); err != nil {
		rep.refuse(rel, err.Error())
		return nil
	}
	if fi, err := sy.root.Lstat(rel); err == nil {
		switch {
		case walked == nil:
			rep.refuse(rel, "a local entry by another spelling is in the way")
			return nil
		case !fi.Mode().IsRegular():
			rep.refuse(rel, "a local entry that is not a regular file is in the way")
			return nil
		}
	}
	allowed := min(int64(maxPullFile), sy.o.maxPullBytes-*pulled)
	switch {
	case r.Size() > maxPullFile:
		rep.refuse(rel, fmt.Sprintf("too large (over %d bytes)", maxPullFile))
		return nil
	case r.Size() > allowed:
		rep.refuse(rel, "over --max-pull-bytes for this pass")
		return nil
	}
	mode := fs.FileMode(0o644)
	if walked != nil {
		mode = walked.mode.Perm() &^ 0o111
	}
	if err := sy.root.MkdirAll(path.Dir(rel), 0o755); err != nil {
		return sy.pathFailed(ctx, rep, rel, "create the local directory", err)
	}
	src, err := sy.c.Open(path.Join(sy.o.remote, rel))
	if err != nil {
		return sy.pathFailed(ctx, rep, rel, "read in the sandbox", err)
	}
	defer src.Close()
	tmpName, tmp, err := sy.localTemp(rel)
	if err != nil {
		return sy.pathFailed(ctx, rep, rel, "create a local temp file", err)
	}
	defer sy.root.Remove(tmpName)
	n, err := io.Copy(tmp, io.LimitReader(src, allowed+1))
	*pulled += n
	rep.BytesIn += n
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return sy.pathFailed(ctx, rep, rel, "read in the sandbox", err)
	}
	if n > allowed {
		rep.refuse(rel, "larger than listed and over the pull limit")
		return nil
	}
	if err := sy.root.Chmod(tmpName, mode); err != nil {
		return sy.pathFailed(ctx, rep, rel, "set the local mode", err)
	}
	mt := r.ModTime()
	_ = sy.root.Chtimes(tmpName, mt, mt)
	if !sy.unchangedLocally(rel, walked) {
		rep.Conflicts = append(rep.Conflicts, rel)
		return nil
	}
	if err := sy.root.Rename(tmpName, rel); err != nil {
		return sy.pathFailed(ctx, rep, rel, "replace the local file", err)
	}
	fi, err := sy.root.Lstat(rel)
	if err != nil {
		return sy.pathFailed(ctx, rep, rel, "stat the local file", err)
	}
	sy.st.Files[rel] = syncFileSeen{Local: localSide(fi), Remote: remoteSide(r)}
	rep.Pulled = append(rep.Pulled, rel)
	return nil
}

// unchangedLocally re-checks the target just before it is replaced: it must
// still be the file the walk saw, or still absent. A developer's save made
// after the walk wins.
func (sy *syncer) unchangedLocally(rel string, walked *syncFile) bool {
	fi, err := sy.root.Lstat(rel)
	if walked == nil {
		return errors.Is(err, fs.ErrNotExist)
	}
	return err == nil && fi.Mode().IsRegular() && localSide(fi) == walked.side
}

// localTemp creates an exclusive 0600 temp file beside rel.
func (sy *syncer) localTemp(rel string) (string, *os.File, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, err
	}
	name := path.Join(path.Dir(rel), ".wardyn-sync-"+hex.EncodeToString(b[:]))
	f, err := sy.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	return name, f, err
}
