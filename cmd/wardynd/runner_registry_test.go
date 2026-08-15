// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Tag-free behavior of the self-registering substrate seam: "none" and an
// unknown -runner behave identically in BOTH build flavors. The per-flavor
// registry expectations live in runner_registry_docker_test.go (docker tag:
// "docker" resolves) and runner_registry_nodocker_test.go (tagless: "docker" is
// not registered — which doubles as the counterfactual for removing
// internal/runner/docker/register.go, since the tagless build IS the build
// without that init()).

package main

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

func rrFlags(runnerSel string) *bootFlags {
	sel, cmap, img := runnerSel, "", "wardyn-proxy:test"
	id, sec, rec := "embedded", "pg", "pg" // the defaults; componentsInfo derefs them
	return &bootFlags{
		runnerSel: &sel, confinementMap: &cmap, proxyImage: &img,
		identitySel: &id, secretStoreSel: &sec, recordingSel: &rec,
	}
}

// -runner none (and "") stays a nil runner with target "none", no registry hit.
func TestBuildRunnerFromFlags_None(t *testing.T) {
	for _, sel := range []string{"none", ""} {
		r, target, err := buildRunnerFromFlags(rrFlags(sel), nil)
		if err != nil {
			t.Fatalf("-runner %q: unexpected error: %v", sel, err)
		}
		if r != nil {
			t.Fatalf("-runner %q: want nil runner, got %T", sel, r)
		}
		if target != "none" {
			t.Fatalf("-runner %q: want target \"none\", got %q", sel, target)
		}
	}
}

// An unknown -runner FAILS CLOSED with the registry-miss error naming the flag.
func TestBuildRunnerFromFlags_UnknownFailsClosed(t *testing.T) {
	r, _, err := buildRunnerFromFlags(rrFlags("bogus"), nil)
	if err == nil {
		t.Fatalf("want error for -runner bogus, got runner %T", r)
	}
	if !strings.Contains(err.Error(), `-runner "bogus"`) || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("want fail-closed registry-miss error naming -runner, got: %v", err)
	}
}

// /healthz must report what recording ACTUALLY does, not what the flag says.
// The stock Helm install sets WARDYN_RECORDING_STORE=fs with an empty dir
// (persistence.enabled=false by default), which recording.New resolves to a
// nil Store per its own "disabled" contract — but componentsInfo used to keep
// echoing *f.recordingSel regardless, so /healthz advertised a live "fs" store
// while every run silently recorded nothing.
func TestComponentsInfo_RecordingReflectsActualStore(t *testing.T) {
	f := rrFlags("none")
	sel := "fs" // the stock Helm chart's pin (see deploy/helm/wardyn/templates/deployment.yaml)
	f.recordingSel = &sel

	got := componentsInfo(f, "none", nil)["recording"]
	if got.Selected != "none" || got.Source != "disabled" {
		t.Fatalf("nil recStore: want Selected=none Source=disabled, got %+v", got)
	}

	store, err := recording.New("fs", recording.Deps{Dir: t.TempDir()})
	if err != nil || store == nil {
		t.Fatalf("recording.New(fs, non-empty dir): %v / %v", store, err)
	}
	got = componentsInfo(f, "none", store)["recording"]
	if got.Selected != "fs" || got.Source != "configured" {
		t.Fatalf("live recStore: want Selected=fs Source=configured, got %+v", got)
	}
}
