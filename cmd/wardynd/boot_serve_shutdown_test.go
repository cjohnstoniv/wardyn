// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
	"testing"
)

// TestServeShutdownOrder pins the shutdown sequence the platform grace period
// is sized for (internal/api TestShutdownGraceCoversTheBudget): stop accepting
// requests, then wait for the detached work handlers left behind, then flush.
// Without the WaitBackground call a SIGTERM drops a superseded sign-in's
// teardown and a run launch in flight; moved after the flush, their audit rows
// land on closed sinks.
func TestServeShutdownOrder(t *testing.T) {
	b, err := os.ReadFile("boot_serve.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	prev := -1
	for _, call := range []string{
		"httpSrv.Shutdown(shutCtx)",
		"srv.WaitBackground()",
		"srv.FlushAuthFailedStreak()",
	} {
		i := strings.Index(src, call)
		if i < 0 {
			t.Fatalf("boot_serve.go no longer calls %s", call)
		}
		if i < prev {
			t.Fatalf("boot_serve.go calls %s before the step that must precede it", call)
		}
		prev = i
	}
}
