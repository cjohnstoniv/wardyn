// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"strconv"
	"strings"
	"sync"

	"github.com/containerd/errdefs"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/client"
)

// fakePullResponse adapts an io.ReadCloser to client.ImagePullResponse (the v29
// ImagePull return type). Wait reads the JSON progress stream and returns an error message in it, as
// the real client does; the progress iterator is an inert no-op.
type fakePullResponse struct{ io.ReadCloser }

func (fakePullResponse) JSONMessages(context.Context) iter.Seq2[jsonstream.Message, error] {
	return nil
}
func (r fakePullResponse) Wait(context.Context) error {
	dec := json.NewDecoder(r)
	for {
		var m jsonstream.Message
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if m.Error != nil {
			return errors.New(m.Error.Message)
		}
	}
}

// fakeEnvbuilderDocker is an in-memory envbuilderDockerAPI for unit tests.
// It simulates a Docker daemon without requiring one to be present.
type fakeEnvbuilderDocker struct {
	copiedTo   []string
	copiedTars []int
	mu         sync.Mutex

	// imagesPresent maps image tag -> present. If nil, behaves as if
	// all images are absent (triggering a pull).
	imagesPresent map[string]bool

	// onBuild maps image ref -> ONBUILD triggers baked into that image, as a real
	// daemon reports them from ImageInspect. Refs absent here inspect clean.
	onBuild map[string][]string

	// repoDigests maps image ref -> the RepoDigests a real daemon reports for it from ImageInspect.
	repoDigests map[string][]string

	// inspected records the refs passed to ImageInspect, in order.
	inspected []string

	// lastDockerfile is the Dockerfile of the last ImageBuild context.
	lastDockerfile string
	// noAuxID, when true, makes ImageBuild's stream carry no {"aux":{"ID":...}} message.
	noAuxID bool

	// exitCode is the exit code that ContainerWait returns. 0 = success.
	exitCode int64

	// Track call counts for assertions.
	pullCalled bool
	pullErr    error // when set, ImagePull fails with it
	// pulledRefs records every ref passed to ImagePull, so a test can assert which
	// image was pulled rather than only that some pull happened.
	pulledRefs   []string
	createCalled int
	startCalled  int
	removed      bool

	// Finalize (second-stage ImageBuild) tracking.
	imageBuildCalled bool
	lastBuildTags    []string
	// lastBuildPullParent records ImageBuildOptions.PullParent, i.e. whether the
	// daemon would re-resolve the FROM base after the ONBUILD preflight.
	lastBuildPullParent bool
	// buildErr, when non-empty, makes ImageBuild's response stream report a build
	// error (simulating e.g. a COPY-source failure).
	buildErr string

	// logsBody, when non-nil, is returned verbatim by ContainerLogs — a test
	// sets this to a properly Docker-multiplexed byte stream (dockerFrame
	// below) to exercise stdcopy demuxing. nil keeps the old plain-text default.
	logsBody []byte

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
	// lastCapAdd/lastCapDrop record hostCfg.CapAdd/CapDrop given to ContainerCreate.
	lastCapAdd  []string
	lastCapDrop []string
	// lastLabels records cfg.Labels given to ContainerCreate.
	lastLabels map[string]string
	// createWarnings is returned as ContainerCreateResult.Warnings, the way a
	// daemon reports a requested limit it discarded.
	createWarnings []string

	// listItems is what ContainerList returns — a test seeds it to simulate
	// containers left over from a prior process (the orphan-sweep scenario).
	listItems []container.Summary
	// lastListFilters/lastListAll record the ContainerList call's options.
	lastListFilters client.Filters
	lastListAll     bool
	// removedIDs records every container ID passed to ContainerRemove, in order.
	removedIDs []string

	// lastImageListHadDeadline records whether the LAST ImageList call's ctx
	// carried a deadline (see ImageList below).
	lastImageListHadDeadline bool

	// removedImages records every ref passed to ImageRemove, in order.
	removedImages []string

	// inspectErrs maps an image ref to the error ImageInspect returns for it,
	// overriding the present/absent answer. A test seeds a NON-NotFound error
	// here to model the daemon failing to answer, which is a different question
	// from "the image is absent" and has to stay one.
	inspectErrs map[string]error

	// remoteDigests maps a ref to the digest DistributionInspect answers; a ref absent here is not found,
	// and remoteErrs overrides that with any other error (the registry unreachable).
	remoteDigests map[string]string
	remoteErrs    map[string]error
	remoteAsked   []string

	// pinFiles maps an output image ref to the pin file a never-started container made from it serves;
	// an image absent here has no such file. pinReadImages and pinReadConfig record what created the
	// pin-read container (the one with the placeholder command), pinReadHost its host config.
	pinFiles       map[string][]byte
	pinReadImages  []string
	pinReadConfig  *container.Config
	pinReadHost    *container.HostConfig
	pinContainerOf map[string]string // container ID -> image it was created from
}

func newFakeEnvbuilderDocker() *fakeEnvbuilderDocker {
	return &fakeEnvbuilderDocker{
		imagesPresent: map[string]bool{"envbuilder:test": true},
	}
}

// pulled reports whether ref was passed to ImagePull.
func (f *fakeEnvbuilderDocker) pulled(ref string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.pulledRefs {
		if r == ref {
			return true
		}
	}
	return false
}

func (f *fakeEnvbuilderDocker) ImageList(ctx context.Context, _ client.ImageListOptions) (client.ImageListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// lastImageListHadDeadline records whether the ctx ensureImage called us
	// with carried a deadline — a test's way of observing that a build path
	// (e.g. FinalizeBase) applied its own timeout bound rather than passing
	// the caller's ctx through unbounded.
	_, f.lastImageListHadDeadline = ctx.Deadline()
	var out []image.Summary
	for ref, present := range f.imagesPresent {
		if !present {
			continue
		}
		out = append(out, summaryForRef(ref))
	}
	return client.ImageListResult{Items: out}, nil
}

// summaryForRef splits a test-seeded ref into the RepoTags/RepoDigests shape a
// REAL daemon reports it under — the fake's whole reason to exist here.
//
// The old collapse ("contains @sha256: => one RepoDigests entry holding the
// whole string") made the fully-qualified `repo:tag@sha256:...` form — the one
// a workspace's resolved BYOI base actually carries — self-matching against
// ensureImage's exact-string scan, so the missing presence check was
// unobtainable as a red test. A real daemon never reports that string: the tag
// and the digest live in two different lists, under two different names
// (`repo:tag` in RepoTags, `repo@sha256:...` in RepoDigests, the tag stripped),
// and NEITHER equals the ref the caller asked about.
func summaryForRef(ref string) image.Summary {
	name, digest, pinned := strings.Cut(ref, "@")
	if !pinned {
		return image.Summary{RepoTags: []string{ref}}
	}
	repo := name
	// A tag is the last ":" AFTER the last "/" (a registry host:port is not one).
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		repo = name[:i]
		return image.Summary{RepoTags: []string{name}, RepoDigests: []string{repo + "@" + digest}}
	}
	return image.Summary{RepoDigests: []string{repo + "@" + digest}}
}

func (f *fakeEnvbuilderDocker) ImagePull(_ context.Context, ref string, _ client.ImagePullOptions) (client.ImagePullResponse, error) {
	f.mu.Lock()
	f.pullCalled = true
	f.pulledRefs = append(f.pulledRefs, ref)
	if f.pullErr != nil {
		f.mu.Unlock()
		return nil, f.pullErr
	}
	if f.imagesPresent == nil {
		f.imagesPresent = map[string]bool{}
	}
	f.imagesPresent[ref] = true
	f.mu.Unlock()
	return fakePullResponse{io.NopCloser(strings.NewReader(`{"status":"pulled"}`))}, nil
}

// ImageInspect reports the image config, carrying any ONBUILD triggers the test
// baked in via onBuild (mirrors a real daemon: triggers surface in Config.OnBuild).
// A bare (no-tag) onBuild key matches ANY tag under that repo (imageID == key,
// or imageID prefixed by "key:") — the devcontainer-build path now resolves its
// FROM through a per-build push ref (Builder.newPushRef) carrying a random tag
// a test cannot predict ahead of a Build() call, so tests seed ONBUILD by bare
// repo instead of the exact ref.
func (f *fakeEnvbuilderDocker) ImageInspect(_ context.Context, imageID string, _ ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspected = append(f.inspected, imageID)
	if err, ok := f.inspectErrs[imageID]; ok {
		return client.ImageInspectResult{}, err
	}
	// A real daemon 404s an inspect of an image it does not have. ensureImage's
	// digest path reads presence THROUGH inspect, so a fake that answered every
	// ref would report every absent digest ref present and never pull.
	if !f.imagesPresent[imageID] {
		return client.ImageInspectResult{}, errdefs.ErrNotFound
	}
	cfg := &dockerspec.DockerOCIImageConfig{}
	for key, triggers := range f.onBuild {
		if imageID == key || strings.HasPrefix(imageID, key+":") {
			cfg.OnBuild = triggers
			break
		}
	}
	return client.ImageInspectResult{
		InspectResponse: image.InspectResponse{ID: imageID, Config: cfg, RepoDigests: f.repoDigests[imageID]},
	}, nil
}

func (f *fakeEnvbuilderDocker) ImageBuild(_ context.Context, buildContext io.Reader, options client.ImageBuildOptions) (client.ImageBuildResult, error) {
	f.mu.Lock()
	f.imageBuildCalled = true
	f.lastBuildTags = options.Tags
	f.lastBuildPullParent = options.PullParent
	buildErr := f.buildErr
	if buildErr == "" {
		// A successful build leaves its output tag resolvable locally, the way a
		// daemon does — so a test can tell "the per-build base was untagged" from
		// "the image we just built was untagged too".
		if f.imagesPresent == nil {
			f.imagesPresent = map[string]bool{}
		}
		for _, tag := range options.Tags {
			f.imagesPresent[tag] = true
		}
	}
	f.mu.Unlock()
	// Read the whole context so the caller's tar writer isn't left dangling, and keep its Dockerfile.
	dockerfile := tarEntry(buildContext, "Dockerfile")
	f.mu.Lock()
	f.lastDockerfile = dockerfile
	noAuxID := f.noAuxID
	f.mu.Unlock()
	body := `{"stream":"Step 1/3 : FROM base\n"}` + "\n"
	if !noAuxID && len(options.Tags) > 0 {
		body += `{"aux":{"ID":` + strconv.Quote(fakeImageID(options.Tags[0])) + `}}` + "\n"
	}
	body += `{"stream":"Successfully built deadbeef\n"}` + "\n"
	if buildErr != "" {
		body = `{"error":` + strconv.Quote(buildErr) + `}` + "\n"
	}
	return client.ImageBuildResult{Body: io.NopCloser(strings.NewReader(body))}, nil
}

func (f *fakeEnvbuilderDocker) ContainerCreate(_ context.Context, opts client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalled++
	if opts.Config != nil && len(opts.Config.Cmd) == 1 && opts.Config.Cmd[0] == "/wardyn-pin-read" {
		id := "fake-pin-container-" + strconv.Itoa(len(f.pinReadImages))
		f.pinReadImages = append(f.pinReadImages, opts.Config.Image)
		f.pinReadConfig, f.pinReadHost = opts.Config, opts.HostConfig
		if f.pinContainerOf == nil {
			f.pinContainerOf = map[string]string{}
		}
		f.pinContainerOf[id] = opts.Config.Image
		return client.ContainerCreateResult{ID: id}, nil
	}
	if opts.Config != nil {
		f.lastEnv = opts.Config.Env
		f.lastLabels = opts.Config.Labels
	}
	if opts.HostConfig != nil {
		f.lastBinds = opts.HostConfig.Binds
		f.lastNetworkMode = opts.HostConfig.NetworkMode
		f.lastResources = opts.HostConfig.Resources
		f.lastStorageOpt = opts.HostConfig.StorageOpt
		f.lastCapAdd = opts.HostConfig.CapAdd
		f.lastCapDrop = opts.HostConfig.CapDrop
	}
	return client.ContainerCreateResult{ID: "fake-build-container", Warnings: append([]string(nil), f.createWarnings...)}, nil
}

// CopyToContainer records the staged build-context tar (drained so callers'
// buffers can be asserted against) and succeeds — the fake's containers are
// never started for real.
func (f *fakeEnvbuilderDocker) CopyToContainer(_ context.Context, containerID string, options client.CopyToContainerOptions) (client.CopyToContainerResult, error) {
	if options.Content != nil {
		b, _ := io.ReadAll(options.Content)
		f.copiedTars = append(f.copiedTars, len(b))
	}
	f.copiedTo = append(f.copiedTo, containerID)
	return client.CopyToContainerResult{}, nil
}

// DistributionInspect answers from remoteDigests, the way a registry does: an unknown ref is not found.
func (f *fakeEnvbuilderDocker) DistributionInspect(_ context.Context, ref string, _ client.DistributionInspectOptions) (client.DistributionInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remoteAsked = append(f.remoteAsked, ref)
	if err, ok := f.remoteErrs[ref]; ok {
		return client.DistributionInspectResult{}, err
	}
	d, ok := f.remoteDigests[ref]
	if !ok {
		return client.DistributionInspectResult{}, errdefs.ErrNotFound
	}
	var out client.DistributionInspectResult
	if err := json.Unmarshal([]byte(`{"Descriptor":{"digest":`+strconv.Quote(d)+`}}`), &out.DistributionInspect); err != nil {
		return client.DistributionInspectResult{}, err
	}
	return out, nil
}

// CopyFromContainer serves pinFiles[image] as the one-entry tar a real daemon returns for a file, and
// not-found for an image without it.
func (f *fakeEnvbuilderDocker) CopyFromContainer(_ context.Context, containerID string, opts client.CopyFromContainerOptions) (client.CopyFromContainerResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	payload, ok := f.pinFiles[f.pinContainerOf[containerID]]
	if !ok || opts.SourcePath != pinFilePath {
		return client.CopyFromContainerResult{}, errdefs.ErrNotFound
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "claude-code", Mode: 0o644, Size: int64(len(payload))})
	_, _ = tw.Write(payload)
	_ = tw.Close()
	return client.CopyFromContainerResult{Content: io.NopCloser(&buf)}, nil
}

func (f *fakeEnvbuilderDocker) ContainerStart(_ context.Context, _ string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalled++
	return client.ContainerStartResult{}, nil
}

func (f *fakeEnvbuilderDocker) ContainerLogs(_ context.Context, _ string, _ client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
	f.mu.Lock()
	body := f.logsBody
	f.mu.Unlock()
	if body == nil {
		return io.NopCloser(strings.NewReader("build log output\n")), nil
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

// dockerFrame builds one Docker log-stream frame: a real (non-TTY)
// ContainerLogs response multiplexes stdout/stderr behind this 8-byte
// header (1-byte stream type, 3 bytes zeroed, 4-byte big-endian payload
// length) per chunk — see stdcopy.StdCopy, the demuxer that must strip it.
func dockerFrame(streamType byte, payload string) []byte {
	buf := make([]byte, 8+len(payload))
	buf[0] = streamType
	binary.BigEndian.PutUint32(buf[4:8], uint32(len(payload)))
	copy(buf[8:], payload)
	return buf
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

func (f *fakeEnvbuilderDocker) ContainerRemove(_ context.Context, containerID string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = true
	f.removedIDs = append(f.removedIDs, containerID)
	return client.ContainerRemoveResult{}, nil
}

// ImageRemove untags ref, the way `docker rmi <tag>` does: the tag stops
// resolving, every OTHER tag on the same image keeps resolving. Recorded so a
// test can assert WHICH ref was reclaimed, not merely that something was.
func (f *fakeEnvbuilderDocker) ImageRemove(_ context.Context, ref string, _ client.ImageRemoveOptions) (client.ImageRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedImages = append(f.removedImages, ref)
	if !f.imagesPresent[ref] {
		return client.ImageRemoveResult{}, errdefs.ErrNotFound
	}
	delete(f.imagesPresent, ref)
	return client.ImageRemoveResult{}, nil
}

// removedImage reports whether ref was passed to ImageRemove.
func (f *fakeEnvbuilderDocker) removedImage(ref string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.removedImages {
		if r == ref {
			return true
		}
	}
	return false
}

// ContainerList returns the test-seeded listItems, recording the call's
// filters/All so SweepOrphanedBuilds' label-filtered, All:true scan can be
// pinned.
func (f *fakeEnvbuilderDocker) ContainerList(_ context.Context, options client.ContainerListOptions) (client.ContainerListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastListFilters = options.Filters
	f.lastListAll = options.All
	return client.ContainerListResult{Items: f.listItems}, nil
}

// fakeImageID is the deterministic image ID the fake's build stream reports for a tag.
func fakeImageID(tag string) string {
	sum := sha256.Sum256([]byte(tag))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// tarEntry returns the named entry of a tar stream ("" when absent), draining the rest.
func tarEntry(r io.Reader, name string) string {
	var found string
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(tr)
		if h.Name == name {
			found = string(b)
		}
	}
	_, _ = io.Copy(io.Discard, r)
	return found
}
