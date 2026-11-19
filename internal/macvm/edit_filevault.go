// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// warnFileVault prints a warning when the edit's recheck read FileVault as on in the guest. It is only a
// warning: the base is kept, and the person is told to turn it off. "off" and a read that did not say print
// nothing.
func (o *EditOptions) warnFileVault(state string) {
	if w := FileVaultWarning(state); w != "" {
		fmt.Fprintln(o.Out, w)
	}
}

// FileVaultWarning is the edit window's live-line words for a guest that has FileVault on, and "" for any
// other state: the base keeps FileVault off.
func FileVaultWarning(state string) string {
	if state != guestagent.FileVaultOn {
		return ""
	}
	return fileVaultWarningText
}

// fileVaultWarningText is the warning wherever FileVault reads on in the guest.
const fileVaultWarningText = "WARN: FileVault is on in the VM. Turn it off in System Settings > Privacy & Security."
