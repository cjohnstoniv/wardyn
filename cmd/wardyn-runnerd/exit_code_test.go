// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// A SIGTERM cancels the command's context; that stop is not a failure.
func TestExitCode(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	live := context.Background()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		want int
	}{
		{"clean exit", live, nil, 0},
		{"cancelled by a signal", cancelled, fmt.Errorf("serve: %w", context.Canceled), 0},
		{"cancelled error without a cancelled context", live, context.Canceled, 1},
		{"another error", cancelled, errors.New("boom"), 1},
	} {
		if got := exitCode(tc.ctx, tc.err); got != tc.want {
			t.Errorf("%s: exitCode = %d, want %d", tc.name, got, tc.want)
		}
	}
}
