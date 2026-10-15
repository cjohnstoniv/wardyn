// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func archDriver(engine string, images map[string]string) (*Driver, *fakeDocker) {
	f := newFakeDocker()
	f.info.Architecture = engine
	for ref, a := range images {
		f.images[ref] = true
		f.imageArch[ref] = a
	}
	return newTestDriver(f), f
}

func TestEnsureImageArch(t *testing.T) {
	const ref = "mirror.example/agent:1"
	cases := []struct {
		name, engine, image string
		wantErr             bool
	}{
		{"match", "x86_64", "amd64", false},
		{"aarch64 is arm64", "aarch64", "arm64", false},
		{"mismatch", "aarch64", "amd64", true},
		{"mismatch reverse", "x86_64", "arm64", true},
		{"image arch unknown", "aarch64", "", false},
		{"engine arch unknown", "", "amd64", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, _ := archDriver(c.engine, map[string]string{ref: c.image})
			err := d.ensureImage(context.Background(), ref, nil)
			if !c.wantErr {
				if err != nil {
					t.Fatalf("got %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrImageArch) {
				t.Fatalf("got %v, want ErrImageArch", err)
			}
			for _, want := range []string{ref, c.image, "this runner is " + normalizeArch(c.engine), "multi-arch"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
		})
	}
}

func TestEnsureImageArchChecksAPulledImage(t *testing.T) {
	d, f := archDriver("aarch64", nil)
	f.onPull = func(ref string) { f.imageArch[ref] = "amd64" }
	if err := d.ensureImage(context.Background(), "mirror.example/agent:2", nil); !errors.Is(err, ErrImageArch) {
		t.Fatalf("got %v, want ErrImageArch", err)
	}
}

func TestCreateSandboxRefusesAWrongArchImage(t *testing.T) {
	for name, images := range map[string]map[string]string{
		"agent image": {testSpec().Image: "amd64", "wardyn-proxy:dev": "arm64"},
		"proxy image": {testSpec().Image: "arm64", "wardyn-proxy:dev": "amd64"},
	} {
		t.Run(name, func(t *testing.T) {
			d, f := archDriver("aarch64", images)
			_, err := d.CreateSandbox(context.Background(), testSpec())
			if !errors.Is(err, ErrImageArch) {
				t.Fatalf("got %v, want ErrImageArch", err)
			}
			if len(f.networks) != 0 || len(f.containers) != 0 {
				t.Errorf("created %d networks, %d containers before refusing", len(f.networks), len(f.containers))
			}
		})
	}
}

func TestProbeRelayRefusesAWrongArchProxyImage(t *testing.T) {
	d, f := archDriver("aarch64", map[string]string{"wardyn-proxy:dev": "amd64"})
	err := d.ProbeRelay(context.Background(), relayProbeTarget, 8443)
	if !errors.Is(err, ErrImageArch) {
		t.Fatalf("got %v, want ErrImageArch", err)
	}
	if len(f.containers) != 0 {
		t.Errorf("a probe container was created: %v", f.containers)
	}
}

func TestEngineArchIsCached(t *testing.T) {
	d, f := archDriver("aarch64", nil)
	if got := d.engineArch(context.Background()); got != "arm64" {
		t.Fatalf("engineArch = %q", got)
	}
	f.info.Architecture = "x86_64"
	if got := d.engineArch(context.Background()); got != "arm64" {
		t.Fatalf("engineArch after the daemon changed = %q, want the cached arm64", got)
	}
}

func TestCheckImageArchSkipsAnImageGoneSinceThePresenceCheck(t *testing.T) {
	d, _ := archDriver("aarch64", nil)
	if err := d.checkImageArch(context.Background(), "absent:1"); err != nil {
		t.Fatalf("got %v, want nil", err)
	}
}
