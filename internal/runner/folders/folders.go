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
// credential dotfile directory (runner.DeniedUserSegment). Reads and writes go
// through an os.Root on that path, so a link swapped in later cannot lead
// outside it.
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

// listBudget bounds the counting one listing does; folders not reached by then
// are listed with More set.
var listBudget = 3 * time.Second

// Server serves list_folders, sync_folder and sync_folder_wait.
type Server struct {
	ctx      context.Context
	roots    []string
	stateDir string

	mu      sync.Mutex
	running int
	passes  map[uint32]*pass
}

type pass struct {
	done chan struct{}
	res  runnerwire.FolderPass
}

// New checks roots (absolute directories, resolved through any symlink once,
// here) and keeps each pass's state under stateDir/folders. ctx ends every pass.
func New(ctx context.Context, roots []string, stateDir string) (*Server, error) {
	if len(roots) == 0 {
		return nil, errors.New("folders: no root folder")
	}
	s := &Server{ctx: ctx, stateDir: filepath.Join(stateDir, "folders"), passes: map[uint32]*pass{}}
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
		s.roots = append(s.roots, real)
	}
	return s, nil
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

// folder checks p, a path the organisation named, and returns it.
func (s *Server) folder(p string) (string, error) {
	if slices.Contains(s.roots, p) {
		return "", runnerwire.Refuse("a whole shared folder is never sent: choose a folder inside it")
	}
	return s.browsable(p)
}

// browsable is folder's check without the root refusal: a root can be opened to list what is
// inside it, never sent.
func (s *Server) browsable(p string) (string, error) {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return "", runnerwire.Refuse("the folder must be a clean absolute path")
	}
	inside := slices.ContainsFunc(s.roots, func(r string) bool {
		return p == r || strings.HasPrefix(p, strings.TrimSuffix(r, string(filepath.Separator))+string(filepath.Separator))
	})
	if !inside {
		return "", runnerwire.Refuse("that folder is not under a folder this runner shares")
	}
	if seg := runner.DeniedUserSegment(p); seg != "" {
		return "", runnerwire.Refuse("a folder inside %s is never shared: it holds credentials", seg)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", runnerwire.Refuse("that folder does not exist on this computer")
	}
	if real != p {
		return "", runnerwire.Refuse("that path goes through a symlink, which is never followed")
	}
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		return "", runnerwire.Refuse("that path is not a folder")
	}
	return p, nil
}

func (s *Server) list(ctx context.Context, args runnerwire.ListFoldersArgs) (runnerwire.FolderList, error) {
	if args.Path == "" {
		out := runnerwire.FolderList{Folders: []runnerwire.FolderEntry{}}
		for _, r := range s.roots {
			out.Folders = append(out.Folders, runnerwire.FolderEntry{Name: filepath.Base(r), Path: r, More: true})
		}
		return out, nil
	}
	dir, err := s.browsable(args.Path)
	if err != nil {
		return runnerwire.FolderList{}, err
	}
	if args.Survey && slices.Contains(s.roots, dir) {
		return runnerwire.FolderList{}, runnerwire.Refuse("a whole shared folder is never sent: choose a folder inside it")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return runnerwire.FolderList{}, runnerwire.Refuse("that folder cannot be read")
	}
	defer root.Close()
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

// count walks rel inside root, never through a symlink, for at most maxCounted
// entries and until deadline.
func count(root *os.Root, rel string, deadline time.Time) (files int, bytes int64, more bool) {
	seen := 0
	stop := errors.New("stop")
	err := fs.WalkDir(root.FS(), rel, func(_ string, d fs.DirEntry, err error) error {
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
	dir, err := s.folder(args.Path)
	if err != nil {
		return nil, err
	}
	remote, err := foldersync.RemoteDir(args.RemoteDir)
	if err != nil {
		return nil, runnerwire.Refuse("%s", err.Error())
	}
	stateFile := filepath.Join(s.stateDir, args.RunID.String()+".json")
	if err := foldersync.StateOutside(dir, stateFile); err != nil {
		return nil, runnerwire.Refuse("%s", err.Error())
	}
	st, err := foldersync.LoadState(stateFile, dir, remote)
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
	go func() {
		p.res = s.run(stream, dir, remote, st, stateFile, args)
		_ = stream.Close()
		release()
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

// run is one pass over stream, which the organisation joins to the sandbox's
// sftp-server started in remote.
func (s *Server) run(stream duplex, dir, remote string, st *foldersync.State, stateFile string, args runnerwire.SyncFolderArgs) runnerwire.FolderPass {
	failed := func(err error) runnerwire.FolderPass {
		return runnerwire.FolderPass{Refused: []runnerwire.FolderRefusal{}, Error: foldersync.Shown(err.Error())}
	}
	ctx, cancel := context.WithCancel(s.ctx)
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
	root, err := os.OpenRoot(dir)
	if err != nil {
		return failed(err)
	}
	defer root.Close()
	sy := &foldersync.Syncer{Client: c, Meter: meter, Root: root, State: st, StateFile: stateFile, Opts: foldersync.Options{
		Remote: remote, Pull: args.Pull, MaxPullBytes: foldersync.DefaultMaxPullBytes, OrgDeny: args.Deny,
		Credential: credentialPath,
	}}
	rep, err := sy.Pass(ctx)
	if err != nil {
		return failed(err)
	}
	out := runnerwire.FolderPass{Pushed: len(rep.Pushed), Pulled: len(rep.Pulled), Conflicts: len(rep.Conflicts),
		Ignored: rep.Ignored, BytesOut: rep.BytesOut, BytesIn: rep.BytesIn, Refused: []runnerwire.FolderRefusal{}}
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
