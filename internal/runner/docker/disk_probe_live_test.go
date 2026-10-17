// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// TestDiskProbe_Live runs the disk probe on the engine at DOCKER_HOST. It needs a proxy image with
// -disk-free already on that engine: WARDYN_TEST_DOCKER=1 and WARDYN_TEST_PROXY_IMAGE.
func TestDiskProbe_Live(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the live disk probe test")
	}
	image := os.Getenv("WARDYN_TEST_PROXY_IMAGE")
	if image == "" {
		t.Fatal("set WARDYN_TEST_PROXY_IMAGE to a wardyn-proxy image with -disk-free present on this engine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	d, err := New(Config{ProxyImage: image})
	if err != nil {
		t.Fatal(err)
	}
	mib, err := d.ProbeDiskFreeMiB(ctx)
	if err != nil || mib <= 0 {
		t.Fatalf("ProbeDiskFreeMiB = %d, %v", mib, err)
	}
	t.Logf("engine reports %d MiB free; VM-backed: %v", mib, func() bool {
		info, ierr := d.cli.Info(ctx, client.InfoOptions{})
		return ierr == nil && VMBacked(info.Info, os.Getenv("DOCKER_HOST"))
	}())
	left, err := d.cli.(containerListerAPI).ContainerList(ctx, client.ContainerListOptions{All: true, Filters: client.Filters{}.Add("label", labelComponent+"="+componentDiskProbe)})
	if err != nil {
		t.Fatal(err)
	}
	if len(left.Items) != 0 {
		t.Errorf("%d disk-probe containers left behind", len(left.Items))
	}
}
