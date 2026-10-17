// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

func TestCopyToDir_ErasureFencesDelayedCastAndLog(t *testing.T) {
	for _, ext := range []string{".cast", ".log"} {
		t.Run(ext, func(t *testing.T) {
			dst, srcDir := t.TempDir(), t.TempDir()
			s, err := recording.NewFSStore(dst)
			if err != nil {
				t.Fatal(err)
			}
			src := filepath.Join(srcDir, "run"+ext)
			if err := unix.Mkfifo(src, 0o600); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- copyToDir(src, dst) }()
			writer, err := os.OpenFile(src, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			if _, err := writer.WriteString("delayed recording"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DeleteRun(t.Context(), "run"); err != nil {
				t.Fatal(err)
			}
			_ = writer.Close()
			select {
			case err := <-done:
				if !errors.Is(err, recording.ErrErased) {
					t.Fatalf("late direct-file delivery = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("delivery did not finish")
			}
			if _, err := os.Stat(filepath.Join(dst, "run"+ext)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("late delivery recreated %s: %v", ext, err)
			}
			fresh := filepath.Join(srcDir, "new"+ext)
			if err := os.WriteFile(fresh, []byte("fresh"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := copyToDir(fresh, dst); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(filepath.Join(dst, "new"+ext)); err != nil || string(data) != "fresh" {
				t.Fatalf("fresh fallback basename/content = %q, %v", data, err)
			}
			if n, err := s.DeleteRun(t.Context(), "new"); err != nil || n != 1 {
				t.Fatalf("erase committed fallback = %d, %v", n, err)
			}
		})
	}
}
