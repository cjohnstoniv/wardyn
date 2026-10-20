// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A FIFO at the configured path is refused as not a regular file, promptly: a blocking open would hold the
// runner's startup for as long as nothing wrote to it.
func TestReadOrgCARefusesAFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skip(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := readOrgCA(path, os.Geteuid(), time.Now(), func(string) {})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("err=%v; want a refusal saying not a regular file", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readOrgCA blocked on a FIFO")
	}
}
