// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"strings"
)

// fdesetupPath is the tool posture runs, with fixed argv only, to read FileVault.
const fdesetupPath = "/usr/bin/fdesetup"

// ToolsFileVault values: ToolsPosture.FileVault is one of them, or "" when the read did not say.
const (
	FileVaultOn  = "on"
	FileVaultOff = "off"
)

// fileVault is whether FileVault is on in the guest, read as `fdesetup status` prints it ("FileVault is
// On." or "FileVault is Off." on its first line). Any other output, an error or a nonzero exit is "": the
// host reads that as unknown, never as off. It changes nothing.
func (t *Tools) fileVault(ctx context.Context) string {
	run := t.run
	if run == nil {
		run = runCombined
	}
	out, code, err := run(ctx, []string{fdesetupPath, "status"})
	if err != nil || code != 0 {
		return ""
	}
	return parseFileVault(string(out))
}

func parseFileVault(out string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	switch strings.TrimSpace(first) {
	case "FileVault is On.":
		return FileVaultOn
	case "FileVault is Off.":
		return FileVaultOff
	}
	return ""
}
