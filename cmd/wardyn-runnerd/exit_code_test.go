// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
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
		{"contain: a run not stopped", live, containExit{code: 3}, 3},
		{"contain: no serve", live, containExit{code: 4}, 4},
		{"contain: no receipt", live, containExit{code: 5}, 5},
		{"contain: a run not stopped, wrapped", live, fmt.Errorf("contain: %w", containExit{code: 3}), 3},
		{"contain: no serve, wrapped", live, fmt.Errorf("contain: %w", containExit{code: 4}), 4},
		{"contain: no receipt, wrapped", live, fmt.Errorf("contain: %w", containExit{code: 5}), 5},
	} {
		if got := exitCode(tc.ctx, tc.err); got != tc.want {
			t.Errorf("%s: exitCode = %d, want %d", tc.name, got, tc.want)
		}
	}
	// A child process's own exit status is not a contain code: wrapped as setup's launchctl call wraps it, it is 1.
	child := exec.Command("sh", "-c", "exit 5").Run()
	if !errors.As(child, new(*exec.ExitError)) {
		t.Fatalf("sh -c 'exit 5' = %v, want an *exec.ExitError", child)
	}
	if got := exitCode(live, fmt.Errorf("launchctl bootstrap: %w: %s", child, "output")); got != 1 {
		t.Fatalf("a wrapped child exit status 5: exitCode = %d, want 1", got)
	}
}
