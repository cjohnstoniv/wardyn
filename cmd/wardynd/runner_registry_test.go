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
	"errors"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
)

// rrFlags is the minimum bootFlags buildRunnerFromFlags and componentsInfo
// dereference. runnerTargetOverride is empty here — the unset default, i.e. no
// override — and boot_runner_target_test.go sets it on its own flags.
func rrFlags(runnerSel string) *bootFlags {
	sel, cmap, img, probeImg := runnerSel, "", "wardyn-proxy:test", ""
	id, sec, rec := "embedded", "pg", "pg" // the defaults; componentsInfo derefs them
	target := ""
	return &bootFlags{
		runnerSel: &sel, runnerTargetOverride: &target, confinementMap: &cmap, proxyImage: &img, driveProbeImage: &probeImg,
		identitySel: &id, secretStoreSel: &sec, recordingSel: &rec,
	}
}

// -runner none (and "") stays a nil runner with target "none", no registry hit.
func TestBuildRunnerFromFlags_None(t *testing.T) {
	for _, sel := range []string{"none", ""} {
		r, target, err := buildRunnerFromFlags(rrFlags(sel), nil, nil)
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
	r, _, err := buildRunnerFromFlags(rrFlags("bogus"), nil, nil)
	if err == nil {
		t.Fatalf("want error for -runner bogus, got runner %T", r)
	}
	if !strings.Contains(err.Error(), `-runner "bogus"`) || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("want fail-closed registry-miss error naming -runner, got: %v", err)
	}
}

// TestBuildRunnerFromFlags_RecordFollowsRecordingStore is the #1113
// boot-level pin: WARDYN_RECORDING_STORE (*f.recordingSel) must reach the
// substrate's registration Deps.Record — the boundary a driver's register.go
// reads to build its own Config.Record (internal/runner/k8s and
// internal/runner/docker's buildConfig, each pinned separately by their own
// register_test.go). A spy substrate captures the Deps buildRunnerFromFlags
// actually passes, so this test needs no live cluster or daemon and holds
// tag-free.
func TestBuildRunnerFromFlags_RecordFollowsRecordingStore(t *testing.T) {
	const spyName = "recordspy-1113"
	spyErr := errors.New("recordspy: refuses to construct (captures Deps only)")
	var got substrate.Deps
	substrate.Register(spyName, func(d substrate.Deps) (substrate.Substrate, error) {
		got = d
		return nil, spyErr
	})

	for _, tt := range []struct {
		store string
		want  bool
	}{
		{"off", false},
		{"pg", true},
		{"fs", true},
	} {
		store := tt.store
		f := rrFlags(spyName)
		f.recordingSel = &store
		if _, _, err := buildRunnerFromFlags(f, nil, nil); !errors.Is(err, spyErr) {
			t.Fatalf("recording store %q: buildRunnerFromFlags error = %v, want the spy's refusal (proves the spy was actually reached)", store, err)
		}
		if got.Record != tt.want {
			t.Errorf("recording store %q: Deps.Record = %v, want %v", store, got.Record, tt.want)
		}
	}
}

// /healthz must report what recording actually does, not what the flag says. The stock Helm install
// sets WARDYN_RECORDING_STORE=off (persistence.enabled=false by default), which recording.New
// resolves to a nil Store per its own "disabled" contract — so componentsInfo must not echo
// *f.recordingSel, or /healthz advertises a live "fs" store while every run silently records
// nothing.
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
