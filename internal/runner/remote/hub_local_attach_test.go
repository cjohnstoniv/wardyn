// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A claimed runner's AUTH_OK says local_attach exactly when the organisation's chooser says so for that
// runner, and says nothing when no chooser is set.
func TestHubAuthOKCarriesLocalAttach(t *testing.T) {
	eachTransport(t, func(t *testing.T, realWire bool) {
		for _, c := range []struct {
			name    string
			chooser func(types.Runner) bool
			want    bool
		}{
			{"no chooser", nil, false},
			{"chooser off", func(types.Runner) bool { return false }, false},
			{"chooser on", func(types.Runner) bool { return true }, true},
		} {
			f := newInventoryFixture(t, realWire)
			f.hub.cfg.LocalAttach = c.chooser
			if _, ok, _ := f.open(t, nil); ok.LocalAttach != c.want {
				t.Fatalf("%s: AUTH_OK local_attach = %v, want %v", c.name, ok.LocalAttach, c.want)
			}
		}
	})
}
