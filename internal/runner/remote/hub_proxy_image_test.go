// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A claimed runner's AUTH_OK carries the proxy image the organisation chose for that runner, and says
// nothing when no chooser is set.
func TestHubAuthOKCarriesTheProxyImage(t *testing.T) {
	eachTransport(t, func(t *testing.T, realWire bool) {
		for _, chooser := range []func(types.Runner) string{nil, func(rn types.Runner) string { return "org/proxy-" + rn.Arch }} {
			f := newInventoryFixture(t, realWire)
			f.row.Arch = "arm64"
			f.hub.cfg.ProxyImage = chooser
			want := ""
			if chooser != nil {
				want = "org/proxy-arm64"
			}
			if _, ok, _ := f.open(t, nil); ok.ProxyImage != want {
				t.Fatalf("AUTH_OK named %q, want %q", ok.ProxyImage, want)
			}
		}
	})
}
