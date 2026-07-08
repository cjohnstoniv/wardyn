// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/system"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// fakeNotFound is a Docker-shaped not-found error: IsErrNotFound matches on
// the NotFound() method, which errdefs recognizes.
type fakeNotFound struct{ msg string }

func (e fakeNotFound) Error() string { return e.msg }
func (e fakeNotFound) NotFound()     {}

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

	networks   map[string]network.CreateOptions // name -> opts (id == name here)
	netRemoved map[string]bool

	containers  map[string]*createdContainer // id (== name) -> record
	createCalls []string                     // names in creation order
	startOrder  []string

	// failpoints
	failCreateContainer string // name prefix that should fail on create
	failConnectNetwork  string // network name that should fail on connect
	failNetworkCreate   bool
	infoErr             error

	execs       map[string]bool // execID -> created
	lastExecCmd []string        // argv of the most recent exec
	// lastResize records the most recent ContainerExecResize options so attach
	// tests can assert the PTY was resized.
	lastResize *container.ResizeOptions
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		info:       infoWithRuntimes(),
		images:     map[string]bool{},
		networks:   map[string]network.CreateOptions{},
		netRemoved: map[string]bool{},
		containers: map[string]*createdContainer{},
		execs:      map[string]bool{},
	}
}

func (f *fakeDocker) Info(ctx context.Context) (system.Info, error) {
	if f.infoErr != nil {
		return system.Info{}, f.infoErr
	}
	return f.info, nil
}

func (f *fakeDocker) ImageList(ctx context.Context, opts image.ListOptions) ([]image.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// The driver passes a reference filter; we just report presence for any
	// image marked present.
	for ref, present := range f.images {
		if present {
			return []image.Summary{{ID: ref}}, nil
		}
	}
	return nil, nil
}

func (f *fakeDocker) ImagePull(ctx context.Context, ref string, opts image.PullOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	f.images[ref] = true
	f.mu.Unlock()
	return io.NopCloser(strings.NewReader(`{"status":"pulled"}`)), nil
}

func (f *fakeDocker) NetworkCreate(ctx context.Context, name string, opts network.CreateOptions) (network.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNetworkCreate {
		return network.CreateResponse{}, fmt.Errorf("boom: network create")
	}
	f.networks[name] = opts
	return network.CreateResponse{ID: name}, nil
}

func (f *fakeDocker) NetworkConnect(ctx context.Context, networkID, containerID string, cfg *network.EndpointSettings) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failConnectNetwork != "" && networkID == f.failConnectNetwork {
		return fmt.Errorf("boom: connect %s", networkID)
	}
	c := f.containers[containerID]
	if c == nil {
		return fakeNotFound{msg: "no such container: " + containerID}
	}
	c.connectedTo = append(c.connectedTo, networkID)
	return nil
}

func (f *fakeDocker) NetworkRemove(ctx context.Context, networkID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.networks[networkID]; !ok {
		return fakeNotFound{msg: "no such network: " + networkID}
	}
	delete(f.networks, networkID)
	f.netRemoved[networkID] = true
	return nil
}

func (f *fakeDocker) ContainerCreate(ctx context.Context, cfg *container.Config, host *container.HostConfig, netCfg *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCreateContainer != "" && strings.HasPrefix(name, f.failCreateContainer) {
		return container.CreateResponse{}, fmt.Errorf("boom: create %s", name)
	}
	f.containers[name] = &createdContainer{
		name:  name,
		cfg:   cfg,
		host:  host,
		net:   netCfg,
		state: &container.State{Status: "created"},
	}
	f.createCalls = append(f.createCalls, name)
	return container.CreateResponse{ID: name}, nil
}

func (f *fakeDocker) ContainerStart(ctx context.Context, id string, opts container.StartOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[id]
	if c == nil {
		return fakeNotFound{msg: "no such container: " + id}
	}
	c.state = &container.State{Status: "running", Running: true}
	f.startOrder = append(f.startOrder, id)
	return nil
}

func (f *fakeDocker) ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[id]
	if c == nil || c.removed {
		return container.InspectResponse{}, fakeNotFound{msg: "no such container: " + id}
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
			nets[name] = &network.EndpointSettings{IPAddress: "10.88.0.2", NetworkID: name}
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
	return container.InspectResponse{
		// Real Docker reports the name with a leading slash; mirror that so
		// name-based run-id recovery is exercised faithfully.
		ContainerJSONBase: &container.ContainerJSONBase{ID: id, Name: "/" + c.name, State: c.state},
		Config:            c.cfg,
		NetworkSettings:   &container.NetworkSettings{Networks: nets},
	}, nil
}

func (f *fakeDocker) ContainerStop(ctx context.Context, id string, opts container.StopOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[id]
	if c == nil || c.removed {
		return fakeNotFound{msg: "no such container: " + id}
	}
	c.state = &container.State{Status: "exited", ExitCode: 0}
	return nil
}

func (f *fakeDocker) ContainerKill(ctx context.Context, id, signal string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[id]
	if c == nil || c.removed {
		return fakeNotFound{msg: "no such container: " + id}
	}
	c.state = &container.State{Status: "exited", ExitCode: 137}
	return nil
}

func (f *fakeDocker) ContainerRemove(ctx context.Context, id string, opts container.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[id]
	if c == nil {
		return fakeNotFound{msg: "no such container: " + id}
	}
	c.removed = true
	return nil
}

// ContainerWait yields the container's exit code (from its recorded state, or 0
// if it never exited), mirroring dockerd's WaitConditionNotRunning behaviour of
// returning immediately for an already-exited container. Used by the exec-less
// (main-process) Wait path.
func (f *fakeDocker) ContainerWait(ctx context.Context, id string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	statusCh := make(chan container.WaitResponse, 1)
	errCh := make(chan error, 1)
	f.mu.Lock()
	c, ok := f.containers[id]
	f.mu.Unlock()
	if !ok || c == nil || c.removed {
		errCh <- fakeNotFound{msg: "no such container: " + id}
		return statusCh, errCh
	}
	var code int64
	if c.state != nil {
		code = int64(c.state.ExitCode)
	}
	statusCh <- container.WaitResponse{StatusCode: code}
	return statusCh, errCh
}

func (f *fakeDocker) ContainerExecCreate(ctx context.Context, id string, opts container.ExecOptions) (container.ExecCreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.containers[id]; c == nil || c.removed {
		return container.ExecCreateResponse{}, fakeNotFound{msg: "no such container: " + id}
	}
	execID := "exec-" + id
	f.execs[execID] = true
	// stash last exec opts for assertion
	f.lastExecCmd = opts.Cmd
	return container.ExecCreateResponse{ID: execID}, nil
}

func (f *fakeDocker) ContainerExecAttach(ctx context.Context, execID string, opts container.ExecAttachOptions) (dockertypes.HijackedResponse, error) {
	return dockertypes.NewHijackedResponse(fakeConn{}, "application/vnd.docker.raw-stream"), nil
}

func (f *fakeDocker) ContainerExecStart(ctx context.Context, execID string, opts container.ExecStartOptions) error {
	return nil
}

func (f *fakeDocker) ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error) {
	return container.ExecInspect{ExecID: execID, Running: true}, nil
}

func (f *fakeDocker) ContainerExecResize(ctx context.Context, execID string, opts container.ResizeOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := opts
	f.lastResize = &o
	return nil
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
