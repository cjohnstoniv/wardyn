// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

import (
	"errors"
	"strings"
	"testing"

	"github.com/containerd/errdefs"
)

const (
	pinTestTag  = "wardyn-ws:gen"
	pinTestFile = "2.1.287 linux-x64 3333333333333333333333333333333333333333333333333333333333333333\n"
)

var pinTestFiles = map[string]string{".devcontainer/devcontainer.json": `{"image":"golang:1.22"}`}

func TestBuildFromDevcontainerFiles_ReadsThePinFile(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	f.pinFiles = map[string][]byte{pinTestTag: []byte(pinTestFile)}
	b := newPushBuilder(t, f)

	built, err := b.BuildFromDevcontainerFiles(t.Context(), pinTestFiles, pinTestTag, nil)
	if err != nil {
		t.Fatalf("BuildFromDevcontainerFiles: %v", err)
	}
	if string(built.PinFile) != pinTestFile {
		t.Fatalf("PinFile = %q, want %q", built.PinFile, pinTestFile)
	}
	if len(f.pinReadImages) != 1 || f.pinReadImages[0] != pinTestTag {
		t.Fatalf("pin-read containers were created from %v, want [%s]", f.pinReadImages, pinTestTag)
	}
	cfg, host := f.pinReadConfig, f.pinReadHost
	if !cfg.NetworkDisabled || host.NetworkMode != "none" || cfg.Labels[envbuildContainerLabel] == "" {
		t.Errorf("pin-read container: NetworkDisabled=%v NetworkMode=%q labels=%v, want no network and the orphan-sweep label", cfg.NetworkDisabled, host.NetworkMode, cfg.Labels)
	}
	if f.startCalled != 1 {
		t.Errorf("%d containers were started; only the build container may be", f.startCalled)
	}
	if !strings.HasPrefix(f.removedIDs[len(f.removedIDs)-1], "fake-pin-container") {
		t.Errorf("removed %v: the pin-read container must be removed last", f.removedIDs)
	}
}

func TestBuildFromDevcontainerFiles_MissingOrOversizedPinFileLeavesItEmpty(t *testing.T) {
	for name, tc := range map[string]struct {
		file []byte
		want int
	}{
		"missing":        {nil, 0},
		"at the cap":     {[]byte(strings.Repeat("a", 4096)), 4096},
		"over the cap":   {[]byte(strings.Repeat("a", 4097)), 0},
		"far over a cap": {[]byte(strings.Repeat("a", 1<<20)), 0},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeEnvbuilderDocker()
			if tc.file != nil {
				f.pinFiles = map[string][]byte{pinTestTag: tc.file}
			}
			built, err := newPushBuilder(t, f).BuildFromDevcontainerFiles(t.Context(), pinTestFiles, pinTestTag, nil)
			if err != nil {
				t.Fatalf("a pin file problem must not fail the build: %v", err)
			}
			if len(built.PinFile) != tc.want {
				t.Fatalf("PinFile has %d bytes, want %d", len(built.PinFile), tc.want)
			}
			if built.Ref != pinTestTag {
				t.Errorf("Ref = %q", built.Ref)
			}
		})
	}
}

// The pin file is read for the generated lane only: a repo author's image or a BYOI base can carry
// the same path, and neither may record an installer from it.
func TestPinFile_OnlyTheGeneratedLaneReadsIt(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	f.imagesPresent["ubuntu:24.04"] = true
	f.pinFiles = map[string][]byte{"wardyn-ws:repo": []byte(pinTestFile), "wardyn-byoi/r:latest": []byte(pinTestFile)}
	b := newPushBuilder(t, f)

	repo, err := b.Build(t.Context(), BuildSpec{RepoURL: "https://github.com/example/repo", OutputImageTag: "wardyn-ws:repo"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	byoi, err := b.FinalizeBase(t.Context(), "ubuntu:24.04", "wardyn-byoi/r:latest", nil, false)
	if err != nil {
		t.Fatalf("FinalizeBase: %v", err)
	}
	if repo.PinFile != nil || byoi.PinFile != nil || len(f.pinReadImages) != 0 {
		t.Fatalf("repo PinFile %q, byoi PinFile %q, pin-read containers %v: only the generated lane reads the file", repo.PinFile, byoi.PinFile, f.pinReadImages)
	}
}

func TestBuilder_RemoteDigest(t *testing.T) {
	const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	f := newFakeEnvbuilderDocker()
	f.remoteDigests = map[string]string{"mcr.microsoft.com/devcontainers/base:ubuntu": digest}
	f.remoteErrs = map[string]error{"unreachable.example/x:1": errors.New("dial tcp: i/o timeout")}
	b := newPushBuilder(t, f)

	if got, err := b.RemoteDigest(t.Context(), "mcr.microsoft.com/devcontainers/base:ubuntu"); err != nil || got != digest {
		t.Fatalf("RemoteDigest = %q, %v; want %s", got, err, digest)
	}
	if _, err := b.RemoteDigest(t.Context(), "ghcr.io/none/here@sha256:0"); !errdefs.IsNotFound(err) {
		t.Errorf("an unknown manifest must stay not-found through the wrap: %v", err)
	}
	if _, err := b.RemoteDigest(t.Context(), "unreachable.example/x:1"); err == nil || errdefs.IsNotFound(err) {
		t.Errorf("an unreachable registry must not read as not-found: %v", err)
	}
}
