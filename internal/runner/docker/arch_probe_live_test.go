// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/moby/moby/client"
)

// TestArchProbe_Live runs the arch probe against a real engine. It needs two local images of the
// OTHER architecture than the engine's, each a single manifest (so it inspects with an
// architecture): one that runs under emulation, and one whose binary the engine cannot exec.
// Set WARDYN_TEST_DOCKER=1, WARDYN_TEST_ARCH_EMULATED and WARDYN_TEST_ARCH_UNRUNNABLE.
func TestArchProbe_Live(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the live arch probe test")
	}
	emulated, unrunnable := os.Getenv("WARDYN_TEST_ARCH_EMULATED"), os.Getenv("WARDYN_TEST_ARCH_UNRUNNABLE")
	if emulated == "" || unrunnable == "" {
		t.Skip("set WARDYN_TEST_ARCH_EMULATED and WARDYN_TEST_ARCH_UNRUNNABLE (single-manifest images of the other architecture)")
	}
	ctx := context.Background()
	// A driver of its own per image: the verdict is cached per architecture, and the two
	// images share one.
	newDriver := func() *Driver {
		d, err := New(Config{ProxyImage: liveProxyImage})
		if err != nil {
			t.Fatalf("docker.New: %v", err)
		}
		return d
	}
	if err := newDriver().checkImageArch(ctx, emulated, ""); err != nil {
		t.Errorf("the emulated image must be allowed, got %v", err)
	}
	d := newDriver()
	err := d.checkImageArch(ctx, unrunnable, "")
	if !errors.Is(err, ErrImageArch) {
		t.Fatalf("the unrunnable image must be refused with ErrImageArch, got %v", err)
	}
	t.Logf("refusal: %v", err)

	// No probe container is left behind.
	left, lerr := d.cli.(containerListerAPI).ContainerList(ctx, client.ContainerListOptions{All: true, Filters: client.Filters{}.Add("label", labelComponent+"="+componentArchProbe)})
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(left.Items) != 0 {
		t.Errorf("%d arch-probe containers left behind", len(left.Items))
	}
}
