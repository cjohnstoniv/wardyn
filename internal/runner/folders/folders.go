// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package folders is a folder-only runner's side of the wire: it lists the
// folders under its roots and runs one `wardyn sync` pass (internal/foldersync)
// between one of them and a run's sandbox, over a stream the organisation joins
// to that sandbox's own sftp-server. It holds no sandbox and runs no code from
// the organisation or a run.
//
// SECURITY: every path the organisation names is checked here, on the person's
// computer. It must sit inside a root (the runner owner's home unless the
// person named others), contain no symlink, and neither be nor go through a
// credential dotfile directory (runner.DeniedUserSegment). The folder is opened
// once, at that check, from its root's handle one directory at a time
// (foldersync.OpenDir), and every later read and write goes through that handle
// the same way: a folder renamed or replaced by a link after its check is never
// followed.
package folders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/sftp"

	"github.com/cjohnstoniv/wardyn/internal/foldersync"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

const (
	// maxFolders bounds one listing; the rest are left off.
	maxFolders = 500
	// maxCounted bounds the entries one folder's count walks.
	maxCounted = 20_000
	// maxPasses bounds the passes running at once.
	maxPasses = 4
)

// passKept is how long a finished pass's outcome waits for sync_folder_wait.
var passKept = 10 * time.Minute

// backPassLimit bounds a pass back when the organisation names no deadline (an older one), so a
// stuck stream cannot hold its run's pass forever. A seed pass then has no limit but the runner's.
var backPassLimit = 5 * time.Minute

// listBudget bounds the counting one listing does; folders not reached by then
// are listed with More set.
var listBudget = 3 * time.Second

// listChecked runs between a browse's check of its folder and its first read; tests change the folder there.
var listChecked = func() {}

// rootChecked runs between New's check of a root and its open; tests change the root there.
var rootChecked = func() {}

// folderChecked runs between the check of a folder's path and its open; tests change the path there.
var folderChecked = func() {}

// Server serves list_folders, sync_folder and sync_folder_wait.
type Server struct {
	ctx      context.Context
	roots    []string
	stateDir string
	// handles are the roots, opened once by New and closed when ctx ends: every folder is opened from
	// its root's handle, never again by its path.
	handles []*os.Root

	// Clock turns the organisation's deadline for a pass into time left here: srvclock.Clock does it
	// from the authenticated hello. Nil, or no reading yet, reads every deadline as absent.
	Clock interface {
		Remaining(at time.Time) (time.Duration, error)
	}

	mu      sync.Mutex
	running int
	passes  map[uint32]*pass
	// byRun holds the runs with a pass running: one at a time each, from before its state is read
	// until after it is saved.
	byRun map[uuid.UUID]bool
}

type pass struct {
	done chan struct{}
	res  runnerwire.FolderPass
}

// New checks roots (absolute directories, resolved through any symlink once,
// here), opens each, and keeps each pass's state under stateDir/folders. ctx ends
// every pass and closes the roots.
func New(ctx context.Context, roots []string, stateDir string) (*Server, error) {
	if len(roots) == 0 {
		return nil, errors.New("folders: no root folder")
	}
	s := &Server{ctx: ctx, stateDir: filepath.Join(stateDir, "folders"), passes: map[uint32]*pass{}, byRun: map[uuid.UUID]bool{}}
	// A root refused below closes every root opened before it.
	built := false
	defer func() {
		if !built {
			s.closeRoots()
		}
	}()
	for _, r := range roots {
		if !filepath.IsAbs(r) {
			return nil, fmt.Errorf("folders: root %q is not an absolute path", r)
		}
		real, err := filepath.EvalSymlinks(r)
		if err != nil {
			return nil, fmt.Errorf("folders: root %q: %w", r, err)
		}
		if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("folders: root %q is not a directory", r)
		}
		if seg := runner.DeniedUserSegment(real); seg != "" {
			return nil, fmt.Errorf("folders: root %q is inside %s, which holds credentials", r, seg)
		}
		rootChecked()
		h, err := openRoot(real)
		if err != nil {
			return nil, fmt.Errorf("folders: root %q: %w", r, err)
		}
		s.roots = append(s.roots, real)
		s.handles = append(s.handles, h)
	}
	context.AfterFunc(ctx, s.closeRoots)
	built = true
	return s, nil
}

// openRoot opens real, a root New has resolved and checked, one directory at a time from the top of
// its volume (foldersync.OpenDir). The handle kept for the server's life is then the directory that
// path names with no link in it: a root renamed or replaced by a link between its check and its open
// refuses the start, and is never opened through. Opening a directory needs read access to it, so a
// folder on the way that the runner's user can pass through but not read refuses the start too, and
// the refusal names that folder.
func openRoot(real string) (*os.Root, error) {
	top := filepath.VolumeName(real) + string(filepath.Separator)
	rel, err := filepath.Rel(top, real)
	if err != nil {
		return nil, err
	}
	cur, err := os.OpenRoot(top)
	if err != nil {
		return nil, err
	}
	at := top
	for _, name := range strings.Split(filepath.ToSlash(rel), "/") {
		at = filepath.Join(at, name)
		next, err := foldersync.OpenDir(cur, name)
		_ = cur.Close()
		if errors.Is(err, fs.ErrPermission) {
			return nil, fmt.Errorf("the runner's user cannot read the folder %s; the runner opens each folder from the top of the drive down to a root, so make that folder readable, or choose another root", at)
		}
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return cur, nil
}

// closeRoots closes the roots' handles. A folder opened from one stays open until its own close.
func (s *Server) closeRoots() {
	for _, h := range s.handles {
		_ = h.Close()
	}
}

// credentialPath is Options.Credential for every pass and survey: a path inside the chosen folder
// that is or goes through a credential folder (runner.DeniedUserSegment) never crosses either way.
func credentialPath(rel string) bool { return runner.DeniedUserSegment(rel) != "" }

// Roots is the resolved roots.
func (s *Server) Roots() []string { return slices.Clone(s.roots) }

// Call serves the three folder methods and refuses every other one.
func (s *Server) Call(ctx context.Context, peer *runnerwire.Peer, id uint32, method string, raw json.RawMessage) (any, error) {
	switch method {
	case runnerwire.MethodListFolders:
		var args runnerwire.ListFoldersArgs
		if err := runnerwire.Unmarshal(raw, &args); err != nil {
			return nil, err
		}
		return s.list(ctx, args)
	case runnerwire.MethodSyncFolder:
		var args runnerwire.SyncFolderArgs
		if err := runnerwire.Unmarshal(raw, &args); err != nil {
			return nil, err
		}
		return s.start(ctx, peer, id, args)
	case runnerwire.MethodSyncFolderWait:
		var args runnerwire.SyncFolderWaitArgs
		if err := runnerwire.Unmarshal(raw, &args); err != nil {
			return nil, err
		}
		return s.wait(ctx, args.Stream)
	}
	return nil, runnerwire.Refuse("this runner shares folders only and holds no sandbox")
}

// folder checks p, a path the organisation named, and returns it opened (browsable).
func (s *Server) folder(p string) (*os.Root, error) {
	if slices.Contains(s.roots, p) {
		return nil, runnerwire.Refuse("a whole shared folder is never sent: choose a folder inside it")
	}
	return s.browsable(p)
}

// browsable is folder's check without the root refusal: a root can be opened to list what is
// inside it, never sent. It returns the folder opened from its root's handle one directory at a
// time, so what is read and written later is the folder checked here, whatever is renamed or linked
// in at its path afterwards. The caller closes it.
func (s *Server) browsable(p string) (*os.Root, error) {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return nil, runnerwire.Refuse("the folder must be a clean absolute path")
	}
	i := slices.IndexFunc(s.roots, func(r string) bool {
		return p == r || strings.HasPrefix(p, strings.TrimSuffix(r, string(filepath.Separator))+string(filepath.Separator))
	})
	if i < 0 {
		return nil, runnerwire.Refuse("that folder is not under a folder this runner shares")
	}
	if seg := runner.DeniedUserSegment(p); seg != "" {
		return nil, runnerwire.Refuse("a folder inside %s is never shared: it holds credentials", seg)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return nil, runnerwire.Refuse("that folder does not exist on this computer")
	}
	if real != p {
		return nil, runnerwire.Refuse("that path goes through a symlink, which is never followed")
	}
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		return nil, runnerwire.Refuse("that path is not a folder")
	}
	rel, err := filepath.Rel(s.roots[i], p)
	if err != nil {
		return nil, runnerwire.Refuse("that folder is not under a folder this runner shares")
	}
	folderChecked()
	dir, err := foldersync.OpenDir(s.handles[i], filepath.ToSlash(rel))
	if errors.Is(err, foldersync.ErrLinkInPath) {
		return nil, runnerwire.Refuse("that path goes through a symlink, which is never followed")
	}
	if err != nil {
		return nil, runnerwire.Refuse("that folder cannot be read")
	}
	return dir, nil
}

func (s *Server) list(ctx context.Context, args runnerwire.ListFoldersArgs) (runnerwire.FolderList, error) {
	if args.Path == "" {
		out := runnerwire.FolderList{Folders: []runnerwire.FolderEntry{}}
		for _, r := range s.roots {
			out.Folders = append(out.Folders, runnerwire.FolderEntry{Name: filepath.Base(r), Path: r, More: true})
		}
		return out, nil
	}
	root, err := s.browsable(args.Path)
	if err != nil {
		return runnerwire.FolderList{}, err
	}
	defer root.Close()
	listChecked()
	dir := args.Path
	if args.Survey && slices.Contains(s.roots, dir) {
		return runnerwire.FolderList{}, runnerwire.Refuse("a whole shared folder is never sent: choose a folder inside it")
	}
	ents, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return runnerwire.FolderList{}, runnerwire.Refuse("that folder cannot be read")
	}
	out := runnerwire.FolderList{Path: dir, Folders: []runnerwire.FolderEntry{}}
	deadline := time.Now().Add(listBudget)
	for _, e := range ents {
		// A symlink, a hidden folder and a credential folder are never listed.
		if !e.IsDir() || e.Type()&fs.ModeSymlink != 0 || strings.HasPrefix(e.Name(), ".") ||
			runner.DeniedUserSegment(filepath.Join(dir, e.Name())) != "" {
			continue
		}
		if len(out.Folders) == maxFolders {
			break
		}
		f := runnerwire.FolderEntry{Name: e.Name(), Path: filepath.Join(dir, e.Name()), More: true}
		if ctx.Err() == nil && time.Now().Before(deadline) {
			f.Files, f.Bytes, f.More = count(root, e.Name(), deadline)
		}
		out.Folders = append(out.Folders, f)
	}
	if args.Survey {
		sv, err := foldersync.Survey(root, args.Deny, credentialPath)
		if err != nil {
			return runnerwire.FolderList{}, runnerwire.Refuse("this folder cannot be sent: %s", foldersync.Shown(err.Error()))
		}
		out.Survey = &runnerwire.FolderSurvey{Files: sv.Files, Bytes: sv.Bytes, LeftOut: sv.LeftOut, Ignored: sv.Ignored}
	}
	return out, nil
}

// count walks rel inside root, never through a symlink (foldersync.FS), for at
// most maxCounted entries and until deadline.
func count(root *os.Root, rel string, deadline time.Time) (files int, bytes int64, more bool) {
	seen := 0
	stop := errors.New("stop")
	err := fs.WalkDir(foldersync.FS(root), rel, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if seen++; seen > maxCounted || seen%1000 == 0 && time.Now().After(deadline) {
			return stop
		}
		if d.Type().IsRegular() {
			files++
			if fi, err := d.Info(); err == nil {
				bytes += fi.Size()
			}
		}
		return nil
	})
	return files, bytes, errors.Is(err, stop)
}

func (s *Server) start(ctx context.Context, peer *runnerwire.Peer, id uint32, args runnerwire.SyncFolderArgs) (any, error) {
	if args.RunID == uuid.Nil {
		return nil, runnerwire.Refuse("a pass names its run")
	}
	s.mu.Lock()
	if s.byRun[args.RunID] {
		s.mu.Unlock()
		return nil, &runnerwire.Error{Code: runnerwire.CodeFolderPassBusy, Message: "a pass for this run is still running; try again"}
	}
	s.byRun[args.RunID] = true
	s.mu.Unlock()
	started := false
	defer func() {
		if !started {
			s.endRun(args.RunID)
		}
	}()
	root, err := s.folder(args.Path)
	if err != nil {
		return nil, err
	}
	// The folder stays open from its check to the end of its pass, through the waits on the
	// organisation and the sandbox: run closes it, and every refusal before then does.
	defer func() {
		if !started {
			_ = root.Close()
		}
	}()
	dir := args.Path
	remote, err := foldersync.RemoteDir(args.RemoteDir)
	if err != nil {
		return nil, runnerwire.Refuse("%s", err.Error())
	}
	stateFile := filepath.Join(s.stateDir, args.RunID.String()+".json")
	if err := foldersync.StateOutside(dir, stateFile); err != nil {
		return nil, runnerwire.Refuse("%s", err.Error())
	}
	end, err := s.passEnd(args)
	if err != nil {
		return nil, err
	}
	st, err := foldersync.LoadStateRecover(stateFile, dir, remote)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	busy := s.running >= maxPasses
	if !busy {
		s.running++
	}
	s.mu.Unlock()
	if busy {
		return nil, runnerwire.Refuse("this runner already runs %d passes", maxPasses)
	}
	release := func() {
		s.mu.Lock()
		s.running--
		s.mu.Unlock()
	}
	stream, err := peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindFolder, Target: args.RunID.String(), Call: id})
	if err != nil {
		release()
		return nil, err
	}
	p := &pass{done: make(chan struct{})}
	s.mu.Lock()
	s.passes[stream.ID()] = p
	s.mu.Unlock()
	started = true
	go func() {
		p.res = s.run(stream, root, remote, st, stateFile, args, end)
		_ = stream.Close()
		release()
		s.endRun(args.RunID)
		close(p.done)
		// An outcome nobody waits for is dropped after a while.
		time.AfterFunc(passKept, func() {
			s.mu.Lock()
			if s.passes[stream.ID()] == p {
				delete(s.passes, stream.ID())
			}
			s.mu.Unlock()
		})
	}()
	return runnerwire.StreamResult{Stream: stream.ID()}, nil
}

func (s *Server) endRun(id uuid.UUID) {
	s.mu.Lock()
	delete(s.byRun, id)
	s.mu.Unlock()
}

// passEnd is when one pass must end, on this runner's monotonic clock: the organisation's deadline,
// read through Clock, when it names one; otherwise backPassLimit from now for a pass back, and none
// (zero) for a seed pass. A deadline already passed is refused.
func (s *Server) passEnd(args runnerwire.SyncFolderArgs) (time.Time, error) {
	if args.DeadlineUnixMs > 0 && s.Clock != nil {
		if left, err := s.Clock.Remaining(time.UnixMilli(args.DeadlineUnixMs)); err == nil {
			if left <= 0 {
				return time.Time{}, errors.New("the pass deadline has passed")
			}
			return time.Now().Add(left), nil
		}
	}
	if args.Pull {
		return time.Now().Add(backPassLimit), nil
	}
	return time.Time{}, nil
}

// passContext is s.ctx, ended at end when it is set.
func (s *Server) passContext(end time.Time) (context.Context, context.CancelFunc) {
	if end.IsZero() {
		return context.WithCancel(s.ctx)
	}
	return context.WithDeadline(s.ctx, end)
}

// run is one pass over stream, which the organisation joins to the sandbox's
// sftp-server started in remote, on root, the folder start opened. It closes root.
func (s *Server) run(stream duplex, root *os.Root, remote string, st *foldersync.State, stateFile string, args runnerwire.SyncFolderArgs, end time.Time) runnerwire.FolderPass {
	defer root.Close()
	failed := func(err error) runnerwire.FolderPass {
		return runnerwire.FolderPass{Refused: []runnerwire.FolderRefusal{}, Recovered: st.Recovered, Error: foldersync.Shown(err.Error())}
	}
	ctx, cancel := s.passContext(end)
	defer cancel()
	// Closing the stream unblocks an sftp call stuck on a sandbox that stalls.
	defer context.AfterFunc(ctx, func() { _ = stream.Close() })()
	meter := foldersync.NewListMeter(stream)
	c, err := sftp.NewClientPipe(meter, halfCloser{stream})
	if err != nil {
		return failed(err)
	}
	// The pass is over once Pass returns: the stream is closed first, so the client's reader ends
	// whether or not the sandbox side ever closes its half.
	defer func() { _ = stream.Close(); _ = c.Close() }()
	// A directory the sandbox lacks starts sftp-server elsewhere: refuse that.
	if got, err := c.RealPath("."); err != nil || got != remote {
		return failed(fmt.Errorf("the sandbox has no folder %s", remote))
	}
	sy := &foldersync.Syncer{Client: c, Meter: meter, Root: root, State: st, StateFile: stateFile, StateRoot: filepath.Dir(s.stateDir), Opts: foldersync.Options{
		Remote: remote, Pull: args.Pull, MaxPullBytes: foldersync.DefaultMaxPullBytes, OrgDeny: args.Deny,
		Credential: credentialPath, RunnerPass: true, RunTag: args.RunID.String()[:8],
	}}
	// A pass cut short (its stream closed) still reports what it finished and saved in its state.
	rep, err := sy.Pass(ctx)
	if rep == nil {
		return failed(err)
	}
	out := runnerwire.FolderPass{Pushed: len(rep.Pushed), Pulled: len(rep.Pulled), Conflicts: len(rep.Conflicts),
		Ignored: rep.Ignored, BytesOut: rep.BytesOut, BytesIn: rep.BytesIn, Unsynced: rep.Unsynced, Refused: []runnerwire.FolderRefusal{},
		NotPreserved: foldersync.Shown(rep.NotPreserved), Recovered: st.Recovered}
	if err != nil {
		out.Error = foldersync.Shown(err.Error())
	}
	unsynced := slices.Sorted(slices.Values(rep.UnsyncedPaths))
	for _, p := range unsynced[:min(len(unsynced), runnerwire.MaxFolderUnsyncedPaths)] {
		out.UnsyncedPaths = append(out.UnsyncedPaths, foldersync.Shown(p))
	}
	for _, r := range rep.Refused[:min(len(rep.Refused), runnerwire.MaxFolderRefusals)] {
		out.Refused = append(out.Refused, runnerwire.FolderRefusal{Path: foldersync.Shown(r.Path), Reason: r.Reason})
	}
	return out
}

func (s *Server) wait(ctx context.Context, id uint32) (runnerwire.FolderPass, error) {
	s.mu.Lock()
	p, ok := s.passes[id]
	s.mu.Unlock()
	if !ok {
		return runnerwire.FolderPass{}, runnerwire.Refuse("no pass on that stream")
	}
	select {
	case <-p.done:
		return p.res, nil
	case <-ctx.Done():
		return runnerwire.FolderPass{}, ctx.Err()
	}
}

// duplex is a pass's byte stream: a *runnerwire.Stream on the wire.
type duplex interface {
	io.Reader
	io.Writer
	Close() error
	CloseWrite() error
}

// halfCloser closes only the stream's write side, so the client still reads
// the server's last replies.
type halfCloser struct{ st duplex }

func (h halfCloser) Write(b []byte) (int, error) { return h.st.Write(b) }
func (h halfCloser) Close() error                { return h.st.CloseWrite() }
