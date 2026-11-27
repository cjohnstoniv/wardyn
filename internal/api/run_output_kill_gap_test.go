// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestKillOutputFinishIsDeterministic: a killed run with no output row, on a process that holds no tail for it,
// gets the same capture-gap row whether the output finish reads the run before or after the kill clears its
// sandbox ref: the finish decides from the ref the kill saw.
func TestKillOutputFinishIsDeterministic(t *testing.T) {
	for name, cleared := range map[string]bool{"the finish reads first": false, "the clear lands first": true} {
		t.Run(name, func(t *testing.T) {
			f := newOutputFixture(t)
			f.st.mu.Lock()
			f.st.state = types.RunKilled
			if cleared {
				f.st.run.SandboxRef = ""
			}
			f.st.mu.Unlock()
			f.srv.finishRunOutputDetached(t.Context(), f.run.ID, "sbx-out")
			f.srv.WaitBackground()
			if row := f.finalRow(t); !row.CaptureGap {
				t.Fatalf("row %+v, want a capture gap", row)
			}
			if got := f.gapReason(t); got != "no_tail" {
				t.Errorf("gap reason %q, want no_tail", got)
			}
		})
	}
}
