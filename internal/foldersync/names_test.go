// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

import (
	"strings"
	"testing"
)

func TestSyncEntryName_RefusesControlCharacters(t *testing.T) {
	for _, n := range []string{"a\x1bb", "a\rb", "a\nb", "a\x07", "\x1b]52;c;ZXZpbA==\x07", "a\tb", "a\x7f", "a\u0085b"} {
		err := syncEntryName(n)
		if err == nil || !strings.Contains(err.Error(), "control character") {
			t.Errorf("syncEntryName(%q) = %v, want a control-character refusal", n, err)
		}
	}
	for _, n := range []string{"plain.txt", "a b", "héllo wörld.txt", "日本語", "emoji-\U0001F600"} {
		if err := syncEntryName(n); err != nil {
			t.Errorf("syncEntryName(%q) refused: %v", n, err)
		}
	}
}
