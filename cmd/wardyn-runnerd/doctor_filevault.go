// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"github.com/cjohnstoniv/wardyn/internal/doctor"
	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// fileVaultStage warns when the base last read FileVault as on in the guest. It is never a failure, and it
// says nothing when FileVault reads off or was not read.
func fileVaultStage(state string) (doctor.Stage, bool) {
	w := macvm.FileVaultWarning(state)
	if w == "" {
		return doctor.Stage{}, false
	}
	return doctor.SkipStage("file-vault", w+" (as the base last read it)"), true
}
