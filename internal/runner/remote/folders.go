// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
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
