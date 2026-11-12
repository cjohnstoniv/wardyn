// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"errors"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// ListFolders asks a folder-only runner (Caps.Folders) for the folders in args.Path.
func (s *Substrate) ListFolders(ctx context.Context, args runnerwire.ListFoldersArgs) (runnerwire.FolderList, error) {
	var out runnerwire.FolderList
	err := s.call(ctx, runnerwire.MethodListFolders, args, &out)
	return out, err
}

// FolderPass is one sync_folder pass in progress: the runner speaks sftp, as the client, on
// Stream, and Wait returns its outcome once it has ended.
type FolderPass struct {
	Stream *runnerwire.Stream
	Wait   func(context.Context) (runnerwire.FolderPass, error)
}

// PassEnded is SyncFolder's answer for a pass that ended before the server took its stream: Pass is the
// runner's account of it, whose Error is the pass's own reason.
type PassEnded struct{ Pass runnerwire.FolderPass }

func (e *PassEnded) Error() string { return "the runner's pass ended before its stream was taken" }

// SyncFolder starts one pass on a folder-only runner. The caller joins Stream to the sandbox's
// sftp-server and closes it when either side ends.
func (s *Substrate) SyncFolder(ctx context.Context, args runnerwire.SyncFolderArgs) (FolderPass, error) {
	sess, err := s.pin()
	if err != nil {
		return FolderPass{}, err
	}
	var res runnerwire.StreamResult
	if err := sess.Call(ctx, runnerwire.MethodSyncFolder, args, &res); err != nil {
		return FolderPass{}, err
	}
	st, err := sess.TakeStream(res.Stream)
	if errors.Is(err, runnerwire.ErrNoStream) {
		// A pass that ended before the server took its stream has no stream left to take: the runner's
		// account of it is still there to ask for, and is its own reason.
		var out runnerwire.FolderPass
		if werr := sess.Call(ctx, runnerwire.MethodSyncFolderWait, runnerwire.SyncFolderWaitArgs{Stream: res.Stream}, &out); werr != nil {
			return FolderPass{}, fmt.Errorf("%w (asking for the pass's outcome failed: %v)", err, werr)
		}
		return FolderPass{}, &PassEnded{Pass: out}
	}
	if err != nil {
		return FolderPass{}, err
	}
	if st.Kind() != runnerwire.KindFolder {
		_ = st.Close()
		return FolderPass{}, fmt.Errorf("runner answered sync_folder with a %q stream", st.Kind())
	}
	return FolderPass{Stream: st, Wait: func(ctx context.Context) (runnerwire.FolderPass, error) {
		var out runnerwire.FolderPass
		err := sess.Call(ctx, runnerwire.MethodSyncFolderWait, runnerwire.SyncFolderWaitArgs{Stream: st.ID()}, &out)
		return out, err
	}}, nil
}
