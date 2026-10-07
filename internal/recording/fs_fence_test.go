// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestFSStore_ErasureSurvivesSweepAndRecreatedCast(t *testing.T) {
	root := t.TempDir()
	s, _ := NewFSStore(root)
	if _, err := s.DeleteRun(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	lock, err := os.Stat(filepath.Join(root, "run.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sweep(-time.Hour); err != nil {
		t.Fatal(err)
	}
	s, _ = NewFSStore(root)
	for _, key := range []string{"run", "run~attach", "run~part-2"} {
		if err := os.WriteFile(filepath.Join(root, key+".cast"), []byte("external writer"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.OpenCast(t.Context(), key); !errors.Is(err, ErrErased) {
			t.Fatalf("recreated OpenCast = %v", err)
		}
		if _, _, err := s.StatAndTail(t.Context(), key, 32); !errors.Is(err, ErrErased) {
			t.Fatalf("recreated StatAndTail = %v", err)
		}
		if err := s.SaveCast(t.Context(), key, strings.NewReader("late")); !errors.Is(err, ErrErased) {
			t.Fatalf("save after sweep/restart = %v", err)
		}
	}
	if n, err := s.DeleteRun(t.Context(), "run"); err != nil || n != 3 {
		t.Fatalf("repeat erasure = %d, %v", n, err)
	}
	after, err := os.Stat(filepath.Join(root, "run.lock"))
	if err != nil || !os.SameFile(lock, after) {
		t.Fatalf("erasure/sweep replaced the lock inode: %v", err)
	}
}

func TestFSStore_FencePathsRefuseSymlinks(t *testing.T) {
	for _, path := range []string{".erased", "run.lock", ".erased/run.cast"} {
		t.Run(path, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			s, _ := NewFSStore(root)
			target := outside
			if path != ".erased" {
				if strings.Contains(path, "/") {
					if err := os.Mkdir(filepath.Join(root, filepath.Dir(path)), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				target = filepath.Join(outside, "untouched")
				if err := os.WriteFile(target, []byte("private"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, filepath.Join(root, path)); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveCast(t.Context(), "run", strings.NewReader("cast")); err == nil {
				t.Fatal("save followed a fence symlink")
			}
			if _, err := s.DeleteRun(t.Context(), "run"); err == nil {
				t.Fatal("erase followed a fence symlink")
			}
			if _, err := s.OpenCast(t.Context(), "run"); err == nil {
				t.Fatal("read ignored a fence symlink")
			}
			entries, _ := os.ReadDir(outside)
			if path != ".erased" {
				data, _ := os.ReadFile(target)
				if string(data) != "private" || len(entries) != 1 {
					t.Fatal("outside file changed")
				}
			} else if len(entries) != 0 {
				t.Fatal("fence files escaped root")
			}
		})
	}
}

func TestFSStore_FenceLockCancellation(t *testing.T) {
	rootPath := t.TempDir()
	s, _ := NewFSStore(rootPath)
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lock, err := lockFSRun(t.Context(), root, "run")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	for _, action := range []string{"save", "erase", "open", "stat"} {
		t.Run(action, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			var err error
			switch action {
			case "save":
				err = s.SaveCast(ctx, "run~part-2", strings.NewReader("late"))
			case "erase":
				_, err = s.DeleteRun(ctx, "run")
			case "open":
				_, err = s.OpenCast(ctx, "run")
			case "stat":
				_, _, err = s.StatAndTail(ctx, "run", 32)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cancelled %s = %v", action, err)
			}
		})
	}
	if err := s.SaveCast(t.Context(), "distinct", strings.NewReader("new")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCast(t.Context(), "run", strings.NewReader("after cancellation")); err != nil {
		t.Fatal(err)
	}
	entries, _ := filepath.Glob(filepath.Join(rootPath, ".tmp-cast-*"))
	if len(entries) != 0 {
		t.Fatalf("cancelled write leaked temp files: %v", entries)
	}
}

func TestFSStore_FIFODoesNotPinErasure(t *testing.T) {
	root := t.TempDir()
	s, _ := NewFSStore(root)
	if err := unix.Mkfifo(filepath.Join(root, "run.cast"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := s.OpenCast(ctx, "run"); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("opened FIFO as a recording")
		}
	case <-ctx.Done():
		t.Fatal("FIFO pinned the run lock")
	}
	if _, err := s.DeleteRun(ctx, "run"); err != nil {
		t.Fatal(err)
	}
}
