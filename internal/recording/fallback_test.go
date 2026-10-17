// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"golang.org/x/sys/unix"
)

func TestFSStore_FallbackPrivateStagingAndFinalFence(t *testing.T) {
	for _, ext := range []string{".cast", ".log"} {
		for _, finish := range []string{"save", "cancel", "erase"} {
			t.Run(ext+"/"+finish, func(t *testing.T) {
				root := t.TempDir()
				s, _ := recording.NewFSStore(root)
				name := "run" + ext
				path := filepath.Join(root, name)
				if err := os.WriteFile(path, []byte("original bytes"), 0o600); err != nil {
					t.Fatal(err)
				}
				before, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				r := &delayedCast{r: strings.NewReader("replacement"), read: make(chan struct{}), release: make(chan struct{})}
				release := sync.OnceFunc(func() { close(r.release) })
				defer release()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- s.SaveRecordingFile(ctx, name, r) }()
				select {
				case <-r.read:
				case <-time.After(5 * time.Second):
					t.Fatal("fallback source did not reach gated EOF")
				}
				assertPrivateStage(t, root)
				if data, err := os.ReadFile(path); err != nil || string(data) != "original bytes" {
					t.Fatalf("source stream modified destination: %q, %v", data, err)
				}
				var wantErr error
				switch finish {
				case "cancel":
					cancel()
					wantErr = context.Canceled
				case "erase":
					if n, err := s.DeleteRun(t.Context(), "run"); err != nil || n != 1 {
						t.Fatalf("erase during source stream = %d, %v", n, err)
					}
					wantErr = recording.ErrErased
				}
				release()
				if err := <-done; !errors.Is(err, wantErr) {
					t.Fatalf("fallback commit = %v, want %v", err, wantErr)
				}
				if finish == "erase" {
					if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("erased fallback survived: %v", err)
					}
				} else {
					after, err := os.Stat(path)
					if err != nil || !os.SameFile(before, after) || after.Mode() != before.Mode() {
						t.Fatalf("fallback changed inode/mode: %v, %v", after, err)
					}
					want := "replacement"
					if finish == "cancel" {
						want = "original bytes"
					}
					if data, err := os.ReadFile(path); err != nil || string(data) != want {
						t.Fatalf("fallback bytes = %q, %v", data, err)
					}
				}
				if paths, _ := filepath.Glob(filepath.Join(root, ".tmp-cast-*")); len(paths) != 0 {
					t.Fatalf("fallback leaked staging: %v", paths)
				}
			})
		}
	}
}

func assertPrivateStage(t *testing.T, root string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, ".tmp-cast-*"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("staging files = %v, %v", paths, err)
	}
	info, err := os.Stat(paths[0])
	if err != nil || info.Mode().Perm()&0o077 != 0 || info.Size() == 0 {
		t.Fatalf("nonempty staging file is not private: %v, %v", info, err)
	}
}

func TestFSStore_FallbackRefusesSpecialDestinations(t *testing.T) {
	for _, ext := range []string{".cast", ".log"} {
		for _, kind := range []string{"inside-symlink", "outside-symlink", "fifo", "directory"} {
			t.Run(ext+"/"+kind, func(t *testing.T) {
				root := t.TempDir()
				s, _ := recording.NewFSStore(root)
				path, target := filepath.Join(root, "run"+ext), filepath.Join(root, "private")
				if kind == "outside-symlink" {
					target = filepath.Join(t.TempDir(), "private")
				}
				if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "inside-symlink", "outside-symlink":
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				case "fifo":
					if err := unix.Mkfifo(path, 0o600); err != nil {
						t.Fatal(err)
					}
					f, err := os.OpenFile(path, os.O_RDWR|unix.O_NONBLOCK, 0)
					if err != nil {
						t.Fatal(err)
					}
					defer f.Close()
				case "directory":
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- s.SaveRecordingFile(ctx, "run"+ext, strings.NewReader("replacement")) }()
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("accepted a special destination")
					}
				case <-ctx.Done():
					t.Fatal("special destination pinned erasure lock")
				}
				if data, err := os.ReadFile(target); err != nil || string(data) != "untouched" {
					t.Fatalf("symlink target changed: %q, %v", data, err)
				}
				if _, err := s.DeleteRun(ctx, "run"); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestFSStore_SaveCastRetainsAtomicPrivateReplacement(t *testing.T) {
	root := t.TempDir()
	s, _ := recording.NewFSStore(root)
	path := filepath.Join(root, "run.cast")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCast(t.Context(), "run", strings.NewReader("new")); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || os.SameFile(before, after) || after.Mode().Perm() != 0o600 {
		t.Fatalf("SaveCast lost atomic private replacement: %v, %v", after, err)
	}
}
