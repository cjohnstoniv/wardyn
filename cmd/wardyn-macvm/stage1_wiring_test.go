// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/stage1"
)

// TestPrepareDepsWiresStage1: prepare's stage 1 is the SSH bootstrapper,
// installing the wardyn-guest-agent beside this helper.
func TestPrepareDepsWiresStage1(t *testing.T) {
	var o macvm.PrepareOptions
	_ = prepareDeps(&o) // still refused: no prep network here
	if _, ok := o.Bootstrap.(*stage1.Bootstrapper); !ok {
		t.Fatalf("Bootstrap = %T; want *stage1.Bootstrapper", o.Bootstrap)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if got := o.Bootstrap.(*stage1.Bootstrapper).AgentPath(); got != filepath.Join(filepath.Dir(self), "wardyn-guest-agent") {
		t.Fatalf("agent path %q", got)
	}
}
