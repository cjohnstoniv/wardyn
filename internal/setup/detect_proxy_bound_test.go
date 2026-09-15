// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestExecCommandOutput_BoundedWhenAGrandchildHoldsThePipe pins probeWaitDelay.
//
// exec.CommandContext's kill on ctx expiry reaches only the DIRECT child, while
// Output() waits for EOF on the stdout pipe — so a grandchild that inherited
// that pipe keeps the call alive long past probeTimeout. That is the WSL interop
// wedge in one line, and it measured 30s against a 3s context before WaitDelay
// was set: the reason a GET /setup/status once took 39.79s on this box, and the
// reason "each probe is bounded by its own 3s timeout" was never true.
//
// The script holds stdout open in a BACKGROUNDED subshell (the grandchild) for
// far longer than the direct child lives, which is exactly the shape that used
// to hang.
func TestExecCommandOutput_BoundedWhenAGrandchildHoldsThePipe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell script; the wedge this pins is WSL-interop-shaped anyway")
	}
	script := filepath.Join(t.TempDir(), "wedge.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n(sleep 20) &\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatalf("write probe script: %v", err)
	}

	start := time.Now()
	if _, err := execCommandOutput(script); err == nil {
		t.Error("a killed probe must report an error, not a usable empty stdout")
	}
	// probeTimeout + probeWaitDelay, plus slack for a loaded box. The point is
	// "bounded at all" — the pre-WaitDelay behaviour was the child's full 30s.
	if bound := probeTimeout + probeWaitDelay + 4*time.Second; time.Since(start) > bound {
		t.Errorf("probe took %s, want under %s — Output() is still waiting on a pipe the kill did not close",
			time.Since(start), bound)
	}
}
