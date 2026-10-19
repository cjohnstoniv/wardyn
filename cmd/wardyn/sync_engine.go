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
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/pkg/sftp"
)

const (
	// maxSyncEntries bounds one side's listing: the sandbox chooses what is in it.
	maxSyncEntries = 200_000
	// maxSyncListBytes bounds the sftp bytes one remote directory listing may
	// read. The sftp client holds a whole directory in memory before the entry
	// cap above can apply, so the budget is what bounds that memory.
	maxSyncListBytes = 64 << 20
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
	// DeletedRemote and Renamed are what this pass removed or renamed in the
	// sandbox to follow the laptop; DeletedLocal is what --pull-deletes removed
	// on the laptop (each kept under KeptIn).
	DeletedRemote []string     `json:"deleted_in_sandbox"`
	Renamed       []syncRename `json:"renamed_in_sandbox"`
	DeletedLocal  []string     `json:"deleted_on_laptop"`
	KeptIn        string       `json:"deleted_kept_in,omitempty"`
	// Ignored counts the entries the ignore files left out, on both sides.
	Ignored  int   `json:"ignored"`
	BytesOut int64 `json:"bytes_to_sandbox"`
	BytesIn  int64 `json:"bytes_from_sandbox"`
	seenSet  map[string]bool
}

type syncRename struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type syncRefusal struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

func newSyncReport() *syncReport {
	return &syncReport{Pushed: []string{}, Pulled: []string{}, Conflicts: []string{}, Refused: []syncRefusal{},
		DeletedRemote: []string{}, Renamed: []syncRename{}, DeletedLocal: []string{}, seenSet: map[string]bool{}}
}

func (r *syncReport) refuse(rel, reason string) {
	if k := rel + "\x00" + reason; !r.seenSet[k] {
		r.seenSet[k] = true
		r.Refused = append(r.Refused, syncRefusal{rel, reason})
	}
}

func (r *syncReport) empty() bool {
	return len(r.Pushed)+len(r.Pulled)+len(r.Conflicts)+len(r.Refused)+len(r.DeletedRemote)+len(r.Renamed)+len(r.DeletedLocal) == 0
}

type syncOptions struct {
	remote       string // absolute, validated sandbox directory
	pull         bool
	maxPullBytes int64
	// pullDeletes (--pull-deletes, needs pull) lets a sandbox delete remove the
	// laptop's copy, at most maxPullDeletes per pass, moving each into trash.
	pullDeletes    bool
	maxPullDeletes int
	trash          string
	// orgDeny is the organisation's wall (GET /runs/{id} sync.deny): ignore
	// patterns no file in the tree can take back.
	orgDeny []string
	// notify gets the list of laptop deletes before they are applied; nil drops it.
	notify io.Writer
}

// syncer holds one sync's connections. Every read and write on the laptop goes
// through root, so a link swapped in under the local directory cannot lead one
// outside it.
type syncer struct {
	c         *sftp.Client
	meter     *listMeter // the byte count on c's input; nil leaves a listing unbounded
	root      *os.Root
	o         syncOptions
	st        *syncState
	stateFile string
	ig        *syncIgnore // this pass's ignore rules, read by walkLocal
}

// listMeter counts the bytes the sftp client reads while a directory listing
// is armed, and cancels the listing once it passes maxSyncListBytes.
type listMeter struct {
	r      io.Reader
	mu     sync.Mutex
	n      int64
	over   bool
	cancel context.CancelFunc
}

func (m *listMeter) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	m.mu.Lock()
	if m.cancel != nil {
		if m.n += int64(n); m.n > maxSyncListBytes && !m.over {
			m.over = true
			m.cancel()
		}
	}
	m.mu.Unlock()
	return n, err
}

// arm starts counting for one listing; cancel ends that listing.
func (m *listMeter) arm(cancel context.CancelFunc) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.n, m.over, m.cancel = 0, false, cancel
	m.mu.Unlock()
}

// disarm stops counting and reports whether the budget was passed.
func (m *listMeter) disarm() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cancel = nil
	return m.over
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
	case errors.As(err, &se):
		return se.FxCode() == sftp.ErrSSHFxNoConnection || se.FxCode() == sftp.ErrSSHFxConnectionLost
	case errors.As(err, &pe), errors.As(err, &le):
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
	treeSeen
}

// treeSeen is what one side's walk saw: every entry it met, refused or left
// out included, and the directories it listed in full ("." is the root).
type treeSeen struct {
	seen   map[string]bool
	listed map[string]bool
}

func newTreeSeen() treeSeen {
	return treeSeen{seen: map[string]bool{}, listed: map[string]bool{".": true}}
}

// gone reports that rel is not on this side: neither it nor any directory
// between it and the nearest listed directory was met. An entry that was met
// but not listed (a directory left out, unreadable or a symlink) keeps what is
// under it, so nothing there is ever read as deleted.
func (t treeSeen) gone(rel string) bool {
	for p := rel; p != "."; p = path.Dir(p) {
		if t.seen[p] {
			return false
		}
		if t.listed[path.Dir(p)] {
			return true
		}
	}
	return true
}

// passView is both sides as one pass found them.
type passView struct {
	local      map[string]syncFile
	localTree  treeSeen
	remote     *remoteView
	localGoing []string // --pull-deletes candidates, applied after the other paths
	// localEmpty is a laptop directory with nothing in it, which deletes
	// nothing: an unmounted drive or a checkout in progress looks the same.
	localEmpty bool
}

// pass syncs once: it lists both sides, then pushes, and under --pull pulls.
// Nothing is ever deleted on either side.
func (sy *syncer) pass(ctx context.Context) (*syncReport, error) {
	rep := newSyncReport()
	sy.ig = newSyncIgnore(sy.o.orgDeny)
	local, localDirs, localTree, err := sy.walkLocal(rep)
	if err != nil {
		return nil, err
	}
	remote, err := sy.walkRemote(ctx, rep)
	if err != nil {
		return nil, err
	}
	v := &passView{local: local, localTree: localTree, remote: remote, localEmpty: len(localTree.seen) == 0}
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
	eligible := func(rel string) bool {
		return !syncCollides(collide, rel) && !syncCollides(remote.blocked, rel)
	}
	renamed, err := sy.renames(ctx, rels, v, eligible, rep)
	if err != nil {
		return nil, err
	}

	pulled := int64(0)
	for _, rel := range rels {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch {
		case renamed[rel]:
		case syncCollides(collide, rel):
			rep.refuse(rel, "case collision")
		case syncCollides(remote.blocked, rel):
		default:
			if err := sy.decide(ctx, rel, v, rep, &pulled); err != nil {
				return nil, err
			}
		}
	}
	if err := sy.deleteLocal(ctx, v, rep); err != nil {
		return nil, err
	}
	// A path both sides have lost needs no record.
	for rel := range sy.st.Files {
		if v.localTree.gone(rel) && remote.gone(rel) {
			delete(sy.st.Files, rel)
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
// wins: a file changed on both sides is pushed, never pulled, and reported. A
// file the laptop deleted is deleted in the sandbox only while the sandbox's
// copy is still the one this sync put there; a sandbox edit keeps it, listed
// as a conflict.
func (sy *syncer) decide(ctx context.Context, rel string, v *passView, rep *syncReport, pulled *int64) error {
	l, hasL := v.local[rel]
	r, hasR := v.remote.files[rel]
	seen, known := sy.st.Files[rel]
	var rSide syncSide
	if hasR {
		rSide = remoteSide(r)
	}
	lChanged := hasL && (!known || l.side != seen.Local)
	rChanged := hasR && (!known || rSide != seen.Remote)
	if known {
		if done, err := sy.deleted(ctx, rel, v, rep, hasL, hasR, lChanged, rChanged); done {
			return err
		}
	}
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

// deleted handles a file this sync sent that one side has since deleted. A
// laptop delete removes the sandbox copy unless the sandbox changed it; a
// sandbox delete of a file the laptop left alone is queued for deleteLocal,
// under --pull-deletes only. It reports whether rel was handled.
func (sy *syncer) deleted(ctx context.Context, rel string, v *passView, rep *syncReport, hasL, hasR, lChanged, rChanged bool) (bool, error) {
	switch {
	case !hasL && hasR && v.laptopDeleted(rel):
		if rChanged {
			rep.Conflicts = append(rep.Conflicts, rel)
			return true, nil
		}
		return true, sy.removeRemote(ctx, rel, v, rep)
	case hasL && !hasR && !lChanged && sy.o.pullDeletes && len(v.remote.seen) > 0 && v.remote.gone(rel):
		if syncIsIgnoreFile(rel) {
			rep.refuse(rel, "ignore file")
			return true, nil
		}
		v.localGoing = append(v.localGoing, rel)
		return true, nil
	}
	return false, nil
}

// renames finds the files the laptop renamed since the last pass: a path this
// sync put in the sandbox, gone from the laptop with the sandbox's copy
// unchanged, paired with a new laptop path the sandbox lacks that has the same
// size and modification time, when no other file shares them. Each pair is
// renamed in the sandbox rather than deleted and sent again. It returns the
// paths it handled.
func (sy *syncer) renames(ctx context.Context, rels []string, v *passView, eligible func(string) bool, rep *syncReport) (map[string]bool, error) {
	gone, fresh := map[syncSide][]string{}, map[syncSide][]string{}
	for _, rel := range rels {
		if !eligible(rel) {
			continue
		}
		l, hasL := v.local[rel]
		r, hasR := v.remote.files[rel]
		seen, known := sy.st.Files[rel]
		switch {
		case !hasL && hasR && known && v.laptopDeleted(rel) && remoteSide(r) == seen.Remote:
			gone[seen.Local] = append(gone[seen.Local], rel)
		case hasL && !hasR && !known && v.remote.gone(rel):
			fresh[l.side] = append(fresh[l.side], rel)
		}
	}
	done := map[string]bool{}
	for side, from := range gone {
		to := fresh[side]
		if len(from) != 1 || len(to) != 1 {
			continue
		}
		if err := sy.renameRemote(ctx, from[0], to[0], v, rep); err != nil {
			return nil, err
		}
		done[from[0]], done[to[0]] = true, true
	}
	return done, nil
}

func (sy *syncer) renameRemote(ctx context.Context, from, to string, v *passView, rep *syncReport) error {
	dst := path.Join(sy.o.remote, to)
	if err := sy.c.MkdirAll(path.Dir(dst)); err != nil {
		return sy.pathFailed(ctx, rep, to, "create the directory in the sandbox", err)
	}
	if err := sy.c.Rename(path.Join(sy.o.remote, from), dst); err != nil {
		return sy.pathFailed(ctx, rep, to, "rename in the sandbox", err)
	}
	rfi, err := sy.c.Stat(dst)
	if err != nil {
		return sy.pathFailed(ctx, rep, to, "stat in the sandbox", err)
	}
	delete(sy.st.Files, from)
	sy.st.Files[to] = syncFileSeen{Local: v.local[to].side, Remote: remoteSide(rfi)}
	rep.Renamed = append(rep.Renamed, syncRename{From: from, To: to})
	sy.pruneRemoteDirs(from, v)
	return nil
}

func (sy *syncer) removeRemote(ctx context.Context, rel string, v *passView, rep *syncReport) error {
	p := path.Join(sy.o.remote, rel)
	// The agent may have written the file since the walk: only the copy the
	// walk saw is deleted, and a changed one is kept as a conflict.
	fi, err := sy.c.Lstat(p)
	if err != nil {
		return sy.pathFailed(ctx, rep, rel, "stat in the sandbox", err)
	}
	if !fi.Mode().IsRegular() || remoteSide(fi) != remoteSide(v.remote.files[rel]) {
		rep.Conflicts = append(rep.Conflicts, rel)
		return nil
	}
	if err := sy.c.Remove(p); err != nil {
		return sy.pathFailed(ctx, rep, rel, "delete in the sandbox", err)
	}
	delete(sy.st.Files, rel)
	rep.DeletedRemote = append(rep.DeletedRemote, rel)
	sy.pruneRemoteDirs(rel, v)
	return nil
}

// pruneRemoteDirs removes the sandbox directories above rel that the laptop no
// longer has, while they are empty: a directory with anything left in it stays.
func (sy *syncer) pruneRemoteDirs(rel string, v *passView) {
	for dir := path.Dir(rel); dir != "." && v.localTree.gone(dir); dir = path.Dir(dir) {
		if sy.c.RemoveDirectory(path.Join(sy.o.remote, dir)) != nil {
			return
		}
	}
}

// deleteLocal applies --pull-deletes to the laptop files the sandbox deleted
// that the laptop has not changed since the last sync. Over maxPullDeletes in
// one pass, none is deleted. The list goes to notify before any is applied, and
// each file is moved into trash, never unlinked.
func (sy *syncer) deleteLocal(ctx context.Context, v *passView, rep *syncReport) error {
	if len(v.localGoing) == 0 {
		return nil
	}
	if len(v.localGoing) > sy.o.maxPullDeletes {
		reason := fmt.Sprintf("deleted in the sandbox; %d deletes in one pass is over --max-pull-deletes (%d), so none is applied", len(v.localGoing), sy.o.maxPullDeletes)
		for _, rel := range v.localGoing {
			rep.refuse(rel, reason)
		}
		return nil
	}
	if sy.o.notify != nil {
		for _, rel := range v.localGoing {
			fmt.Fprintf(sy.o.notify, "deleting on this computer (deleted in the sandbox; kept in %s): %s\n", syncShown(sy.o.trash), syncShown(rel))
		}
	}
	for _, rel := range v.localGoing {
		if err := ctx.Err(); err != nil {
			return err
		}
		l := v.local[rel]
		if !sy.unchangedLocally(rel, &l) {
			rep.Conflicts = append(rep.Conflicts, rel)
			continue
		}
		if err := sy.moveToTrash(rel); err != nil {
			rep.refuse(rel, "delete on this computer: "+err.Error())
			continue
		}
		delete(sy.st.Files, rel)
		rep.DeletedLocal = append(rep.DeletedLocal, rel)
		rep.KeptIn = sy.o.trash
	}
	return nil
}

// moveToTrash copies rel out of the root into trash, then removes it.
func (sy *syncer) moveToTrash(rel string) error {
	src, err := sy.root.Open(rel)
	if err != nil {
		return err
	}
	defer src.Close()
	dst := filepath.Join(sy.o.trash, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, src)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return sy.root.Remove(rel)
}

func (sy *syncer) walkLocal(rep *syncReport) (files map[string]syncFile, dirs []string, tree treeSeen, err error) {
	files = map[string]syncFile{}
	tree = newTreeSeen()
	count := 0
	err = fs.WalkDir(sy.root.FS(), ".", func(rel string, d fs.DirEntry, werr error) error {
		if werr != nil {
			if rel == "." {
				return werr
			}
			delete(tree.listed, rel)
			rep.refuse(rel, "unreadable: "+werr.Error())
			return nil
		}
		if rel == "." {
			sy.ig.load(sy.root, rel)
			return nil
		}
		if count++; count > maxSyncEntries {
			return fmt.Errorf("the local directory holds more than %d entries", maxSyncEntries)
		}
		tree.seen[rel] = true
		switch {
		case syncDenied(rel):
			rep.refuse(rel, "denylist")
			return skipIf(d.IsDir())
		case syncInvisible(d.Name()):
			rep.refuse(rel, "invisible character in the name")
			return skipIf(d.IsDir())
		case sy.ig.orgDenied(rel, d.IsDir()):
			rep.refuse(rel, "organisation rule")
			return skipIf(d.IsDir())
		case sy.ig.ignored(rel, d.IsDir()):
			rep.Ignored++
			return skipIf(d.IsDir())
		case d.Type()&fs.ModeSymlink != 0:
			rep.refuse(rel, "symlink")
		case d.IsDir():
			dirs = append(dirs, rel)
			tree.listed[rel] = true
			sy.ig.load(sy.root, rel)
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
	return files, dirs, tree, err
}

func skipIf(dir bool) error {
	if dir {
		return fs.SkipDir
	}
	return nil
}

func (sy *syncer) walkRemote(ctx context.Context, rep *syncReport) (*remoteView, error) {
	v := &remoteView{files: map[string]os.FileInfo{}, blocked: map[string]bool{}, treeSeen: newTreeSeen()}
	count := 0
	var walk func(abs, rel string) error
	walk = func(abs, rel string) error {
		lctx, cancel := context.WithCancel(ctx)
		sy.meter.arm(cancel)
		entries, err := sy.c.ReadDirContext(lctx, abs)
		over := sy.meter.disarm()
		cancel()
		if over {
			return fmt.Errorf("list %s in the sandbox: the listing is larger than %d MiB", abs, maxSyncListBytes>>20)
		}
		if err != nil {
			if rel != "" && !syncAbort(ctx, err) {
				rep.refuse(rel, "unreadable: "+err.Error())
				return nil
			}
			return fmt.Errorf("list %s in the sandbox: %w", abs, err)
		}
		if rel != "" {
			v.listed[rel] = true
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
			v.seen[childRel] = true
			switch {
			case syncDenied(childRel):
				rep.refuse(childRel, "denylist")
			case sy.ig.orgDenied(childRel, fi.IsDir()):
				rep.refuse(childRel, "organisation rule")
			case sy.ig.ignored(childRel, fi.IsDir()):
				rep.Ignored++
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
	if syncIsIgnoreFile(rel) {
		rep.refuse(rel, "ignore file")
		return nil
	}
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

// laptopDeleted reports whether the laptop deleted rel: its walk read rel's
// directory and found no rel, and the laptop tree is not empty as a whole.
func (v *passView) laptopDeleted(rel string) bool {
	return !v.localEmpty && v.localTree.gone(rel)
}
