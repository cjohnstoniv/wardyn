// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm/basesettings"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// BaseSettingsGuest is a tools guest that can have the guest agent apply the base settings
// (internal/macvm/basesettings): the stage-2 boot's guest. A guest without it is left alone.
type BaseSettingsGuest interface {
	// BaseSettings applies every row with scope "", or only Terminal's kill and saved-state removal with
	// guestagent.BaseSettingsScopeTerminal. The route takes no other input.
	BaseSettings(ctx context.Context, scope string) (guestagent.BaseSettingsResult, error)
}

var _ BaseSettingsGuest = (*toolsGuest)(nil)

// baseSettingsDeadline bounds the route: it runs a few dozen fixed commands.
var baseSettingsDeadline = 10 * time.Minute

// BaseSettingsOldAgent is what tools.json says when the base's agent has no base-settings route: the old
// agent answers the first update after an upgrade, and the next update applies the rows.
const BaseSettingsOldAgent = "agent predates this setting; run `wardyn-runnerd macos-vm update-tools` again"

func (g *toolsGuest) BaseSettings(ctx context.Context, scope string) (guestagent.BaseSettingsResult, error) {
	ctx, done := g.bound(ctx)
	defer done()
	ctx, cancel := context.WithTimeout(ctx, baseSettingsDeadline)
	defer cancel()
	return g.tools.ToolsBaseSettings(ctx, scope)
}

// applyBaseSettings has the guest agent apply the table and records what it skipped in res. A route that
// fails, or an agent that has none, is a skipped entry too: base settings never fail a tools update.
func applyBaseSettings(ctx context.Context, g ToolsGuest, res *ToolsResult, log *slog.Logger) {
	bg, ok := g.(BaseSettingsGuest)
	if !ok {
		return
	}
	r, err := bg.BaseSettings(ctx, "")
	switch {
	case errors.Is(err, guestagent.ErrNoExec):
		res.BaseSettingsSkipped = []basesettings.Skipped{{Name: "base-settings", Reason: BaseSettingsOldAgent}}
	case err != nil:
		res.BaseSettingsSkipped = []basesettings.Skipped{{Name: "base-settings", Reason: "the guest agent could not apply the base settings: " + err.Error()}}
	default:
		res.BaseSettingsSkipped = r.Skipped
	}
	if log != nil && len(res.BaseSettingsSkipped) > 0 {
		log.Warn("macvm: base settings skipped", "count", len(res.BaseSettingsSkipped), "first", res.BaseSettingsSkipped[0].Reason)
	}
}

// cleanTerminal kills Terminal and removes its saved state in the guest, as edit-base's finish does: the
// admin Terminal the person opened there is where restored windows come from. It never fails the edit.
func cleanTerminal(ctx context.Context, g ToolsGuest, log *slog.Logger) {
	bg, ok := g.(BaseSettingsGuest)
	if !ok {
		return
	}
	r, err := bg.BaseSettings(ctx, guestagent.BaseSettingsScopeTerminal)
	if log == nil {
		return
	}
	switch {
	case errors.Is(err, guestagent.ErrNoExec):
		log.Warn("macvm: Terminal's saved state was not cleaned", "reason", BaseSettingsOldAgent)
	case err != nil:
		log.Warn("macvm: Terminal's saved state was not cleaned", "err", err.Error())
	case len(r.Skipped) > 0:
		log.Warn("macvm: Terminal's saved state was not cleaned", "reason", r.Skipped[0].Reason)
	}
}
