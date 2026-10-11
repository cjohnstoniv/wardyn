// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/pkg/sftp"
)

func TestSyncPushDoesNotTraverseRefusedSymlinkParent(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	target := t.TempDir()
	putSyncFile(t, filepath.Join(target, "child.txt"), "untouched", 0o644)
	if err := os.Symlink(target, filepath.Join(box, "linked")); err != nil {
		t.Fatal(err)
	}
	sy, _, root := g.syncer(syncRemote, false)
	putSyncFile(t, filepath.Join(root, "linked/child.txt"), "from laptop", 0o644)
	putSyncFile(t, filepath.Join(root, "fine.txt"), "fine", 0o644)
	rep := mustPass(t, sy)
	if got := getSyncFile(t, filepath.Join(target, "child.txt")); got != "untouched" {
		t.Errorf("push traversed a refused remote symlink: target = %q; report = %+v", got, rep)
	}
	if !slices.Equal(rep.Pushed, []string{"fine.txt"}) {
		t.Errorf("pushed = %v", rep.Pushed)
	}
}

func TestSyncConnectionStatusAborts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		abort bool
	}{
		{"no connection", &sftp.StatusError{Code: uint32(sftp.ErrSSHFxNoConnection)}, true},
		{"connection lost", &sftp.StatusError{Code: uint32(sftp.ErrSSHFxConnectionLost)}, true},
		{"local connection loss", sftp.ErrSSHFxConnectionLost, true},
		{"permission", &sftp.StatusError{Code: uint32(sftp.ErrSSHFxPermissionDenied)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := syncAbort(context.Background(), tc.err); got != tc.abort {
				t.Fatalf("syncAbort(%v) = %v, want %v", tc.err, got, tc.abort)
			}
		})
	}
}
