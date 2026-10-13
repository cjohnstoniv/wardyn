// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerspool

import (
	"os"
	"syscall"
)

// processUID and chownFile are this process's effective uid and the fchown of an open file, swapped in
// tests, which cannot be root.
var (
	processUID = os.Geteuid
	chownFile  = func(f *os.File, uid, gid int) error { return f.Chown(uid, gid) }
)

// own gives f, a file this process just created in the spool, to the spool directory's owner. The spool
// has two writers that may run as different users: wardyn-runnerd, and the proxy as the user the
// directory was handed to. A file either of them writes (each rewrite is a new file, renamed into place)
// must stay readable and writable by the other for the run's whole life, so every file in the spool
// keeps the directory's owner and its 0600 mode. It acts on the open file, never on a name, and does
// nothing when this process already is that owner.
func (s *Spool) own(f *os.File) error {
	d, err := s.root.Stat(".")
	if err != nil {
		return err
	}
	st, ok := d.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) == processUID() {
		return nil
	}
	return chownFile(f, int(st.Uid), int(st.Gid))
}
