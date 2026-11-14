// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"errors"
	"fmt"
	"testing"
)

func TestRefusedForNewerGeneration(t *testing.T) {
	refusal := fmt.Errorf("%w: it is for generation 3, this call is for 2", ErrProxyGenerationNewer)
	other := errors.New("docker: stop agent: engine unavailable")
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"nil":                      {nil, false},
		"the refusal":              {refusal, true},
		"the refusal, wrapped":     {fmt.Errorf("stop proxy: %w", refusal), true},
		"another error":            {other, false},
		"the refusal joined":       {errors.Join(refusal, other), false},
		"two errors in one wrap":   {fmt.Errorf("%w; %w", refusal, other), false},
		"another error wraps none": {fmt.Errorf("stop proxy: %w", other), false},
	} {
		if got := RefusedForNewerGeneration(tc.err); got != tc.want {
			t.Errorf("%s: RefusedForNewerGeneration = %v, want %v", name, got, tc.want)
		}
	}
}
