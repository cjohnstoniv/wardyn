// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

import (
	"strings"
	"testing"
)

// The build sandbox's blast-radius caps are env-tunable. A value the builder
// cannot parse must fail the build, never quietly resolve to "not configured":
// for WARDYN_ENVBUILD_MAX_CONTEXT_MB that misread turns the writable-layer cap
// OFF, and for the memory/CPU caps it substitutes a bound the operator did not
// choose. These tests pin unset -> default, valid -> parsed, invalid -> loud.

func TestEnvInt64_UnsetValidInvalid(t *testing.T) {
	const key = "WARDYN_ENVBUILD_TEST_INT"

	t.Setenv(key, "")
	if n, err := envInt64(key); n != 0 || err != nil {
		t.Fatalf("unset: got (%d, %v), want (0, nil)", n, err)
	}

	t.Setenv(key, "  512 ")
	if n, err := envInt64(key); n != 512 || err != nil {
		t.Fatalf("valid: got (%d, %v), want (512, nil)", n, err)
	}

	for _, bad := range []string{"512m", "5o12", "-1", "1.5", "abc"} {
		t.Setenv(key, bad)
		n, err := envInt64(key)
		if err == nil {
			t.Fatalf("invalid %s=%q: got (%d, nil), want an error (must not silently mean \"unset\")", key, bad, n)
		}
		if !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), bad) {
			t.Fatalf("invalid %s=%q: error %q must name the variable and the value", key, bad, err)
		}
	}
}

func TestEnvFloat_UnsetValidInvalid(t *testing.T) {
	const key = "WARDYN_ENVBUILD_TEST_FLOAT"

	t.Setenv(key, "")
	if f, err := envFloat(key); f != 0 || err != nil {
		t.Fatalf("unset: got (%v, %v), want (0, nil)", f, err)
	}

	t.Setenv(key, " 1.5 ")
	if f, err := envFloat(key); f != 1.5 || err != nil {
		t.Fatalf("valid: got (%v, %v), want (1.5, nil)", f, err)
	}

	for _, bad := range []string{"1.5cpus", "-2", "two", "1,5"} {
		t.Setenv(key, bad)
		f, err := envFloat(key)
		if err == nil {
			t.Fatalf("invalid %s=%q: got (%v, nil), want an error", key, bad, f)
		}
		if !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), bad) {
			t.Fatalf("invalid %s=%q: error %q must name the variable and the value", key, bad, err)
		}
	}
}

// Unset => the compiled-in defaults, quietly.
func TestHardenedHostConfig_UnsetUsesDefaults(t *testing.T) {
	b := newPushBuilder(t, newFakeEnvbuilderDocker())
	hc, err := b.hardenedHostConfig()
	if err != nil {
		t.Fatalf("unset env must not error: %v", err)
	}
	if hc.Memory != defaultBuildMemoryBytes {
		t.Errorf("Memory = %d, want default %d", hc.Memory, defaultBuildMemoryBytes)
	}
	if hc.NanoCPUs != defaultBuildNanoCPUs {
		t.Errorf("NanoCPUs = %d, want default %d", hc.NanoCPUs, defaultBuildNanoCPUs)
	}
	if hc.StorageOpt != nil {
		t.Errorf("StorageOpt = %v, want nil (context cap off by default)", hc.StorageOpt)
	}
}

// Valid => parsed and applied.
func TestHardenedHostConfig_ValidEnvApplied(t *testing.T) {
	t.Setenv(envBuildMemoryMB, "512")
	t.Setenv(envBuildCPUs, "1.5")
	t.Setenv(envMaxContextMB, "1024")

	b := newPushBuilder(t, newFakeEnvbuilderDocker())
	hc, err := b.hardenedHostConfig()
	if err != nil {
		t.Fatalf("valid env must not error: %v", err)
	}
	if want := int64(512) << 20; hc.Memory != want {
		t.Errorf("Memory = %d, want %d", hc.Memory, want)
	}
	if hc.MemorySwap != hc.Memory {
		t.Errorf("MemorySwap = %d, want == Memory %d", hc.MemorySwap, hc.Memory)
	}
	if want := int64(1.5 * 1e9); hc.NanoCPUs != want {
		t.Errorf("NanoCPUs = %d, want %d", hc.NanoCPUs, want)
	}
	if got, want := hc.StorageOpt["size"], "1073741824"; got != want {
		t.Errorf("StorageOpt[size] = %q, want %q", got, want)
	}
}

// Invalid => the whole HostConfig resolution fails. Each var is exercised on its
// own so none of them can be the one that still swallows bad input.
func TestHardenedHostConfig_InvalidEnvIsLoud(t *testing.T) {
	for _, tc := range []struct{ key, val string }{
		{envBuildMemoryMB, "512m"},  // unit suffix: the plausible operator typo
		{envBuildMemoryMB, "-512"},  // negative
		{envBuildCPUs, "two"},       //
		{envBuildCPUs, "-1"},        //
		{envMaxContextMB, "1024MB"}, // used to silently disable the layer cap
		{envMaxContextMB, "1_024"},  //
	} {
		t.Run(tc.key+"="+tc.val, func(t *testing.T) {
			t.Setenv(tc.key, tc.val)
			b := newPushBuilder(t, newFakeEnvbuilderDocker())
			hc, err := b.hardenedHostConfig()
			if err == nil {
				t.Fatalf("invalid %s=%q: hardenedHostConfig returned %+v, nil — bad input must fail closed", tc.key, tc.val, hc.Resources)
			}
			if !strings.Contains(err.Error(), tc.key) || !strings.Contains(err.Error(), tc.val) {
				t.Fatalf("error %q must name the variable and the value", err)
			}
		})
	}
}

// End-to-end through the exported entrypoint: a typo'd cap must abort the build
// before a container is ever created, not run it with substituted limits.
func TestBuild_InvalidCapEnvAbortsBeforeContainerCreate(t *testing.T) {
	t.Setenv(envMaxContextMB, "1024MB")

	f := newFakeEnvbuilderDocker()
	b := newPushBuilder(t, f)
	_, err := b.Build(t.Context(), BuildSpec{
		RepoURL:        "https://example.com/repo.git",
		OutputImageTag: "wardyn/out:test",
	})
	if err == nil {
		t.Fatal("Build must fail when a build-sandbox cap env is unparseable")
	}
	if !strings.Contains(err.Error(), envMaxContextMB) {
		t.Fatalf("error %q must name %s", err, envMaxContextMB)
	}
	if f.createCalled != 0 {
		t.Fatalf("build container was created (%d times) despite an unresolvable cap", f.createCalled)
	}
}
