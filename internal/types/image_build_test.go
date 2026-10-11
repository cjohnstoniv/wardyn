// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"testing"
	"time"
)

func TestImageBuild_SHA256IsStable(t *testing.T) {
	rec := ImageBuild{
		Schema: ImageBuildSchema, Lane: ImageBuildLaneGenerated, CacheKey: "k", BuildID: "b",
		Reason: ImageBuildReasonCacheMiss, BuiltAt: time.Now().UTC().Truncate(time.Second),
		WardynVersion: "0.9.0", Resolved: true,
		Inputs: ImageBuildInputs{
			Base:       ImageBuildRef{Ref: "base:ubuntu", Digest: "sha256:aa"},
			Features:   []ImageBuildFeature{{Ref: "f:1", Options: map[string]any{"version": "1.22", "b": true}}},
			Installers: []ImageBuildInstaller{{Tool: "claude-code", Channel: "stable"}},
			Generator:  &ImageBuildGenerator{Salt: "v2", FilesSHA256: map[string]string{"b": "2", "a": "1"}},
		},
		Image: ImageBuildImage{Ref: "r:t", ID: "sha256:bb"},
	}
	want, err := rec.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var back ImageBuild
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	got, err := back.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("hash changed across a JSON round trip: %s != %s", got, want)
	}
	rec.Resolved = false
	if other, _ := rec.SHA256(); other == want {
		t.Fatal("hash ignores a changed field")
	}
}
