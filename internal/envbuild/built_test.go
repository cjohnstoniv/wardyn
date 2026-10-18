// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
)

const (
	testBaseDigest  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testOtherDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestFinalizeImage_IDComesFromTheBuildStream(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	f.imagesPresent["ubuntu:24.04"] = true
	b := newWithClient(f, "envbuilder:test", "")
	b.ToolsDir = toolsDirWithRequired(t)

	built, err := b.FinalizeBase(context.Background(), "ubuntu:24.04", "wardyn-byoi/run-id:latest", nil, false)
	if err != nil {
		t.Fatalf("FinalizeBase: %v", err)
	}
	if want := fakeImageID("wardyn-byoi/run-id:latest"); built.ID != want {
		t.Fatalf("Built.ID = %q, want the stream's aux.ID %q", built.ID, want)
	}
	// The ID is never read back from an inspect of the output tag: the only inspect is the base's.
	for _, ref := range f.inspected {
		if ref == "wardyn-byoi/run-id:latest" {
			t.Fatalf("the output tag was inspected (%v); the ID must come from the build stream", f.inspected)
		}
	}
}

func TestFinalizeImage_NoAuxIDLeavesTheIDEmpty(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	f.imagesPresent["ubuntu:24.04"] = true
	f.noAuxID = true
	b := newWithClient(f, "envbuilder:test", "")
	b.ToolsDir = toolsDirWithRequired(t)

	built, err := b.FinalizeBase(context.Background(), "ubuntu:24.04", "wardyn-byoi/run-noid:latest", nil, false)
	if err != nil {
		t.Fatalf("a stream with no aux.ID must not fail the build: %v", err)
	}
	if built.ID != "" || built.Ref != "wardyn-byoi/run-noid:latest" {
		t.Fatalf("Built = %+v, want the tag and an empty ID", built)
	}
}

func TestBuild_ReturnsTheStreamIDAndKeepsItsTagFrom(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	b := newPushBuilder(t, f)
	built, err := b.Build(t.Context(), BuildSpec{RepoURL: "https://github.com/example/repo", OutputImageTag: "wardyn-ws:abc"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if built.ID != fakeImageID("wardyn-ws:abc") || built.BaseDigest != "" {
		t.Fatalf("Built = %+v, want the stream ID and no base digest on the stage-1 path", built)
	}
	if !strings.HasPrefix(f.lastDockerfile, "FROM registry.example.com/wardyn-cache:") {
		t.Fatalf("stage-1 finalize FROM = %q, want the per-build push ref (tag kept)", f.lastDockerfile)
	}
}

func TestFinalizeBase_WrapsByDigestWhenTheBaseHasOne(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	f.imagesPresent["ubuntu:24.04"] = true
	f.repoDigests = map[string][]string{"ubuntu:24.04": {"other/img@" + testOtherDigest, "ubuntu@" + testBaseDigest}}
	b := newWithClient(f, "envbuilder:test", "")
	b.ToolsDir = toolsDirWithRequired(t)

	built, err := b.FinalizeBase(context.Background(), "ubuntu:24.04", "wardyn-byoi/run-dg:latest", nil, false)
	if err != nil {
		t.Fatalf("FinalizeBase: %v", err)
	}
	if !strings.HasPrefix(f.lastDockerfile, "FROM ubuntu@"+testBaseDigest+"\n") {
		t.Fatalf("wrap Dockerfile = %q, want FROM ubuntu@%s", f.lastDockerfile, testBaseDigest)
	}
	if built.BaseDigest != testBaseDigest {
		t.Fatalf("BaseDigest = %q, want %q", built.BaseDigest, testBaseDigest)
	}
}

func TestFinalizeBase_WrapsByRefWhenTheBaseHasNoDigest(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	f.imagesPresent["myco/dev:latest"] = true
	f.repoDigests = map[string][]string{"myco/dev:latest": {"unrelated/repo@" + testBaseDigest}}
	b := newWithClient(f, "envbuilder:test", "")
	b.ToolsDir = toolsDirWithRequired(t)

	built, err := b.FinalizeBase(context.Background(), "myco/dev:latest", "wardyn-byoi/run-lo:latest", nil, false)
	if err != nil {
		t.Fatalf("FinalizeBase: %v", err)
	}
	if !strings.HasPrefix(f.lastDockerfile, "FROM myco/dev:latest\n") || built.BaseDigest != "" {
		t.Fatalf("local-only base: Dockerfile %q, BaseDigest %q; want FROM the ref and no digest", f.lastDockerfile, built.BaseDigest)
	}
}

func TestRepoDigestFor(t *testing.T) {
	cases := []struct {
		name, base string
		digests    []string
		want       string
	}{
		{"short name matches its normalized entry", "ubuntu:24.04", []string{"docker.io/library/ubuntu@" + testBaseDigest}, "docker.io/library/ubuntu@" + testBaseDigest},
		{"first match wins", "a/b:1", []string{"c/d@" + testOtherDigest, "a/b@" + testBaseDigest, "a/b@" + testOtherDigest}, "a/b@" + testBaseDigest},
		{"a pinned base keeps its own digest", "a/b@" + testOtherDigest, []string{"a/b@" + testBaseDigest, "a/b@" + testOtherDigest}, "a/b@" + testOtherDigest},
		{"private registry with a port", "localhost:5000/team/img:dev", []string{"localhost:5000/team/img@" + testBaseDigest}, "localhost:5000/team/img@" + testBaseDigest},
		{"other repository", "a/b:1", []string{"a/c@" + testBaseDigest}, ""},
		{"none", "a/b:1", nil, ""},
		{"unparseable base", "A B", []string{"a/b@" + testBaseDigest}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := repoDigestFor(c.base, c.digests); got != c.want {
				t.Fatalf("repoDigestFor(%q, %v) = %q, want %q", c.base, c.digests, got, c.want)
			}
		})
	}
}

func TestLocalImageID(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	f.imagesPresent["img:here"] = true
	f.inspectErrs = map[string]error{"img:boom": errors.New("daemon down")}
	b := newWithClient(f, "envbuilder:test", "")

	if id, found, err := b.LocalImageID(context.Background(), "img:here"); err != nil || !found || id == "" {
		t.Fatalf("present image: id=%q found=%v err=%v", id, found, err)
	}
	if id, found, err := b.LocalImageID(context.Background(), "img:gone"); err != nil || found || id != "" {
		t.Fatalf("absent image must be (\"\", false, nil), got id=%q found=%v err=%v", id, found, err)
	}
	f.inspectErrs["img:nf"] = errdefs.ErrNotFound
	if _, found, err := b.LocalImageID(context.Background(), "img:nf"); err != nil || found {
		t.Fatalf("a not-found error must read as absent: found=%v err=%v", found, err)
	}
	if _, _, err := b.LocalImageID(context.Background(), "img:boom"); err == nil {
		t.Fatal("any other inspect error must surface")
	}
}

// TestFinalizeBase_IDEqualsTheDaemonsAuxID is the real-daemon proof that the ID read from
// the build stream is the ID the daemon holds for the output tag. It needs
// WARDYN_TEST_DOCKER=1 and WARDYN_TEST_TOOLS_DIR (the runner tools, staged as
// `make test-envbuild-integration` stages them).
func TestFinalizeBase_IDEqualsTheDaemonsAuxID(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the real-Docker image ID test")
	}
	toolsDir := os.Getenv("WARDYN_TEST_TOOLS_DIR")
	if toolsDir == "" {
		t.Skip("set WARDYN_TEST_TOOLS_DIR=<dir with the runner tools> to run the image ID test")
	}
	b, err := New("", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b.ToolsDir = toolsDir
	tag := "wardyn-envbuild-idtest:" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", "-f", tag).Run() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	built, err := b.FinalizeBase(ctx, "busybox:latest", tag, nil, false)
	if err != nil {
		t.Fatalf("FinalizeBase: %v", err)
	}
	got, found, err := b.LocalImageID(ctx, tag)
	if err != nil || !found {
		t.Fatalf("LocalImageID(%q) = (%q, %v, %v)", tag, got, found, err)
	}
	if built.ID == "" || built.ID != got {
		t.Fatalf("Built.ID = %q, the daemon's ID for %s = %q", built.ID, tag, got)
	}
}
