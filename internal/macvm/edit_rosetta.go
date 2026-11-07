// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// RosettaGuest is a window boot's guest that can install Rosetta when the person presses the edit window's
// Install Rosetta button. Rosetta is not installed unless they do.
type RosettaGuest interface {
	EditInstallRosetta(ctx context.Context) (guestagent.EditActionResult, error)
}

var _ RosettaGuest = (*windowGuest)(nil)

// EditInstallRosetta asks the guest agent to run Apple's Rosetta installer.
func (g *windowGuest) EditInstallRosetta(ctx context.Context) (guestagent.EditActionResult, error) {
	ctx, cancel := g.bound(ctx)
	defer cancel()
	return g.tools.ToolsEditInstallRosetta(ctx)
}

// wireRosetta gives text the Install Rosetta callback when the guest has the action.
func (b *editButtons) wireRosetta(text *WindowText) {
	rg, ok := b.g.(RosettaGuest)
	if !ok {
		return
	}
	text.InstallRosetta = b.act("Install Rosetta", rg.EditInstallRosetta, func(guestagent.EditActionResult) string {
		return "Rosetta is installed in the VM. Intel apps run in it now."
	})
}
