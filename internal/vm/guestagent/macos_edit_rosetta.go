// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// The edit window's Install Rosetta action, served in tools mode beside the actions macos_edit_actions.go
// lists:
//
//	POST /edit/install-rosetta     run softwareupdate --install-rosetta --agree-to-license, as root
//
// It takes no input and answers EditActionResult with Installed set. Rosetta is installed only when the
// person presses the button: it is off by default, and the same argv stays refused as a tools exec
// (ToolsExecAllowed is unchanged). Apple's installer needs root and no console user.
const (
	rosettaTool = "/usr/sbin/softwareupdate"
	// rosettaWait bounds the install: the download and the installer.
	rosettaWait = 15 * time.Minute
)

// rosettaArgv is the one command the action runs.
var rosettaArgv = []string{rosettaTool, "--install-rosetta", "--agree-to-license"}

func (t *Tools) handleEditInstallRosetta(w http.ResponseWriter, r *http.Request) {
	run := t.run
	if run == nil {
		run = runCombined
	}
	ctx, cancel := context.WithTimeout(r.Context(), rosettaWait)
	defer cancel()
	out, code, err := run(ctx, rosettaArgv)
	if err != nil || code != 0 {
		slog.Warn("guest agent: tools mode: Rosetta was not installed", "exit", code, "err", err, "output", strings.TrimSpace(string(out)))
		http.Error(w, "Rosetta was not installed", http.StatusInternalServerError)
		return
	}
	writeJSONAny(w, EditActionResult{Installed: true})
}

// ToolsEditInstallRosetta asks the guest to install Rosetta.
func (c *Client) ToolsEditInstallRosetta(ctx context.Context) (EditActionResult, error) {
	return c.editAction(ctx, "/edit/install-rosetta")
}
