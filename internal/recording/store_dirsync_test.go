// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
)

// A filesystem that cannot fsync a directory (FUSE, 9p, older CIFS) answers EINVAL or ENOTSUP after the
// rename has already stored the cast; that is not a failed save. Any other directory-sync error still is.
func TestFSStore_SaveCastIgnoresDirectorySyncThatIsNotSupported(t *testing.T) {
	const runID = "11111111-1111-4111-8111-111111111111"
	orig := syncDir
	t.Cleanup(func() { syncDir = orig })
	for _, tc := range []struct {
		name    string
		err     error
		wantErr bool
	}{
		{"EINVAL", &os.PathError{Op: "sync", Path: ".", Err: syscall.EINVAL}, false},
		{"ENOTSUP", fmt.Errorf("sync: %w", syscall.ENOTSUP), false},
		{"EIO", &os.PathError{Op: "sync", Path: ".", Err: syscall.EIO}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			syncDir = func(*os.File) error { return tc.err }
			s, err := NewFSStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			err = s.SaveCast(context.Background(), runID, strings.NewReader("data"))
			if (err != nil) != tc.wantErr || (err != nil && !errors.Is(err, tc.err)) {
				t.Fatalf("SaveCast error = %v, want error=%v", err, tc.wantErr)
			}
			if !tc.wantErr {
				rc, err := s.OpenCast(context.Background(), runID)
				if err != nil {
					t.Fatalf("the stored cast cannot be read back: %v", err)
				}
				rc.Close()
			}
		})
	}
}

// The in-place copy of wardyn-rec's shared-volume file is acknowledged like a cast, so its bytes are
// fsynced before SaveRecordingFile returns, and a failed fsync is a failed save.
func TestFSStore_SaveRecordingFileSyncsTheFile(t *testing.T) {
	const name = "11111111-1111-4111-8111-111111111111.log"
	orig := syncFile
	t.Cleanup(func() { syncFile = orig })
	synced := 0
	syncFile = func(f *os.File) error { synced++; return orig(f) }
	s, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRecordingFile(context.Background(), name, strings.NewReader("data")); err != nil {
		t.Fatal(err)
	}
	if synced != 1 {
		t.Errorf("the file was fsynced %d times, want once", synced)
	}
	boom := errors.New("fsync failed")
	syncFile = func(*os.File) error { return boom }
	if err := s.SaveRecordingFile(context.Background(), name, strings.NewReader("more")); !errors.Is(err, boom) {
		t.Errorf("a failed fsync returned %v, want it", err)
	}
}
