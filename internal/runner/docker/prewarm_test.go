// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"testing"
	"time"
)

// TestPrewarmImagesPullsTheDriveProbeImageBesideTheProxy (#721): ProbeDrive
// never pulls — it errors when its image is absent, and the API reads a probe
// error as "unknown" and fails open. The boot-time prewarm is therefore the only
// thing that makes the first host_path readability check real, so dropping the
// probe image from it would silently turn every probe on a fresh daemon into a
// pass.
func TestPrewarmImagesPullsTheDriveProbeImageBesideTheProxy(t *testing.T) {
	f := newFakeDocker()
	pulled := make(chan string, 8)
	f.onPull = func(ref string) { pulled <- ref }
	d := newTestDriver(f)

	d.PrewarmImages()

	want := map[string]bool{d.cfg.ProxyImage: false, defaultDriveProbeImage: false}
	deadline := time.After(10 * time.Second)
	for seen := 0; seen < len(want); {
		select {
		case ref := <-pulled:
			if done, ok := want[ref]; ok && !done {
				want[ref] = true
				seen++
			}
		case <-deadline:
			t.Fatalf("after 10s PrewarmImages had pulled %v (true = pulled); want the proxy image AND the drive-probe image", want)
		}
	}
}
