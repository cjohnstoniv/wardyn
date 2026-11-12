// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire_test

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// The organisation tells the helper's window refusals by sentence (the runner's reply carries no code), so
// the sentences the wire holds must be the helper's own.
func TestMacWindowSentencesAreTheHelpers(t *testing.T) {
	for name, pair := range map[string][2]string{
		"already open": {runnerwire.MacWindowOpenSentence, macvm.WindowAlreadyOpenSentence},
		"not at mac":   {runnerwire.MacWindowNotAtMacSentence, macvm.WindowNotAtMacSentence},
		"unreadable":   {runnerwire.MacWindowUnreadableSentence, macvm.WindowUnreadableSentence},
		"lock unknown": {runnerwire.MacWindowLockUnknownSentence, macvm.WindowLockUnknownSentence},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s: runnerwire says %q, the helper says %q", name, pair[0], pair[1])
		}
	}
}
