// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvmtest

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// SockRoot makes an installed socket root (root's sticky 1777 sock/, as the
// runner package's postinstall makes it) under a short temp dir: a macOS
// TMPDIR would put the sockets past the 103-byte limit.
func SockRoot(t testing.TB) string {
	t.Helper()
	// /tmp's real path (/private/tmp on macOS): the ledger walks every directory without following a link.
	root, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	d, err := os.MkdirTemp(root, "wm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	dir := filepath.Join(d, "sock")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}
	return dir
}

// SockFstat stands in for root ownership in the socket directory checks:
// everything reads as root's except a login's own u<uid>/ under a sock/ dir
// and what is inside it, which keep their real owner.
func SockFstat(fd int, name string) (unix.Stat_t, error) {
	var st unix.Stat_t
	err := unix.Fstat(fd, &st)
	if !strings.Contains(name+"/", "/sock/u"+strconv.Itoa(os.Getuid())+"/") {
		st.Uid, st.Gid = 0, 0
	}
	return st, err
}
