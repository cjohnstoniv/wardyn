// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

// Package spoolowner keeps every file in a run's spool directory with the directory's owner. The
// directory has two writers that may run as different users: wardyn-runnerd, and the egress proxy as
// the user the directory was handed to (a root wardyn-runnerd on a rootful daemon hands it to the proxy
// image's non-root user). A file either of them creates there (each rewrite is a new file, renamed into
// place) must stay readable and writable by the other for the run's whole life.
package spoolowner

import (
	"errors"
	"os"
	"syscall"
)

// ProcessUID and Chown are this process's effective uid and the fchown of an open file. They are
// variables only so a test, which cannot be root, can stand in for another user.
var (
	ProcessUID = os.Geteuid
	Chown      = func(f *os.File, uid, gid int) error { return f.Chown(uid, gid) }
)

// Keep gives f, an open file in the directory root holds, to that directory's owner, and does nothing
// when this process already is that owner (every case but a root wardyn-runnerd beside a non-root
// proxy). It acts on the open file, never on a name, and changes no mode. A file with more than one
// link is refused and left as it is: a name that already existed could be a hard link to a file
// outside the spool, which must never be handed over.
func Keep(root *os.Root, f *os.File) error {
	d, err := root.Stat(".")
	if err != nil {
		return err
	}
	dir, ok := d.Sys().(*syscall.Stat_t)
	if !ok || int(dir.Uid) == ProcessUID() {
		return nil
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Nlink != 1 {
		return errors.New("spool file has more than one link; not handed to the spool's owner")
	}
	return Chown(f, int(dir.Uid), int(dir.Gid))
}
