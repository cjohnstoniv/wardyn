// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// toolsDirWithRequired creates a temp dir containing every required runner tool
// as an executable stub, so validateToolsDir passes.
func toolsDirWithRequired(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range requiredTools {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("write tool %q: %v", name, err)
		}
	}
	return dir
}

func TestFinalizeBase_WrapsPresentBase(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	f.imagesPresent["ubuntu:24.04"] = true // pre-pulled base
	b := newWithClient(f, "envbuilder:test", "")
	b.ToolsDir = toolsDirWithRequired(t)

	tag, err := b.FinalizeBase(context.Background(), "ubuntu:24.04", "wardyn-byoi/run-1:latest")
	if err != nil {
		t.Fatalf("FinalizeBase: %v", err)
	}
	if tag != "wardyn-byoi/run-1:latest" {
		t.Fatalf("wrong output tag: %q", tag)
	}
	if !f.imageBuildCalled {
		t.Fatal("finalize did not run the wrap ImageBuild")
	}
	if len(f.lastBuildTags) != 1 || f.lastBuildTags[0] != "wardyn-byoi/run-1:latest" {
		t.Fatalf("wrap tagged %v, want [wardyn-byoi/run-1:latest]", f.lastBuildTags)
	}
	// A present base must NOT trigger a pull (private/local images work).
	if f.pullCalled {
		t.Fatal("FinalizeBase pulled a base that was already present")
	}
}

func TestFinalizeBase_PullsAbsentBase(t *testing.T) {
	f := newFakeEnvbuilderDocker() // only envbuilder:test present
	b := newWithClient(f, "envbuilder:test", "")
	b.ToolsDir = toolsDirWithRequired(t)

	if _, err := b.FinalizeBase(context.Background(), "myco/dev:latest", "wardyn-byoi/run-2:latest"); err != nil {
		t.Fatalf("FinalizeBase: %v", err)
	}
	if !f.pullCalled {
		t.Fatal("FinalizeBase did not pull an absent base")
	}
}

func TestFinalizeBase_PrePulledDigestBaseIsNotRePulled(t *testing.T) {
	// A digest-pinned base pre-pulled on the host (present via RepoDigests) must
	// NOT trigger a pull — that's the "immutable, no registry-auth" workflow.
	ref := "myco/dev@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	f := newFakeEnvbuilderDocker()
	f.imagesPresent[ref] = true
	b := newWithClient(f, "envbuilder:test", "")
	b.ToolsDir = toolsDirWithRequired(t)

	if _, err := b.FinalizeBase(context.Background(), ref, "wardyn-byoi/run-d:latest"); err != nil {
		t.Fatalf("FinalizeBase on a pre-pulled digest base: %v", err)
	}
	if f.pullCalled {
		t.Fatal("FinalizeBase re-pulled a pre-pulled digest-pinned base (RepoDigests not matched)")
	}
}

func TestFinalizeBase_FailsClosedOnBuildError(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	f.imagesPresent["distroless/static"] = true
	f.buildErr = "COPY failed: no shell in base"
	b := newWithClient(f, "envbuilder:test", "")
	b.ToolsDir = toolsDirWithRequired(t)

	_, err := b.FinalizeBase(context.Background(), "distroless/static", "wardyn-byoi/run-3:latest")
	if err == nil {
		t.Fatal("expected FinalizeBase to fail closed on a wrap-build error")
	}
	if !strings.Contains(err.Error(), "COPY failed") {
		t.Fatalf("error should carry the build failure, got: %v", err)
	}
}

func TestFinalizeBase_FailsWhenToolsDirMissing(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	b := newWithClient(f, "envbuilder:test", "")
	// No ToolsDir configured (and no env var in this test).
	t.Setenv(envToolsDir, "")
	if _, err := b.FinalizeBase(context.Background(), "ubuntu:24.04", "wardyn-byoi/run-4:latest"); err == nil {
		t.Fatal("expected FinalizeBase to fail closed with no tools dir")
	}
}
