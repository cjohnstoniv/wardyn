// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package nodump keeps a process that holds credentials from writing them to
// disk in a core dump, or from having its memory read by another process of
// the same user (credential-storage design §2.8, CS-4). wardynd and
// wardyn-proxy call Disable before anything else.
package nodump

import (
	"fmt"
	"syscall"
)

// prSetDumpable is PR_SET_DUMPABLE from linux/prctl.h, which the syscall
// package does not export.
const prSetDumpable = 4

// Disable sets RLIMIT_CORE to zero and marks the process non-dumpable.
//
// The rlimit alone is not enough: when core_pattern pipes to a handler
// (systemd-coredump, apport) the kernel ignores it. A non-dumpable process
// gets no core at all (unless fs.suid_dumpable is 2, when the core goes to the
// handler readable by root only), and another process of the same uid can no
// longer ptrace it or read its /proc/<pid>/mem and /proc/<pid>/environ. A child
// it execs is dumpable again.
func Disable() error {
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{}); err != nil {
		return fmt.Errorf("set RLIMIT_CORE to 0: %w", err)
	}
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); errno != 0 {
		return fmt.Errorf("prctl(PR_SET_DUMPABLE, 0): %w", errno)
	}
	return nil
}
