// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

import (
	"context"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

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

func (f *fakeEnvbuilderDocker) ImageList(_ context.Context, _ image.ListOptions) ([]image.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []image.Summary
	for tag, present := range f.imagesPresent {
		if present {
			out = append(out, image.Summary{RepoTags: []string{tag}})
		}
	}
	return out, nil
}

func (f *fakeEnvbuilderDocker) ImagePull(_ context.Context, ref string, _ image.PullOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	f.pullCalled = true
	if f.imagesPresent == nil {
		f.imagesPresent = map[string]bool{}
	}
	f.imagesPresent[ref] = true
	f.mu.Unlock()
	return io.NopCloser(strings.NewReader(`{"status":"pulled"}`)), nil
}

func (f *fakeEnvbuilderDocker) ImageBuild(_ context.Context, buildContext io.Reader, options build.ImageBuildOptions) (build.ImageBuildResponse, error) {
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
	return build.ImageBuildResponse{Body: io.NopCloser(strings.NewReader(body)), OSType: "linux"}, nil
}

func (f *fakeEnvbuilderDocker) ContainerCreate(_ context.Context, cfg *container.Config, hostCfg *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalled++
	if cfg != nil {
		f.lastEnv = cfg.Env
	}
	if hostCfg != nil {
		f.lastBinds = hostCfg.Binds
		f.lastNetworkMode = hostCfg.NetworkMode
		f.lastResources = hostCfg.Resources
		f.lastStorageOpt = hostCfg.StorageOpt
	}
	return container.CreateResponse{ID: "fake-build-container"}, nil
}

func (f *fakeEnvbuilderDocker) ContainerStart(_ context.Context, _ string, _ container.StartOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalled++
	return nil
}

func (f *fakeEnvbuilderDocker) ContainerLogs(_ context.Context, _ string, _ container.LogsOptions) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("build log output\n")), nil
}

func (f *fakeEnvbuilderDocker) ContainerWait(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	resultC := make(chan container.WaitResponse, 1)
	errC := make(chan error, 1)
	f.mu.Lock()
	code := f.exitCode
	f.mu.Unlock()
	resultC <- container.WaitResponse{StatusCode: code}
	return resultC, errC
}

func (f *fakeEnvbuilderDocker) ContainerRemove(_ context.Context, _ string, _ container.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = true
	return nil
}

func (f *fakeEnvbuilderDocker) ContainerInspect(_ context.Context, id string) (container.InspectResponse, error) {
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{ID: id},
	}, nil
}
