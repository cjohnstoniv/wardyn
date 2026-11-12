// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build e2etmux && !docker && linux

package localtmux

import (
	"context"
	"errors"
	"testing"
)

// A stop whose context is already done ran no exec: it says so, instead of the nil that reads as stopped.
func TestLocaltmux_StopOnCancelledCtxReportsIt(t *testing.T) {
	s := &Substrate{tmux: "/nonexistent/tmux", socket: "unused"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.StopSandbox(ctx, "ref"); !errors.Is(err, context.Canceled) {
		t.Errorf("StopSandbox on a cancelled context = %v; want context.Canceled", err)
	}
	if err := s.KillSandbox(ctx, "ref"); !errors.Is(err, context.Canceled) {
		t.Errorf("KillSandbox on a cancelled context = %v; want context.Canceled", err)
	}
	if err := s.StopSandbox(context.Background(), "ref"); err != nil {
		t.Errorf("StopSandbox with a live context = %v; want nil (a missing server is already stopped)", err)
	}
}
