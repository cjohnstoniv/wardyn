// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"
	"io"
	"iter"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"
)

// fakeNotFound is a Docker-shaped not-found error: the driver's isNotFound now
// classifies via containerd errdefs.IsNotFound, which recognizes any error
// implementing the NotFound() marker method (the same shape the moby v29 client
// returns for a 404).
type fakeNotFound struct{ msg string }

func (e fakeNotFound) Error() string { return e.msg }
func (e fakeNotFound) NotFound()     {}

// fakePullResponse adapts an io.ReadCloser to client.ImagePullResponse (the v29
// ImagePull return type). PullImage only drains the reader, so the extra
// progress helpers are inert no-ops.
type fakePullResponse struct{ io.ReadCloser }

func (fakePullResponse) JSONMessages(context.Context) iter.Seq2[jsonstream.Message, error] {
	return nil
}
func (fakePullResponse) Wait(context.Context) error { return nil }

// createdContainer records a ContainerCreate call for assertions.
type createdContainer struct {
	name string
	cfg  *container.Config
	host *container.HostConfig
	net  *network.NetworkingConfig
	// connectedTo lists networks NetworkConnect attached this container to,
	// in order.
	connectedTo []string
	state       *container.State
	removed     bool
}

// fakeDocker is an in-memory dockerAPI for unit tests. It is concurrency-safe
// because the Runner contract requires concurrent safety.
type fakeDocker struct {
	mu sync.Mutex

	info system.Info

	images map[string]bool // ref -> present

	networks map[string]client.NetworkCreateOptions // name -> opts (id == name here)

	containers map[string]*createdContainer // id (== name) -> record

	// failpoints
	failCreateContainer string   // name prefix that should fail on create
	failImagePull       bool     // ImagePull returns an error (image absent + unpullable)
	createWarnings      []string // Warnings the ContainerCreate response carries (e.g. a discarded resource limit)
	// onCreate, when set, runs INSIDE ContainerCreate (before the container is
	// recorded) so a test can interleave another driver call with an in-flight
	// create. Called without f.mu held.
	onCreate func(name string)
	// execInspectErrs / waitErrs make the next N ExecInspect / ContainerWait
	// probes fail with a transient (non-not-found) error, modelling a daemon blip.
	execInspectErrs int
	waitErrs        int
	// execExitCode is reported once execInspectErrs is exhausted; when
	// execExited is true ExecInspect reports the process as finished.
	execExitCode int
	execExited   bool
	// execGone is an exec id ExecInspect reports as not-found (the authoritative
	// "it is really gone", as opposed to the transient execInspectErrs blip).
	execGone string

	lastExecCmd []string // argv of the most recent exec
	// lastResize records the most recent ExecResize options so attach
	// tests can assert the PTY was resized.
	lastResize *client.ExecResizeOptions
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		info:       infoWithRuntimes(),
		images:     map[string]bool{},
		networks:   map[string]client.NetworkCreateOptions{},
		containers: map[string]*createdContainer{},
	}
}

func (f *fakeDocker) Info(ctx context.Context, _ client.InfoOptions) (client.SystemInfoResult, error) {
	return client.SystemInfoResult{Info: f.info}, nil
}

func (f *fakeDocker) ImageList(ctx context.Context, _ client.ImageListOptions) (client.ImageListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// The driver passes a reference filter; we just report presence for any
	// image marked present.
	for ref, present := range f.images {
		if present {
			return client.ImageListResult{Items: []image.Summary{{ID: ref}}}, nil
		}
	}
	return client.ImageListResult{}, nil
}

func (f *fakeDocker) ImagePull(ctx context.Context, ref string, _ client.ImagePullOptions) (client.ImagePullResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failImagePull {
		return nil, fmt.Errorf("registry: denied")
	}
	f.images[ref] = true
	return fakePullResponse{io.NopCloser(strings.NewReader(`{"status":"pulled"}`))}, nil
}

func (f *fakeDocker) ImageInspect(ctx context.Context, imageID string, _ ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.images[imageID] {
		return client.ImageInspectResult{InspectResponse: image.InspectResponse{ID: imageID}}, nil
	}
	return client.ImageInspectResult{}, fakeNotFound{msg: "no such image: " + imageID}
}

func (f *fakeDocker) NetworkCreate(ctx context.Context, name string, opts client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.networks[name] = opts
	return client.NetworkCreateResult{ID: name}, nil
}

func (f *fakeDocker) NetworkConnect(ctx context.Context, networkID string, opts client.NetworkConnectOptions) (client.NetworkConnectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[opts.Container]
	if c == nil {
		return client.NetworkConnectResult{}, fakeNotFound{msg: "no such container: " + opts.Container}
	}
	c.connectedTo = append(c.connectedTo, networkID)
	return client.NetworkConnectResult{}, nil
}

func (f *fakeDocker) NetworkRemove(ctx context.Context, networkID string, _ client.NetworkRemoveOptions) (client.NetworkRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.networks[networkID]; !ok {
		return client.NetworkRemoveResult{}, fakeNotFound{msg: "no such network: " + networkID}
	}
	delete(f.networks, networkID)
	return client.NetworkRemoveResult{}, nil
}

func (f *fakeDocker) ContainerCreate(ctx context.Context, opts client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	f.mu.Lock()
	hook := f.onCreate
	f.mu.Unlock()
	if hook != nil {
		hook(opts.Name)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	name := opts.Name
	if f.failCreateContainer != "" && strings.HasPrefix(name, f.failCreateContainer) {
		return client.ContainerCreateResult{}, fmt.Errorf("boom: create %s", name)
	}
	f.containers[name] = &createdContainer{
		name:  name,
		cfg:   opts.Config,
		host:  opts.HostConfig,
		net:   opts.NetworkingConfig,
		state: &container.State{Status: "created"},
	}
	// createWarnings simulates a daemon that discarded a requested limit (e.g. a
	// cgroup-v1-rootless host) — surfaced in the create response like real Moby.
	return client.ContainerCreateResult{ID: name, Warnings: f.createWarnings}, nil
}

func (f *fakeDocker) ContainerStart(ctx context.Context, id string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[id]
	if c == nil {
		return client.ContainerStartResult{}, fakeNotFound{msg: "no such container: " + id}
	}
	c.state = &container.State{Status: "running", Running: true}
	return client.ContainerStartResult{}, nil
}

func (f *fakeDocker) ContainerInspect(ctx context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[id]
	if c == nil || c.removed {
		return client.ContainerInspectResult{}, fakeNotFound{msg: "no such container: " + id}
	}
	// Synthesize NetworkSettings from the container's known networks (primary
	// NetworkMode + explicit endpoints + NetworkConnect'd nets) with a
	// deterministic placeholder IP, so the driver can read the proxy's IP (used
	// to pin it in the agent's /etc/hosts). Gateway is left empty; the real
	// gatewayless-L0 assertion runs against a real docker client (network_test).
	nets := map[string]*network.EndpointSettings{}
	addNet := func(name string) {
		if name == "" || name == "none" || name == "host" || name == "bridge" || name == "default" {
			return
		}
		if _, ok := nets[name]; !ok {
			nets[name] = &network.EndpointSettings{IPAddress: netip.MustParseAddr("10.88.0.2"), NetworkID: name}
		}
	}
	if c.host != nil {
		addNet(string(c.host.NetworkMode))
	}
	if c.net != nil {
		for name := range c.net.EndpointsConfig {
			addNet(name)
		}
	}
	for _, n := range c.connectedTo {
		addNet(n)
	}
	return client.ContainerInspectResult{Container: container.InspectResponse{
		// Real Docker reports the name with a leading slash; mirror that so
		// name-based run-id recovery is exercised faithfully. (v29 inlined the
		// old ContainerJSONBase fields onto InspectResponse.)
		ID:              id,
		Name:            "/" + c.name,
		State:           c.state,
		Config:          c.cfg,
		NetworkSettings: &container.NetworkSettings{Networks: nets},
	}}, nil
}

func (f *fakeDocker) ContainerStop(ctx context.Context, id string, _ client.ContainerStopOptions) (client.ContainerStopResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[id]
	if c == nil || c.removed {
		return client.ContainerStopResult{}, fakeNotFound{msg: "no such container: " + id}
	}
	c.state = &container.State{Status: "exited", ExitCode: 0}
	return client.ContainerStopResult{}, nil
}

func (f *fakeDocker) ContainerKill(ctx context.Context, id string, _ client.ContainerKillOptions) (client.ContainerKillResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[id]
	if c == nil || c.removed {
		return client.ContainerKillResult{}, fakeNotFound{msg: "no such container: " + id}
	}
	c.state = &container.State{Status: "exited", ExitCode: 137}
	return client.ContainerKillResult{}, nil
}

func (f *fakeDocker) ContainerRemove(ctx context.Context, id string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[id]
	if c == nil {
		return client.ContainerRemoveResult{}, fakeNotFound{msg: "no such container: " + id}
	}
	c.removed = true
	return client.ContainerRemoveResult{}, nil
}

// ContainerWait yields the container's exit code (from its recorded state, or 0
// if it never exited), mirroring dockerd's WaitConditionNotRunning behaviour of
// returning immediately for an already-exited container. Used by the exec-less
// (main-process) Wait path. v29 returns both channels wrapped in a
// ContainerWaitResult.
func (f *fakeDocker) ContainerWait(ctx context.Context, id string, _ client.ContainerWaitOptions) client.ContainerWaitResult {
	statusCh := make(chan container.WaitResponse, 1)
	errCh := make(chan error, 1)
	f.mu.Lock()
	if f.waitErrs > 0 {
		f.waitErrs--
		f.mu.Unlock()
		// A daemon blip: NOT a not-found (the container still exists).
		errCh <- fmt.Errorf("Cannot connect to the Docker daemon: EOF")
		return client.ContainerWaitResult{Result: statusCh, Error: errCh}
	}
	c, ok := f.containers[id]
	f.mu.Unlock()
	if !ok || c == nil || c.removed {
		errCh <- fakeNotFound{msg: "no such container: " + id}
		return client.ContainerWaitResult{Result: statusCh, Error: errCh}
	}
	var code int64
	if c.state != nil {
		code = int64(c.state.ExitCode)
	}
	statusCh <- container.WaitResponse{StatusCode: code}
	return client.ContainerWaitResult{Result: statusCh, Error: errCh}
}

func (f *fakeDocker) ExecCreate(ctx context.Context, id string, opts client.ExecCreateOptions) (client.ExecCreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.containers[id]; c == nil || c.removed {
		return client.ExecCreateResult{}, fakeNotFound{msg: "no such container: " + id}
	}
	execID := "exec-" + id
	// stash last exec opts for assertion
	f.lastExecCmd = opts.Cmd
	return client.ExecCreateResult{ID: execID}, nil
}

func (f *fakeDocker) ExecAttach(ctx context.Context, execID string, opts client.ExecAttachOptions) (client.ExecAttachResult, error) {
	return client.ExecAttachResult{
		HijackedResponse: client.NewHijackedResponse(fakeConn{}, "application/vnd.docker.raw-stream"),
	}, nil
}

func (f *fakeDocker) ExecStart(ctx context.Context, execID string, opts client.ExecStartOptions) (client.ExecStartResult, error) {
	return client.ExecStartResult{}, nil
}

func (f *fakeDocker) ExecInspect(ctx context.Context, execID string, _ client.ExecInspectOptions) (client.ExecInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.execGone != "" && execID == f.execGone {
		return client.ExecInspectResult{}, fakeNotFound{msg: "no such exec: " + execID}
	}
	if f.execInspectErrs > 0 {
		f.execInspectErrs--
		// A daemon blip: NOT a not-found (the exec still exists).
		return client.ExecInspectResult{}, fmt.Errorf("Cannot connect to the Docker daemon: EOF")
	}
	return client.ExecInspectResult{ID: execID, Running: !f.execExited, ExitCode: f.execExitCode}, nil
}

func (f *fakeDocker) ExecResize(ctx context.Context, execID string, opts client.ExecResizeOptions) (client.ExecResizeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := opts
	f.lastResize = &o
	return client.ExecResizeResult{}, nil
}

// fakeConn is a net.Conn whose reads return EOF immediately, so the Exec
// drain goroutine completes promptly.
type fakeConn struct{}

func (fakeConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (fakeConn) Write(b []byte) (int, error)      { return len(b), nil }
func (fakeConn) Close() error                     { return nil }
func (fakeConn) LocalAddr() net.Addr              { return fakeAddr{} }
func (fakeConn) RemoteAddr() net.Addr             { return fakeAddr{} }
func (fakeConn) SetDeadline(time.Time) error      { return nil }
func (fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (fakeConn) SetWriteDeadline(time.Time) error { return nil }

type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }
