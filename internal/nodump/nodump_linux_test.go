// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package nodump

import (
	"syscall"
	"testing"
)

// prGetDumpable is PR_GET_DUMPABLE from linux/prctl.h.
const prGetDumpable = 3

// A process holding credentials must never leave them in a core file or let a
// same-uid process read its memory: after Disable, both the core limit and the
// dumpable flag are zero.
func TestDisable_NoCoreDumpAndNotDumpable(t *testing.T) {
	if err := Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_CORE, &lim); err != nil {
		t.Fatalf("getrlimit: %v", err)
	}
	if lim.Cur != 0 || lim.Max != 0 {
		t.Errorf("RLIMIT_CORE = %d/%d after Disable, want 0/0", lim.Cur, lim.Max)
	}
	dumpable, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prGetDumpable, 0, 0)
	if errno != 0 {
		t.Fatalf("prctl(PR_GET_DUMPABLE): %v", errno)
	}
	if dumpable != 0 {
		t.Errorf("PR_GET_DUMPABLE = %d after Disable, want 0", dumpable)
	}
}
