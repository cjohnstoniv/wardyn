// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
)

// kekLocalRowsCheck is the kek_local_rows row: WARDYN_KEK_REQUIRED makes new writes use the key service
// but does not move the stored credential rows and principal keys already wrapped by the local key. Absent at zero; a warning, never blocking.
func kekLocalRowsCheck(n int) (SetupCheck, bool) {
	if n <= 0 {
		return SetupCheck{}, false
	}
	return SetupCheck{
		ID: "kek_local_rows", Label: "Credential key custody", Status: "warn",
		Detail: fmt.Sprintf("%d stored credential rows and principal keys are still wrapped by the local key. WARDYN_KEK_REQUIRED covers new writes only; it does not move existing ones.", n),
		Fix:    "Run `wardynd -rewrap` to move them onto the key service.",
	}, true
}

// kekLocalRows counts the rows still wrapped by the local key when the flag is set and a key service
// wraps new writes, for an operator (handleSetupStatus redacts every check for anyone else). A failed
// count leaves the row out rather than showing a guess.
func (s *Server) kekLocalRows(ctx context.Context) []SetupCheck {
	if !s.cfg.KEKRequired || s.cfg.SecretKeyService == "" || s.cfg.SecretStoreExternal != "" || s.cfg.SecretLocalRows == nil || !s.isOperator(ctx) {
		return nil
	}
	n, err := s.cfg.SecretLocalRows(ctx)
	if err != nil {
		return nil
	}
	if chk, ok := kekLocalRowsCheck(n); ok {
		return []SetupCheck{chk}
	}
	return nil
}
