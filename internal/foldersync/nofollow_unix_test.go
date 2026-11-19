// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package foldersync

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A walk that starts at a FIFO returns at once: FS looks at the start without opening it, and opening
// a FIFO waits for a writer.
func TestFS_AWalkFromAFIFONeverWaitsForAWriter(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	fsys := FS(openTestRoot(t, dir))
	done := make(chan error, 1)
	go func() { done <- fs.WalkDir(fsys, "pipe", func(string, fs.DirEntry, error) error { return nil }) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		// A writer lets the stuck walk end.
		if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
		t.Fatal("a walk from a FIFO waited for a writer")
	}
}
