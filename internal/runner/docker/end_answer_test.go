// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// A wipe warning alone is a finished end; a part of the end left undone, alone or beside the warning, is
// not, and the runner's own answer for it (runner.KeptDespiteAWSCacheWipe) passes it on.
func TestEndAnswerKeepsWhatTheEndLeftBesideAWipeWarning(t *testing.T) {
	wipe := fmt.Errorf("%w: exit status 3", runner.ErrAWSCacheWipeFailed)
	left := errors.New("docker: remove wardyn-vmfs-1: device busy")
	for name, tc := range map[string]struct {
		end, left error
		done      bool
	}{
		"neither":          {done: true},
		"the warning only": {end: wipe, done: true},
		"a removal only":   {left: left},
		"both":             {end: wipe, left: left},
	} {
		t.Run(name, func(t *testing.T) {
			got := endAnswer(tc.end, tc.left)
			if done := runner.KeptDespiteAWSCacheWipe(context.Background(), "ref", got) == nil; done != tc.done {
				t.Fatalf("endAnswer = %v, answered as done %v, want %v", got, done, tc.done)
			}
			if tc.left != nil && !errors.Is(got, tc.left) {
				t.Fatalf("endAnswer = %v, lost the removal failure", got)
			}
			if tc.end != nil && !strings.Contains(got.Error(), tc.end.Error()) {
				t.Fatalf("endAnswer = %v, lost the wipe warning's words", got)
			}
		})
	}
}
