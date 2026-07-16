// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

import (
	"context"
	"io"
	"iter"
	"strconv"
	"strings"
	"sync"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/client"
)

// fakePullResponse adapts an io.ReadCloser to client.ImagePullResponse (the v29
// ImagePull return type). PullImage only drains the reader, so the extra
// progress helpers are inert no-ops.
type fakePullResponse struct{ io.ReadCloser }

func (fakePullResponse) JSONMessages(context.Context) iter.Seq2[jsonstream.Message, error] {
	return nil
}
func (fakePullResponse) Wait(context.Context) error { return nil }

// fakeEnvbuilderDocker is an in-memory envbuilderDockerAPI for unit tests.
// It simulates a Docker daemon without requiring one to be present.
type fakeEnvbuilderDocker struct {
	mu sync.Mutex

	// imagesPresent maps image tag -> present. If nil, behaves as if
	// all images are absent (triggering a pull).
	imagesPresent map[string]bool

	// exitCode is the exit code that ContainerWait returns. 0 = success.
	exitCode int64

	// Track call counts for assertions.
	pullCalled   bool
	createCalled int
	startCalled  int
	removed      bool

	// Finalize (second-stage ImageBuild) tracking.
	imageBuildCalled bool
	lastBuildTags    []string
	// buildErr, when non-empty, makes ImageBuild's response stream report a build
	// error (simulating e.g. a COPY-source failure).
	buildErr string

	// lastEnv records the Env slice given to ContainerCreate.
	lastEnv []string
	// lastBinds records the Binds slice given to ContainerCreate.
	lastBinds []string
	// lastNetworkMode records hostCfg.NetworkMode given to ContainerCreate.
	lastNetworkMode container.NetworkMode
	// lastResources records hostCfg.Resources given to ContainerCreate.
	lastResources container.Resources
	// lastStorageOpt records hostCfg.StorageOpt given to ContainerCreate.
	lastStorageOpt map[string]string
}

func newFakeEnvbuilderDocker() *fakeEnvbuilderDocker {
	return &fakeEnvbuilderDocker{
		imagesPresent: map[string]bool{"envbuilder:test": true},
	}
}

func (f *fakeEnvbuilderDocker) ImageList(_ context.Context, _ client.ImageListOptions) (client.ImageListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []image.Summary
	for ref, present := range f.imagesPresent {
		if !present {
			continue
		}
		// A digest-pinned ref lives under RepoDigests, not RepoTags (mirrors the
		// real daemon), so ensureImage must match it there.
		if strings.Contains(ref, "@sha256:") {
			out = append(out, image.Summary{RepoDigests: []string{ref}})
		} else {
			out = append(out, image.Summary{RepoTags: []string{ref}})
		}
	}
	return client.ImageListResult{Items: out}, nil
}

func (f *fakeEnvbuilderDocker) ImagePull(_ context.Context, ref string, _ client.ImagePullOptions) (client.ImagePullResponse, error) {
	f.mu.Lock()
	f.pullCalled = true
	if f.imagesPresent == nil {
		f.imagesPresent = map[string]bool{}
	}
	f.imagesPresent[ref] = true
	f.mu.Unlock()
	return fakePullResponse{io.NopCloser(strings.NewReader(`{"status":"pulled"}`))}, nil
}

func (f *fakeEnvbuilderDocker) ImageBuild(_ context.Context, buildContext io.Reader, options client.ImageBuildOptions) (client.ImageBuildResult, error) {
	f.mu.Lock()
	f.imageBuildCalled = true
	f.lastBuildTags = options.Tags
	buildErr := f.buildErr
	f.mu.Unlock()
	// Drain the context so the caller's tar writer isn't left dangling.
	_, _ = io.Copy(io.Discard, buildContext)
	body := `{"stream":"Step 1/3 : FROM base\n"}` + "\n" + `{"stream":"Successfully built deadbeef\n"}` + "\n"
	if buildErr != "" {
		body = `{"error":` + strconv.Quote(buildErr) + `}` + "\n"
	}
	return client.ImageBuildResult{Body: io.NopCloser(strings.NewReader(body))}, nil
}

func (f *fakeEnvbuilderDocker) ContainerCreate(_ context.Context, opts client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalled++
	if opts.Config != nil {
		f.lastEnv = opts.Config.Env
	}
	if opts.HostConfig != nil {
		f.lastBinds = opts.HostConfig.Binds
		f.lastNetworkMode = opts.HostConfig.NetworkMode
		f.lastResources = opts.HostConfig.Resources
		f.lastStorageOpt = opts.HostConfig.StorageOpt
	}
	return client.ContainerCreateResult{ID: "fake-build-container"}, nil
}

func (f *fakeEnvbuilderDocker) ContainerStart(_ context.Context, _ string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalled++
	return client.ContainerStartResult{}, nil
}

func (f *fakeEnvbuilderDocker) ContainerLogs(_ context.Context, _ string, _ client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
	return io.NopCloser(strings.NewReader("build log output\n")), nil
}

func (f *fakeEnvbuilderDocker) ContainerWait(_ context.Context, _ string, _ client.ContainerWaitOptions) client.ContainerWaitResult {
	resultC := make(chan container.WaitResponse, 1)
	errC := make(chan error, 1)
	f.mu.Lock()
	code := f.exitCode
	f.mu.Unlock()
	resultC <- container.WaitResponse{StatusCode: code}
	return client.ContainerWaitResult{Result: resultC, Error: errC}
}

func (f *fakeEnvbuilderDocker) ContainerRemove(_ context.Context, _ string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = true
	return client.ContainerRemoveResult{}, nil
}
