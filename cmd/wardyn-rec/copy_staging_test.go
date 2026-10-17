// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func copyUIDPrivateStaging(t *testing.T, owner uint32, ext string) {
	t.Helper()
	root := copyUIDRoot(t)
	src, dst := filepath.Join(root, "run"+ext), filepath.Join(root, "out", "run"+ext)
	writeCopyFixture(t, dst, "private original bytes", 0o600)
	if err := os.Chown(dst, int(owner), 2222); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(src, 0o644); err != nil {
		t.Fatal(err)
	}
	writer, err := os.OpenFile(src, os.O_RDWR|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	cmd := copyUIDCommand(t, owner, "allow", src, filepath.Dir(dst), "022")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	if _, err := writer.WriteString("private incoming bytes"); err != nil {
		t.Fatal(err)
	}
	staged := waitForCopyStaging(t, filepath.Dir(dst))
	runCopyUID(t, copyUIDCommand(t, otherCopyUID(owner), "private", staged, "", "022"))
	assertCopyDestination(t, dst, before, "private original bytes")
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("staged writer: %v\n%s", err, output.String())
	}
	assertCopyDestination(t, dst, before, "private incoming bytes")
}

func waitForCopyStaging(t *testing.T, dir string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		paths, err := filepath.Glob(filepath.Join(dir, ".tmp-cast-*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(paths) == 1 {
			info, err := os.Stat(paths[0])
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("staging exposed before final commit: mode=%04o", info.Mode().Perm())
			}
			if info.Size() > 0 {
				return paths[0]
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("recorder did not stage its stream")
	return ""
}
