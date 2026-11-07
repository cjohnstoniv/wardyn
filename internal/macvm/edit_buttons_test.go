// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// layoutStable is the edit code's alphabet: the 20 lowercase letters on the same key on QWERTY, QWERTZ and
// AZERTY keyboards (AZERTY swaps a/q and w/z and moves m; QWERTZ swaps y/z), with no digit and no shift.
const layoutStable = "bcdefghijklnoprstuvx"

// The edit's one-time code is 10 letters that sit on the same key on QWERTY, QWERTZ and AZERTY, so Type code
// types it right on any of them; the generator draws every one of the 20 and nothing else.
func TestTypableCodeUsesLayoutStableLetters(t *testing.T) {
	e := newEdit(t)
	if _, err := macvm.EditBase(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	code := e.codeSeen.Code
	if len(code) != 10 || strings.Trim(code, layoutStable) != "" {
		t.Errorf("the edit's code %q: want 10 letters of %q", code, layoutStable)
	}
	seen := map[byte]bool{}
	for range 200 {
		c, err := macvm.TypableCode(10)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range c {
			if strings.IndexByte(layoutStable, b) < 0 {
				t.Fatalf("typableCode drew %q, which moves between keyboard layouts or needs shift", b)
			}
			seen[b] = true
		}
	}
	if len(seen) != len(layoutStable) {
		t.Errorf("2000 draws used %d letters, want all %d of %q", len(seen), len(layoutStable), layoutStable)
	}
}
