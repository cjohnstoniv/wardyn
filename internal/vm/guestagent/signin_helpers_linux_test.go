// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"testing"

	"golang.org/x/sys/unix"
)

func syscallKillGroup(pid int) error { return unix.Kill(-pid, unix.SIGKILL) }

func unmount(p string) error { return unix.Unmount(p, unix.MNT_DETACH) }

func isTmpfs(t *testing.T, p string) bool {
	t.Helper()
	var st unix.Statfs_t
	if err := unix.Statfs(p, &st); err != nil {
		t.Fatal(err)
	}
	return st.Type == unix.TMPFS_MAGIC
}
