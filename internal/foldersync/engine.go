// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

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
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
)

const (
	// MaxEntries bounds one side's listing: the sandbox chooses what is in it.
	MaxEntries = 200_000
	// maxSyncListBytes bounds the sftp bytes one remote directory listing may
	// read. The sftp client holds a whole directory in memory before the entry
	// cap above can apply, so the budget is what bounds that memory.
	maxSyncListBytes = 64 << 20
	// MaxPullFile is the largest single file a pull accepts.
	MaxPullFile = 256 << 20
	// DefaultMaxPullBytes is --max-pull-bytes: what one pass may pull in total.
	DefaultMaxPullBytes = 1 << 30
)

// Report is one pass's outcome. Every list is non-nil so --json prints [].
type Report struct {
	Pushed    []string  `json:"pushed"`
	Pulled    []string  `json:"pulled"`
	Conflicts []string  `json:"conflicts"`
	Refused   []Refusal `json:"refused"`
	// DeletedRemote and Renamed are what this pass removed or renamed in the
	// sandbox to follow the laptop; DeletedLocal is what --pull-deletes removed
	// on the laptop (each kept under KeptIn).
	DeletedRemote []string `json:"deleted_in_sandbox"`
	Renamed       []Rename `json:"renamed_in_sandbox"`
	DeletedLocal  []string `json:"deleted_on_laptop"`
	KeptIn        string   `json:"deleted_kept_in,omitempty"`
	// Ignored counts the entries the ignore files left out, on both sides.
	Ignored  int   `json:"ignored"`
	BytesOut int64 `json:"bytes_to_sandbox"`
	BytesIn  int64 `json:"bytes_from_sandbox"`
	seenSet  map[string]bool
}

type Rename struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type Refusal struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

func NewReport() *Report {
	return &Report{Pushed: []string{}, Pulled: []string{}, Conflicts: []string{}, Refused: []Refusal{},
		DeletedRemote: []string{}, Renamed: []Rename{}, DeletedLocal: []string{}, seenSet: map[string]bool{}}
}

func (r *Report) refuse(rel, reason string) {
	if k := rel + "\x00" + reason; !r.seenSet[k] {
		r.seenSet[k] = true
		r.Refused = append(r.Refused, Refusal{rel, reason})
	}
}

func (r *Report) Empty() bool {
	return len(r.Pushed)+len(r.Pulled)+len(r.Conflicts)+len(r.Refused)+len(r.DeletedRemote)+len(r.Renamed)+len(r.DeletedLocal) == 0
}

type Options struct {
	Remote       string // absolute, validated sandbox directory
	Pull         bool
	MaxPullBytes int64
	// PullDeletes (--pull-deletes, needs Pull) lets a sandbox delete remove the
	// laptop's copy, at most MaxPullDeletes per pass, moving each into Trash.
	PullDeletes    bool
	MaxPullDeletes int
	Trash          string
	// OrgDeny is the organisation's wall (GET /runs/{id} sync.deny): ignore
	// patterns no file in the tree can take back.
	OrgDeny []string
	// Credential, when set, names the paths (slash-separated, relative to the synced root) that hold
	// credentials. Each is left out with its whole subtree in both directions, as the denylist is.
	// The folder-only runner sets it to runner.DeniedUserSegment; the CLI leaves it nil.
	Credential func(rel string) bool
	// Notify gets the list of laptop deletes before they are applied; nil drops it.
	Notify io.Writer
}

// Syncer holds one sync's connections. Every read and write on the laptop goes
// through Root, so a link swapped in under the local directory cannot lead one
// outside it.
type Syncer struct {
	Client    *sftp.Client
	Meter     *ListMeter // the byte count on Client's input; nil leaves a listing unbounded
	Root      *os.Root
	Opts      Options
	State     *State
	StateFile string
	ig        *syncIgnore // this pass's ignore rules, read by walkLocal
}

// ListMeter counts the bytes the sftp client reads while a directory listing
// is armed, and cancels the listing once it passes maxSyncListBytes.
type ListMeter struct {
	r      io.Reader
	mu     sync.Mutex
	n      int64
	over   bool
	cancel context.CancelFunc
}

// NewListMeter meters r, the sftp client's input.
func NewListMeter(r io.Reader) *ListMeter { return &ListMeter{r: r} }

func (m *ListMeter) Read(p []byte) (int, error) {
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
func (m *ListMeter) arm(cancel context.CancelFunc) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.n, m.over, m.cancel = 0, false, cancel
	m.mu.Unlock()
}

// disarm stops counting and reports whether the budget was passed.
func (m *ListMeter) disarm() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cancel = nil
	return m.over
}

type syncFile struct {
	side Side
	mode fs.FileMode
}

func localSide(fi fs.FileInfo) Side { return Side{fi.Size(), fi.ModTime().UnixNano()} }

// remoteSide has second resolution: that is all the sftp attributes carry.
// ponytail: a sandbox edit of the same size inside the recorded second, or with
// its mtime set back, is not seen; the laptop's copy stays safe. A content hash
// would close it at the price of reading every file on each pass.
func remoteSide(fi fs.FileInfo) Side {
	return Side{fi.Size(), fi.ModTime().Unix() * int64(time.Second)}
}

// Abort reports whether err ends the whole pass: the connection is gone or
// the user interrupted. A failure on one path (a permission, a type clash, a
// name too long) is a refusal of that path instead, so the sandbox cannot wedge
// every later pass with one bad entry.
func Abort(ctx context.Context, err error) bool {
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
func (sy *Syncer) pathFailed(ctx context.Context, rep *Report, rel, what string, err error) error {
	if Abort(ctx, err) {
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
func (sy *Syncer) Pass(ctx context.Context) (*Report, error) {
	rep := NewReport()
	sy.ig = newSyncIgnore(sy.Opts.OrgDeny)
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
	for rel := range sy.State.Files {
		if v.localTree.gone(rel) && remote.gone(rel) {
			delete(sy.State.Files, rel)
		}
	}
	if err := sy.State.Save(sy.StateFile); err != nil {
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
func (sy *Syncer) decide(ctx context.Context, rel string, v *passView, rep *Report, pulled *int64) error {
	l, hasL := v.local[rel]
	r, hasR := v.remote.files[rel]
	seen, known := sy.State.Files[rel]
	var rSide Side
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
	case hasL && hasR && lChanged && rChanged && (known || sy.Opts.Pull):
		rep.Conflicts = append(rep.Conflicts, rel)
		return sy.push(ctx, rel, l, rep)
	case hasL && lChanged:
		return sy.push(ctx, rel, l, rep)
	case hasL && !lChanged && rChanged && sy.Opts.Pull:
		return sy.pull(ctx, rel, r, &l, rep, pulled)
	case !hasL && hasR && sy.Opts.Pull && rChanged:
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
func (sy *Syncer) deleted(ctx context.Context, rel string, v *passView, rep *Report, hasL, hasR, lChanged, rChanged bool) (bool, error) {
	switch {
	case !hasL && hasR && v.laptopDeleted(rel):
		if rChanged {
			rep.Conflicts = append(rep.Conflicts, rel)
			return true, nil
		}
		return true, sy.removeRemote(ctx, rel, v, rep)
	case hasL && !hasR && !lChanged && sy.Opts.PullDeletes && len(v.remote.seen) > 0 && v.remote.gone(rel):
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
func (sy *Syncer) renames(ctx context.Context, rels []string, v *passView, eligible func(string) bool, rep *Report) (map[string]bool, error) {
	gone, fresh := map[Side][]string{}, map[Side][]string{}
	for _, rel := range rels {
		if !eligible(rel) {
			continue
		}
		l, hasL := v.local[rel]
		r, hasR := v.remote.files[rel]
		seen, known := sy.State.Files[rel]
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

func (sy *Syncer) renameRemote(ctx context.Context, from, to string, v *passView, rep *Report) error {
	dst := path.Join(sy.Opts.Remote, to)
	if err := sy.Client.MkdirAll(path.Dir(dst)); err != nil {
		return sy.pathFailed(ctx, rep, to, "create the directory in the sandbox", err)
	}
	if err := sy.Client.Rename(path.Join(sy.Opts.Remote, from), dst); err != nil {
		return sy.pathFailed(ctx, rep, to, "rename in the sandbox", err)
	}
	rfi, err := sy.Client.Stat(dst)
	if err != nil {
		return sy.pathFailed(ctx, rep, to, "stat in the sandbox", err)
	}
	delete(sy.State.Files, from)
	sy.State.Files[to] = FileSeen{Local: v.local[to].side, Remote: remoteSide(rfi)}
	rep.Renamed = append(rep.Renamed, Rename{From: from, To: to})
	sy.pruneRemoteDirs(from, v)
	return nil
}

func (sy *Syncer) removeRemote(ctx context.Context, rel string, v *passView, rep *Report) error {
	p := path.Join(sy.Opts.Remote, rel)
	// The agent may have written the file since the walk: only the copy the
	// walk saw is deleted, and a changed one is kept as a conflict.
	fi, err := sy.Client.Lstat(p)
	if err != nil {
		return sy.pathFailed(ctx, rep, rel, "stat in the sandbox", err)
	}
	if !fi.Mode().IsRegular() || remoteSide(fi) != remoteSide(v.remote.files[rel]) {
		rep.Conflicts = append(rep.Conflicts, rel)
		return nil
	}
	if err := sy.Client.Remove(p); err != nil {
		return sy.pathFailed(ctx, rep, rel, "delete in the sandbox", err)
	}
	delete(sy.State.Files, rel)
	rep.DeletedRemote = append(rep.DeletedRemote, rel)
	sy.pruneRemoteDirs(rel, v)
	return nil
}

// pruneRemoteDirs removes the sandbox directories above rel that the laptop no
// longer has, while they are empty: a directory with anything left in it stays.
func (sy *Syncer) pruneRemoteDirs(rel string, v *passView) {
	for dir := path.Dir(rel); dir != "." && v.localTree.gone(dir); dir = path.Dir(dir) {
		if sy.Client.RemoveDirectory(path.Join(sy.Opts.Remote, dir)) != nil {
			return
		}
	}
}

// deleteLocal applies --pull-deletes to the laptop files the sandbox deleted
// that the laptop has not changed since the last sync. Over maxPullDeletes in
// one pass, none is deleted. The list goes to notify before any is applied, and
// each file is moved into trash, never unlinked.
func (sy *Syncer) deleteLocal(ctx context.Context, v *passView, rep *Report) error {
	if len(v.localGoing) == 0 {
		return nil
	}
	if len(v.localGoing) > sy.Opts.MaxPullDeletes {
		reason := fmt.Sprintf("deleted in the sandbox; %d deletes in one pass is over --max-pull-deletes (%d), so none is applied", len(v.localGoing), sy.Opts.MaxPullDeletes)
		for _, rel := range v.localGoing {
			rep.refuse(rel, reason)
		}
		return nil
	}
	if sy.Opts.Notify != nil {
		for _, rel := range v.localGoing {
			fmt.Fprintf(sy.Opts.Notify, "deleting on this computer (deleted in the sandbox; kept in %s): %s\n", Shown(sy.Opts.Trash), Shown(rel))
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
		delete(sy.State.Files, rel)
		rep.DeletedLocal = append(rep.DeletedLocal, rel)
		rep.KeptIn = sy.Opts.Trash
	}
	return nil
}

// moveToTrash copies rel out of the root into trash, then removes it.
func (sy *Syncer) moveToTrash(rel string) error {
	src, err := sy.Root.Open(rel)
	if err != nil {
		return err
	}
	defer src.Close()
	dst := filepath.Join(sy.Opts.Trash, filepath.FromSlash(rel))
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
	return sy.Root.Remove(rel)
}

func (sy *Syncer) walkLocal(rep *Report) (files map[string]syncFile, dirs []string, tree treeSeen, err error) {
	files = map[string]syncFile{}
	tree = newTreeSeen()
	count := 0
	err = fs.WalkDir(sy.Root.FS(), ".", func(rel string, d fs.DirEntry, werr error) error {
		if werr != nil {
			if rel == "." {
				return werr
			}
			delete(tree.listed, rel)
			rep.refuse(rel, "unreadable: "+werr.Error())
			return nil
		}
		if rel == "." {
			sy.ig.load(sy.Root, rel)
			return nil
		}
		if count++; count > MaxEntries {
			return fmt.Errorf("the local directory holds more than %d entries", MaxEntries)
		}
		tree.seen[rel] = true
		switch {
		case syncDenied(rel):
			rep.refuse(rel, "denylist")
			return skipIf(d.IsDir())
		case sy.Opts.Credential != nil && sy.Opts.Credential(rel):
			rep.refuse(rel, ReasonCredential)
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
			sy.ig.load(sy.Root, rel)
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

func (sy *Syncer) walkRemote(ctx context.Context, rep *Report) (*remoteView, error) {
	v := &remoteView{files: map[string]os.FileInfo{}, blocked: map[string]bool{}, treeSeen: newTreeSeen()}
	count := 0
	var walk func(abs, rel string) error
	walk = func(abs, rel string) error {
		lctx, cancel := context.WithCancel(ctx)
		sy.Meter.arm(cancel)
		entries, err := sy.Client.ReadDirContext(lctx, abs)
		over := sy.Meter.disarm()
		cancel()
		if over {
			return fmt.Errorf("list %s in the sandbox: the listing is larger than %d MiB", abs, maxSyncListBytes>>20)
		}
		if err != nil {
			if rel != "" && !Abort(ctx, err) {
				rep.refuse(rel, "unreadable: "+err.Error())
				return nil
			}
			return fmt.Errorf("list %s in the sandbox: %w", abs, err)
		}
		if rel != "" {
			v.listed[rel] = true
		}
		for _, fi := range entries {
			if count++; count > MaxEntries {
				return fmt.Errorf("the sandbox directory holds more than %d entries", MaxEntries)
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
			case sy.Opts.Credential != nil && sy.Opts.Credential(childRel):
				rep.refuse(childRel, ReasonCredential)
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
	if err := walk(sy.Opts.Remote, ""); err != nil {
		return nil, err
	}
	return v, nil
}

func (sy *Syncer) push(ctx context.Context, rel string, l syncFile, rep *Report) error {
	f, err := sy.Root.Open(rel)
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
	if lstat, err := sy.Root.Lstat(rel); err != nil || !os.SameFile(lstat, fi) {
		rep.refuse(rel, "symlink")
		return nil
	}
	dst := path.Join(sy.Opts.Remote, rel)
	if err := sy.Client.MkdirAll(path.Dir(dst)); err != nil {
		return sy.pathFailed(ctx, rep, rel, "create the directory in the sandbox", err)
	}
	out, err := sy.Client.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
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
	_ = sy.Client.Chmod(dst, l.mode.Perm())
	_ = sy.Client.Chtimes(dst, mt, mt)
	rfi, err := sy.Client.Stat(dst)
	if err != nil {
		return sy.pathFailed(ctx, rep, rel, "stat in the sandbox", err)
	}
	sy.State.Files[rel] = FileSeen{Local: l.side, Remote: remoteSide(rfi)}
	rep.Pushed = append(rep.Pushed, rel)
	return nil
}

// pull writes a sandbox file onto the laptop: never through a symlink, never
// over anything but the regular file the walk listed, and never with an exec
// bit. The bytes counted against the budget are the bytes read, not the size
// the sandbox listed.
func (sy *Syncer) pull(ctx context.Context, rel string, r os.FileInfo, walked *syncFile, rep *Report, pulled *int64) error {
	if syncIsIgnoreFile(rel) {
		rep.refuse(rel, "ignore file")
		return nil
	}
	if err := syncCheckLocal(sy.Root, rel); err != nil {
		rep.refuse(rel, err.Error())
		return nil
	}
	if fi, err := sy.Root.Lstat(rel); err == nil {
		switch {
		case walked == nil:
			rep.refuse(rel, "a local entry by another spelling is in the way")
			return nil
		case !fi.Mode().IsRegular():
			rep.refuse(rel, "a local entry that is not a regular file is in the way")
			return nil
		}
	}
	allowed := min(int64(MaxPullFile), sy.Opts.MaxPullBytes-*pulled)
	switch {
	case r.Size() > MaxPullFile:
		rep.refuse(rel, fmt.Sprintf("too large (over %d bytes)", MaxPullFile))
		return nil
	case r.Size() > allowed:
		rep.refuse(rel, "over --max-pull-bytes for this pass")
		return nil
	}
	mode := fs.FileMode(0o644)
	if walked != nil {
		mode = walked.mode.Perm() &^ 0o111
	}
	if err := sy.Root.MkdirAll(path.Dir(rel), 0o755); err != nil {
		return sy.pathFailed(ctx, rep, rel, "create the local directory", err)
	}
	src, err := sy.Client.Open(path.Join(sy.Opts.Remote, rel))
	if err != nil {
		return sy.pathFailed(ctx, rep, rel, "read in the sandbox", err)
	}
	defer src.Close()
	tmpName, tmp, err := sy.localTemp(rel)
	if err != nil {
		return sy.pathFailed(ctx, rep, rel, "create a local temp file", err)
	}
	defer sy.Root.Remove(tmpName)
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
	if err := sy.Root.Chmod(tmpName, mode); err != nil {
		return sy.pathFailed(ctx, rep, rel, "set the local mode", err)
	}
	mt := r.ModTime()
	_ = sy.Root.Chtimes(tmpName, mt, mt)
	if !sy.unchangedLocally(rel, walked) {
		rep.Conflicts = append(rep.Conflicts, rel)
		return nil
	}
	if err := sy.Root.Rename(tmpName, rel); err != nil {
		return sy.pathFailed(ctx, rep, rel, "replace the local file", err)
	}
	fi, err := sy.Root.Lstat(rel)
	if err != nil {
		return sy.pathFailed(ctx, rep, rel, "stat the local file", err)
	}
	sy.State.Files[rel] = FileSeen{Local: localSide(fi), Remote: remoteSide(r)}
	rep.Pulled = append(rep.Pulled, rel)
	return nil
}

// unchangedLocally re-checks the target just before it is replaced: it must
// still be the file the walk saw, or still absent. A developer's save made
// after the walk wins.
func (sy *Syncer) unchangedLocally(rel string, walked *syncFile) bool {
	fi, err := sy.Root.Lstat(rel)
	if walked == nil {
		return errors.Is(err, fs.ErrNotExist)
	}
	return err == nil && fi.Mode().IsRegular() && localSide(fi) == walked.side
}

// localTemp creates an exclusive 0600 temp file beside rel.
func (sy *Syncer) localTemp(rel string) (string, *os.File, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, err
	}
	name := path.Join(path.Dir(rel), ".wardyn-sync-"+hex.EncodeToString(b[:]))
	f, err := sy.Root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	return name, f, err
}

// laptopDeleted reports whether the laptop deleted rel: its walk read rel's
// directory and found no rel, and the laptop tree is not empty as a whole.
func (v *passView) laptopDeleted(rel string) bool {
	return !v.localEmpty && v.localTree.gone(rel)
}

// SurveyResult is what a pass from a folder would send, after every rule.
// LeftOut names the top-level paths the rules always leave out (.git and the
// like); Ignored counts the entries the ignore files leave out.
type SurveyResult struct {
	Files   int
	Bytes   int64
	LeftOut []string
	Ignored int
}

// ReasonCredential is the refusal reason for a path Options.Credential names.
const ReasonCredential = "credential folder"

// Survey walks root as a pass would, under the same rules, the organisation's
// orgDeny and credential (Options.Credential), and reads nothing but root.
func Survey(root *os.Root, orgDeny []string, credential func(rel string) bool) (SurveyResult, error) {
	sy := &Syncer{Root: root, Opts: Options{Credential: credential}, ig: newSyncIgnore(orgDeny)}
	rep := NewReport()
	files, _, _, err := sy.walkLocal(rep)
	if err != nil {
		return SurveyResult{}, err
	}
	out := SurveyResult{Files: len(files), Ignored: rep.Ignored, LeftOut: []string{}}
	for _, f := range files {
		out.Bytes += f.side.Size
	}
	for _, r := range rep.Refused {
		if (r.Reason == "denylist" || r.Reason == ReasonCredential) && !strings.Contains(r.Path, "/") {
			out.LeftOut = append(out.LeftOut, r.Path)
		}
	}
	return out, nil
}
