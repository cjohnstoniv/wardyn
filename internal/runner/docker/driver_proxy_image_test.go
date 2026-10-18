// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"testing"
)

// SetProxyImage moves every sidecar started after it, which is how a runner follows the organisation's
// image without a restart; without it, Config.ProxyImage is what runs.
func TestSetProxyImageMovesLaterSidecars(t *testing.T) {
	spec := testSpec()
	for _, set := range []string{"", "org/proxy:2"} {
		f := newFakeDocker()
		f.images["busybox:latest"] = true
		d := newTestDriver(f)
		want := d.cfg.ProxyImage
		if set != "" {
			d.SetProxyImage(set)
			want = set
		}
		if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
		if got := f.containers[proxyContainerName(spec.RunID)].cfg.Image; got != want {
			t.Errorf("SetProxyImage(%q): the proxy ran %q, want %q", set, got, want)
		}
	}
}
