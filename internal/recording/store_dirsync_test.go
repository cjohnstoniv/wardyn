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
