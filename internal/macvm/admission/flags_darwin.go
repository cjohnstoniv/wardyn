// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package admission

import "golang.org/x/sys/unix"

// statFlags is st's BSD file flags (st_flags).
func statFlags(st unix.Stat_t) uint32 { return st.Flags }

// fchflags is fchflags(2) on fd.
func fchflags(fd int, flags uint32) error { return unix.Fchflags(fd, int(flags)) }
