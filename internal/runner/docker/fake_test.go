// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/api/types/volume"
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
	// forceRemoved records whether the removal asked for Force, which a
	// container that may be running needs.
	forceRemoved bool
}

// fakeDocker is an in-memory dockerAPI for unit tests. It is concurrency-safe
// because the Runner contract requires concurrent safety.
type fakeDocker struct {
	mu sync.Mutex

	info system.Info
	// pingErr is what Ping answers; pingBlock makes it wait for ctx instead.
	pingErr   error
	pingBlock bool

	images map[string]bool // ref -> present

	networks map[string]client.NetworkCreateOptions // name -> opts (id == name here)
	// subnets is each network's IPv4 subnet: the one its create asked for, or
	// else the first free 10.N.0.0/16 from N=88, as the daemon picks one. A
	// create asking for a subnet another network holds fails on the overlap.
	subnets map[string]netip.Prefix
	// onNetworkCreate, when set, runs INSIDE NetworkCreate before the network
	// is recorded, so a test can have another network take a subnet first.
	// Called without f.mu held.
	onNetworkCreate func(name string, opts client.NetworkCreateOptions)

	containers map[string]*createdContainer // id (== name) -> record

	// stats is the canned ContainerStats body per container id; statsCalls records every read's options.
	stats      map[string]container.StatsResponse
	statsCalls []client.ContainerStatsOptions

	// stdin is what was written to each container's stdin through
	// ContainerAttach (the proxy's config, #1176), by container id; attached
	// lists every attach in order, with started showing whether the container
	// had already been started when it was made.
	stdin    map[string][]byte
	attached []fakeAttach

	// startedNames records every ContainerStart in order, and SURVIVES rollback
	// (unlike containers, which a rollback removes). Lets a test prove a container
	// was never started, not merely started-then-reaped.
	startedNames []string
	// copies records every CopyToContainer, in order and interleaved with
	// startedNames by way of copiedBeforeStart: the managed-file contract is
	// not "the archive was sent" but "the archive was sent BEFORE the agent
	// could run", and only the ordering proves that.
	copies []fakeCopy
	// failCopyToContainer makes every CopyToContainer fail, so a test can prove
	// the sandbox is torn down rather than started without its ceiling.
	failCopyToContainer bool
	// existingPaths are the in-container paths ContainerStatPath reports as
	// present; every other path is not-found, as a fresh image's is.
	existingPaths map[string]bool
	// imageUsers is each image's USER, merged into a created container's
	// config when the create leaves User empty, as the daemon does.
	imageUsers map[string]string
	// etcDir is the /etc entry CopyFromContainer reports (nil: a root-owned
	// 0755 directory), and passwd is /etc/passwd's content ("" : absent).
	etcDir *tar.Header
	passwd string
	// runDir is the /run entry CopyFromContainer reports (nil: absent, as in
	// an image that ships no /run).
	runDir *tar.Header

	// failpoints
	failCreateContainer string   // name prefix that should fail on create
	failImagePull       bool     // ImagePull returns an error (image absent + unpullable)
	createWarnings      []string // Warnings the ContainerCreate response carries (e.g. a discarded resource limit)
	// createIDOverride, when set, is returned as ContainerCreate's ID instead of
	// echoing Name — a real daemon's ID is an opaque hash unrelated to the name
	// (this fake's default "id == name" is a simplification real Docker does not
	// share). The container stays looked-up-able under BOTH keys, matching a
	// real daemon accepting either as the "{id}" path param.
	createIDOverride string
	// onCreate, when set, runs INSIDE ContainerCreate (before the container is
	// recorded) so a test can interleave another driver call with an in-flight
	// create. Called without f.mu held.
	onCreate func(name string)
	// onPull, when set, runs INSIDE ImagePull so a test can prove the "this is
	// downloading" report reached the caller BEFORE the call that blocks rather
	// than after it, which is the whole value of the report. Called without
	// f.mu held.
	onPull func(ref string)
	// execInspectErrs / waitErrs make the next N ExecInspectRaw / ContainerWait
	// probes fail with a transient (non-not-found) error, modelling a daemon blip.
	execInspectErrs int
	waitErrs        int
	// execExitCode is reported once execInspectErrs is exhausted; when
	// execExited is true ExecInspectRaw reports the process as finished.
	execExitCode int
	execExited   bool
	// execUnstarted makes the next N ExecInspectRaw probes report an exec the
	// daemon has created but not started: not running, a null exit code, no pid.
	execUnstarted int
	// execStartRefused reports a start the daemon refused (binary missing, not
	// executable): finished with execExitCode and no pid.
	execStartRefused bool
	// execPIDZeroedOnExit models Podman's Docker-compatible API, which zeroes
	// an exec's pid when it exits (and always sends an exit code).
	execPIDZeroedOnExit bool
	// execInspects counts the exec inspects served.
	execInspects int
	// execGone is an exec id ExecInspectRaw reports as not-found (the authoritative
	// "it is really gone", as opposed to the transient execInspectErrs blip).
	execGone string

	lastExecCmd []string // argv of the most recent exec
	// lastExecOpts records the full ExecCreateOptions of the most recent exec
	// (lastExecCmd predates this and stays as the narrow, common-case reader),
	// so a test can also assert TTY/Env were passed through correctly.
	lastExecOpts client.ExecCreateOptions
	// lastResize records the most recent ExecResize options so attach
	// tests can assert the PTY was resized.
	lastResize *client.ExecResizeOptions
	// execAttachConn, when non-nil, is returned as the hijacked connection's
	// Conn by ExecAttach INSTEAD of the default fakeConn{} — lets a test
	// script real bytes through the hijack (e.g. a stdcopy-multiplexed
	// stream, or a conn that records CloseWrite) rather than the default
	// instant-EOF stub. nil (the default) preserves every existing test's
	// behavior unchanged.
	execAttachConn net.Conn

	// volumes are the named volumes the daemon holds (name -> the create
	// options it was made with, so a test can assert the driver and labels a
	// user drive's volume carries). Pre-seed an entry to model a volume that
	// already exists.
	volumes map[string]client.VolumeCreateOptions
	// volumeCreates counts VolumeCreate calls, so a test can prove the SECOND
	// run against one drive creates nothing (idempotence is the contract, not
	// merely the observable end state).
	volumeCreates int
	// failVolumeInspect makes VolumeInspect return a NON-not-found error — the
	// "the daemon cannot say whether it exists" arm, which must fail closed
	// rather than create over the top of it.
	failVolumeInspect bool
	// volumeInspectMissesExisting makes VolumeInspect answer NOT-FOUND for a
	// volume this fake actually holds — the FIRST-RUN RACE, and the only way to
	// reach ensureDriveVolume's create-then-verify arm: two colliding drives
	// both probe before either creates, so the loser's inspect legitimately saw
	// nothing and its VolumeCreate is then handed the winner's volume.
	volumeInspectMissesExisting bool
	// failVolumeCreate makes VolumeCreate fail (quota, driver refusal).
	failVolumeCreate bool
	// volumeRemoves records every VolumeRemove call, so a test can prove a
	// REFUSED reclaim issued none at all — the assertion that matters most,
	// since the call it is refusing is irreversible.
	volumeRemoves []string
	// lastVolumeRemoveForce pins that the driver never sets Force: a forced
	// remove would pull a member's storage out from under a live agent.
	lastVolumeRemoveForce bool
	// volumeInUse names the one volume whose removal answers CONFLICT, the way
	// a real daemon refuses a volume a container still mounts.
	volumeInUse string

	// listItems is what ContainerList answers — a test seeds it to model the
	// daemon's view for SweepOrphanedSandboxes. lastListFilters/lastListAll
	// record the call, so the label-filtered All:true scan is pinnable.
	listItems       []container.Summary
	lastListFilters client.Filters
	lastListAll     bool

	// probeExitCode is the exit code ContainerWait reports for a container
	// whose name carries the drive-probe prefix ("wardyn-drive-probe-") —
	// there is no real command interpreter here to run `test -r/-x` against a
	// bind mount, so a ProbeDrive test scripts the answer this way instead.
	// Zero (readable) unless a test overrides it.
	probeExitCode int64

	// exitAfterInspects, keyed by container name, makes ContainerInspect report
	// Running for that many calls and then flip the container to exited(1) on
	// every call after — the shape of a real wardyn-proxy that Docker already
	// reports Running (the daemon flips State.Running the INSTANT
	// ContainerStart returns) but which dies a beat later refusing its own
	// config: against a real daemon, a watch that trusted the FIRST Running
	// sighting caught that failure only ~2/10 times (F1). 0 means "already
	// exited by the first inspect". inspectCounts is this field's own
	// per-container call counter. logs pairs the same container names to what
	// ContainerLogs answers, so startProxy's exit-watch log tail is
	// exercisable.
	exitAfterInspects map[string]int
	inspectCounts     map[string]int
	logs              map[string][]byte
	logOpts           []client.ContainerLogsOptions // every ContainerLogs call's options, in order

	// startedAtOverride, keyed by container name, overrides ContainerStart's
	// default (real, current-time) StartedAt for that container — used to
	// simulate a daemon whose clock lags the host's (L1): a test sets an
	// in-the-past value before Start to prove watchProxyExit measures its
	// settle window from the watch's own start, never from a skewed
	// StartedAt (driver_proxy_revive.go's proxySettleSince).
	startedAtOverride map[string]time.Time
}

// ContainerList makes this fake a containerListerAPI, the narrow seam
// SweepOrphanedSandboxes type-asserts for. The label filter is RECORDED rather
// than applied: the sweep's own skips (unparseable run id, age, live run) are
// what the tests drive, and pre-filtering here would hide them.
func (f *fakeDocker) ContainerList(_ context.Context, opts client.ContainerListOptions) (client.ContainerListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastListFilters = opts.Filters
	f.lastListAll = opts.All
	return client.ContainerListResult{Items: f.listItems}, nil
}

var _ containerListerAPI = (*fakeDocker)(nil)

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		info:       infoWithRuntimes(),
		images:     map[string]bool{},
		networks:   map[string]client.NetworkCreateOptions{},
		subnets:    map[string]netip.Prefix{},
		containers: map[string]*createdContainer{},
		volumes:    map[string]client.VolumeCreateOptions{},
		stdin:      map[string][]byte{},
	}
}

// fakeAttach records one ContainerAttach.
type fakeAttach struct {
	id      string
	started bool
}

// ContainerAttach hands back a connection whose writes land in f.stdin[id].
func (f *fakeDocker) ContainerAttach(_ context.Context, id string, opts client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[id]
	if c == nil || c.removed {
		return client.ContainerAttachResult{}, fakeNotFound{msg: "no such container: " + id}
	}
	f.attached = append(f.attached, fakeAttach{id: id, started: c.state != nil && c.state.Running})
	return client.ContainerAttachResult{
		HijackedResponse: client.NewHijackedResponse(&fakeStdinConn{f: f, id: id}, "application/vnd.docker.raw-stream"),
	}, nil
}

// fakeStdinConn is an attach connection that records what is written to it.
type fakeStdinConn struct {
	fakeConn
	f  *fakeDocker
	id string
}

func (c *fakeStdinConn) Write(b []byte) (int, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.stdin[c.id] = append(c.f.stdin[c.id], b...)
	return len(b), nil
}

func (c *fakeStdinConn) CloseWrite() error { return nil }

// stdinOf is what was written to container id's stdin.
func (f *fakeDocker) stdinOf(id string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stdin[id]
}

func (f *fakeDocker) Info(ctx context.Context, _ client.InfoOptions) (client.SystemInfoResult, error) {
	return client.SystemInfoResult{Info: f.info}, nil
}

// Ping answers pingErr: a test sets it to make the daemon refuse, or hang until
// its ctx ends when pingBlock is set.
func (f *fakeDocker) Ping(ctx context.Context, _ client.PingOptions) (client.PingResult, error) {
	if f.pingBlock {
		<-ctx.Done()
		return client.PingResult{}, ctx.Err()
	}
	return client.PingResult{}, f.pingErr
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
	if f.onPull != nil {
		f.onPull(ref)
	}
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

func (f *fakeDocker) ImageRemove(ctx context.Context, imageID string, _ client.ImageRemoveOptions) (client.ImageRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.images[imageID] {
		return client.ImageRemoveResult{}, fakeNotFound{msg: "no such image: " + imageID}
	}
	delete(f.images, imageID)
	return client.ImageRemoveResult{}, nil
}

func (f *fakeDocker) NetworkCreate(ctx context.Context, name string, opts client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
	f.mu.Lock()
	hook := f.onNetworkCreate
	f.mu.Unlock()
	if hook != nil {
		hook(name, opts)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var subnet netip.Prefix
	if opts.IPAM != nil && len(opts.IPAM.Config) > 0 {
		subnet = opts.IPAM.Config[0].Subnet
		for other, held := range f.subnets {
			if held == subnet {
				return client.NetworkCreateResult{}, fmt.Errorf("invalid pool request: Pool overlaps with other one on this address space (%s)", other)
			}
		}
	} else {
		for n := 88; !subnet.IsValid() || slices.Contains(slices.Collect(maps.Values(f.subnets)), subnet); n++ {
			subnet = netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(n), 0, 0}), 16)
		}
	}
	f.networks[name] = opts
	f.subnets[name] = subnet
	return client.NetworkCreateResult{ID: name}, nil
}

func (f *fakeDocker) NetworkInspect(ctx context.Context, networkID string, _ client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.networks[networkID]; !ok {
		return client.NetworkInspectResult{}, fakeNotFound{msg: "no such network: " + networkID}
	}
	var res client.NetworkInspectResult
	res.Network.Name = networkID
	res.Network.IPAM.Config = []network.IPAMConfig{{Subnet: f.subnets[networkID]}}
	return res, nil
}

// refusePinWithoutSubnet reproduces Docker Engine 28's create-time rule
// (moby daemon/container_operations.go validateEndpointIPAddress): an endpoint
// pinned to an IPv4 address is refused on a network whose create named no
// subnet. Engine 29 accepts it, which is why a real Docker Desktop cannot show it.
func (f *fakeDocker) refusePinWithoutSubnet(nc *network.NetworkingConfig) error {
	if nc == nil {
		return nil
	}
	for name, ep := range nc.EndpointsConfig {
		opts, known := f.networks[name]
		if !known || ep == nil || ep.IPAMConfig == nil || !ep.IPAMConfig.IPv4Address.IsValid() {
			continue
		}
		if opts.IPAM == nil || len(opts.IPAM.Config) == 0 {
			return errors.New("invalid endpoint settings:\nuser specified IP address is supported only when connecting to networks with user configured subnets")
		}
	}
	return nil
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
	delete(f.subnets, networkID)
	return client.NetworkRemoveResult{}, nil
}

// refuseUnsupportedRRO reproduces the ONE create-time rule a fake daemon has to
// carry for the drive's read-only bind to be testable at all: moby refuses a
// create whose mount asks for BindOptions.ReadOnlyForceRecursive when the
// container's RUNTIME does not declare the OCI `rro` mount option.
//
// Upstream (daemon/container_operations.go, verbatim in shape):
//
//	if rroErr := supportsRecursivelyReadOnly(daemonCfg, c.HostConfig.Runtime); rroErr != nil {
//	        rro = false
//	        if m.ReadOnlyForceRecursive { return rroErr }
//	}
//
// Without this the fake accepts every create and the gVisor case — every
// read-only share drive on this product's own default confinement floor —
// grades green here while failing on any real host that has runsc.
func (f *fakeDocker) refuseUnsupportedRRO(host *container.HostConfig) error {
	if host == nil {
		return nil
	}
	for _, m := range host.Mounts {
		if m.BindOptions == nil || !m.BindOptions.ReadOnlyForceRecursive {
			continue
		}
		if !runtimeSupportsRecursiveReadOnly(f.info, host.Runtime) {
			rt := host.Runtime
			if rt == "" {
				rt = f.info.DefaultRuntime
			}
			return fmt.Errorf("rro is not supported by runtime %q", rt)
		}
	}
	return nil
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
	delete(f.stdin, name) // a new container under the name has an empty stdin
	if f.failCreateContainer != "" && strings.HasPrefix(name, f.failCreateContainer) {
		return client.ContainerCreateResult{}, fmt.Errorf("boom: create %s", name)
	}
	if err := f.refuseUnsupportedRRO(opts.HostConfig); err != nil {
		return client.ContainerCreateResult{}, err
	}
	if err := f.refusePinWithoutSubnet(opts.NetworkingConfig); err != nil {
		return client.ContainerCreateResult{}, err
	}
	cfg := opts.Config
	if u := f.imageUsers[cfg.Image]; u != "" && cfg.User == "" {
		merged := *cfg
		merged.User = u
		cfg = &merged
	}
	c := &createdContainer{
		name:  name,
		cfg:   cfg,
		host:  opts.HostConfig,
		net:   opts.NetworkingConfig,
		state: &container.State{Status: "created"},
	}
	f.containers[name] = c
	id := name
	if f.createIDOverride != "" {
		id = f.createIDOverride
		f.containers[id] = c // alias: a real daemon accepts either as the "{id}" path param
	}
	// createWarnings simulates a daemon that discarded a requested limit (e.g. a
	// cgroup-v1-rootless host) — surfaced in the create response like real Moby.
	return client.ContainerCreateResult{ID: id, Warnings: f.createWarnings}, nil
}

// fakeCopy records one CopyToContainer: which container, where, the raw
// archive bytes, and whether that container had already been started.
type fakeCopy struct {
	id         string
	dest       string
	archive    []byte
	copyUIDGID bool
	afterStart bool
}

func (f *fakeDocker) CopyToContainer(ctx context.Context, id string, opts client.CopyToContainerOptions) (client.CopyToContainerResult, error) {
	body, err := io.ReadAll(opts.Content)
	if err != nil {
		return client.CopyToContainerResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCopyToContainer {
		return client.CopyToContainerResult{}, fmt.Errorf("boom: copy to %s", id)
	}
	if f.containers[id] == nil {
		return client.CopyToContainerResult{}, fakeNotFound{msg: "no such container: " + id}
	}
	started := slices.Contains(f.startedNames, id)
	f.copies = append(f.copies, fakeCopy{id: id, dest: opts.DestinationPath, archive: body, copyUIDGID: opts.CopyUIDGID, afterStart: started})
	return client.CopyToContainerResult{}, nil
}

func (f *fakeDocker) ContainerStatPath(ctx context.Context, id string, opts client.ContainerStatPathOptions) (client.ContainerStatPathResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.containers[id] == nil {
		return client.ContainerStatPathResult{}, fakeNotFound{msg: "no such container: " + id}
	}
	if !f.existingPaths[opts.Path] {
		return client.ContainerStatPathResult{}, fakeNotFound{msg: "no such path: " + opts.Path}
	}
	return client.ContainerStatPathResult{}, nil
}

func (f *fakeDocker) CopyFromContainer(ctx context.Context, id string, opts client.CopyFromContainerOptions) (client.CopyFromContainerResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.containers[id] == nil {
		return client.CopyFromContainerResult{}, fakeNotFound{msg: "no such container: " + id}
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	switch {
	case opts.SourcePath == "/etc":
		hdr := &tar.Header{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755}
		if f.etcDir != nil {
			hdr = f.etcDir
		}
		_ = tw.WriteHeader(hdr)
	case opts.SourcePath == "/run" && f.runDir != nil:
		_ = tw.WriteHeader(f.runDir)
	case opts.SourcePath == "/etc/passwd" && f.passwd != "":
		_ = tw.WriteHeader(&tar.Header{Name: "passwd", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(f.passwd))})
		_, _ = tw.Write([]byte(f.passwd))
	default:
		return client.CopyFromContainerResult{}, fakeNotFound{msg: "no such path: " + opts.SourcePath}
	}
	_ = tw.Close()
	return client.CopyFromContainerResult{Content: io.NopCloser(&buf)}, nil
}

func (f *fakeDocker) ContainerStart(ctx context.Context, id string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[id]
	if c == nil {
		return client.ContainerStartResult{}, fakeNotFound{msg: "no such container: " + id}
	}
	if strings.HasPrefix(c.name, "wardyn-drive-probe-") {
		// No real command interpreter here to run the probe's `test -r/-x`
		// against a bind mount — model it as already exited with the
		// scripted code, the same "immediate" shape a real one-shot process
		// this fast would leave ContainerWait to observe.
		c.state = &container.State{Status: "exited", ExitCode: int(f.probeExitCode)}
	} else {
		// StartedAt defaults to real, current time — a Driver's proxySettle
		// defaults to 0 for every fake-backed test driver (newWithClient),
		// so the settle check is trivially satisfied regardless of this value
		// UNLESS a test explicitly sets proxySettle back to a real duration,
		// in which case the true current time is exactly what a real daemon
		// would report. startedAtOverride lets a specific test (the L1 clock-
		// skew regression) simulate a daemon whose clock lags the host's.
		startedAt := time.Now()
		if t, ok := f.startedAtOverride[id]; ok {
			startedAt = t
		}
		c.state = &container.State{Status: "running", Running: true, StartedAt: startedAt.Format(time.RFC3339Nano)}
	}
	f.startedNames = append(f.startedNames, id)
	return client.ContainerStartResult{}, nil
}

func (f *fakeDocker) ContainerInspect(ctx context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[id]
	if c == nil || c.removed {
		return client.ContainerInspectResult{}, fakeNotFound{msg: "no such container: " + id}
	}
	if n, ok := f.exitAfterInspects[id]; ok {
		if f.inspectCounts == nil {
			f.inspectCounts = map[string]int{}
		}
		count := f.inspectCounts[id]
		f.inspectCounts[id] = count + 1
		if count >= n {
			c.state = &container.State{Status: "exited", ExitCode: 1}
		}
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
		HostConfig:      c.host,
		NetworkSettings: &container.NetworkSettings{Networks: nets},
	}}, nil
}

// ContainerLogs answers f.logs[id] verbatim (a test scripts it pre-framed with
// muxFrame when the reader under test demuxes it, as startProxy's exit watch
// does). Options are ignored: no fake here models Tail/Follow/Since filtering.
func (f *fakeDocker) ContainerLogs(ctx context.Context, id string, opts client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logOpts = append(f.logOpts, opts)
	if f.containers[id] == nil {
		return nil, fakeNotFound{msg: "no such container: " + id}
	}
	return io.NopCloser(bytes.NewReader(f.logs[id])), nil
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

// ContainerStats serves a canned reading from f.stats (by id); a container the fake does not hold is a
// 404, one with no entry is a daemon error. statsCalls counts the reads.
func (f *fakeDocker) ContainerStats(_ context.Context, id string, opts client.ContainerStatsOptions) (client.ContainerStatsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statsCalls = append(f.statsCalls, opts)
	c := f.containers[id]
	if c == nil || c.removed {
		return client.ContainerStatsResult{}, fakeNotFound{msg: "no such container: " + id}
	}
	st, ok := f.stats[id]
	if !ok {
		return client.ContainerStatsResult{}, errors.New("stats unavailable")
	}
	b, _ := json.Marshal(st)
	return client.ContainerStatsResult{Body: io.NopCloser(bytes.NewReader(b))}, nil
}

// ContainerPause / ContainerUnpause mirror the real daemon's redundant-state
// conflicts (a real "already paused"/"is not paused" 409) so the driver's
// isAlreadyPaused/isNotPaused idempotency handling is actually exercised by a
// repeated Freeze/Thaw, not merely assumed. A paused container reports what a
// real daemon does — Status "paused" with Running=true and Paused=true — and
// statusFromInspect must keep reporting RUNNING while frozen (the design's
// "paused → Running=true → RUNNING" contract).
func (f *fakeDocker) ContainerPause(ctx context.Context, id string, _ client.ContainerPauseOptions) (client.ContainerPauseResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[id]
	if c == nil || c.removed {
		return client.ContainerPauseResult{}, fakeNotFound{msg: "no such container: " + id}
	}
	if c.state != nil && c.state.Paused {
		return client.ContainerPauseResult{}, fmt.Errorf("Error response from daemon: Container %s is already paused", id)
	}
	c.state = &container.State{Status: "paused", Running: true, Paused: true}
	return client.ContainerPauseResult{}, nil
}

func (f *fakeDocker) ContainerUnpause(ctx context.Context, id string, _ client.ContainerUnpauseOptions) (client.ContainerUnpauseResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[id]
	if c == nil || c.removed {
		return client.ContainerUnpauseResult{}, fakeNotFound{msg: "no such container: " + id}
	}
	if c.state == nil || !c.state.Paused {
		return client.ContainerUnpauseResult{}, fmt.Errorf("Error response from daemon: Container %s is not paused", id)
	}
	c.state = &container.State{Status: "running", Running: true, Paused: false}
	return client.ContainerUnpauseResult{}, nil
}

func (f *fakeDocker) ContainerRemove(ctx context.Context, id string, opts client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[id]
	if c == nil {
		return client.ContainerRemoveResult{}, fakeNotFound{msg: "no such container: " + id}
	}
	c.removed = true
	c.forceRemoved = opts.Force
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
	f.lastExecOpts = opts
	return client.ExecCreateResult{ID: execID}, nil
}

func (f *fakeDocker) ExecAttach(ctx context.Context, execID string, opts client.ExecAttachOptions) (client.ExecAttachResult, error) {
	f.mu.Lock()
	conn := f.execAttachConn
	f.mu.Unlock()
	if conn == nil {
		conn = fakeConn{}
	}
	return client.ExecAttachResult{
		HijackedResponse: client.NewHijackedResponse(conn, "application/vnd.docker.raw-stream"),
	}, nil
}

func (f *fakeDocker) ExecStart(ctx context.Context, execID string, opts client.ExecStartOptions) (client.ExecStartResult, error) {
	return client.ExecStartResult{}, nil
}

func (f *fakeDocker) ExecInspectRaw(ctx context.Context, execID string) (execInspect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execInspects++
	if f.execGone != "" && execID == f.execGone {
		return execInspect{}, fakeNotFound{msg: "no such exec: " + execID}
	}
	if f.execInspectErrs > 0 {
		f.execInspectErrs--
		// A daemon blip: NOT a not-found (the exec still exists).
		return execInspect{}, fmt.Errorf("Cannot connect to the Docker daemon: EOF")
	}
	if f.execUnstarted > 0 {
		f.execUnstarted--
		return execInspect{}, nil
	}
	code := f.execExitCode
	if f.execStartRefused {
		return execInspect{ExitCode: &code}, nil
	}
	if !f.execExited {
		return execInspect{Running: true, Pid: 4242}, nil
	}
	if f.execPIDZeroedOnExit {
		return execInspect{ExitCode: &code}, nil
	}
	// Docker keeps a started exec's pid after it exits.
	return execInspect{ExitCode: &code, Pid: 4242}, nil
}

func (f *fakeDocker) ExecResize(ctx context.Context, execID string, opts client.ExecResizeOptions) (client.ExecResizeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := opts
	f.lastResize = &o
	return client.ExecResizeResult{}, nil
}

// VolumeInspect answers the driver's existence probe for a MANAGED user drive's
// named volume: a not-found error (the Docker-shaped one isNotFound classifies)
// for a volume this fake does not hold, and the create options it was made with
// for one it does.
func (f *fakeDocker) VolumeInspect(ctx context.Context, volumeID string, _ client.VolumeInspectOptions) (client.VolumeInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failVolumeInspect {
		return client.VolumeInspectResult{}, fmt.Errorf("daemon: connection reset")
	}
	opts, ok := f.volumes[volumeID]
	if !ok || f.volumeInspectMissesExisting {
		return client.VolumeInspectResult{}, fakeNotFound{msg: "no such volume: " + volumeID}
	}
	return client.VolumeInspectResult{Volume: volume.Volume{
		Name:   opts.Name,
		Driver: opts.Driver,
		Labels: opts.Labels,
		// DriverOpts is what a `docker volume create --opt type=cifs …` volume
		// carries, and it is how a test seeds one the driver must REFUSE to
		// adopt (ensureDriveVolume's inspect-hit arm) — so the fake has to
		// echo it back the way a real daemon does.
		Options: opts.DriverOpts,
	}}, nil
}

// VolumeCreate records the full options the driver asked for — name, driver,
// labels and (the one that must always be empty) DriverOpts.
//
// A create against a name this fake ALREADY HOLDS succeeds and returns the
// EXISTING volume, applying none of the options it was handed — which is what a
// real daemon does, and is the whole reason ensureDriveVolume verifies the
// create's result rather than trusting it. Without this the loser of a first-run
// race would look here exactly like the winner.
func (f *fakeDocker) VolumeCreate(ctx context.Context, opts client.VolumeCreateOptions) (client.VolumeCreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumeCreates++
	if f.failVolumeCreate {
		return client.VolumeCreateResult{}, fmt.Errorf("daemon: create volume: quota exceeded")
	}
	if existing, ok := f.volumes[opts.Name]; ok {
		return client.VolumeCreateResult{Volume: volume.Volume{
			Name: existing.Name, Driver: existing.Driver, Labels: existing.Labels, Options: existing.DriverOpts,
		}}, nil
	}
	f.volumes[opts.Name] = opts
	return client.VolumeCreateResult{Volume: volume.Volume{Name: opts.Name, Driver: opts.Driver, Labels: opts.Labels}}, nil
}

// VolumeRemove is ReclaimDrive's destroy call and nothing else's. It answers
// the two errors the real daemon answers and that the driver branches on: a
// not-found for a name this fake does not hold, and a CONFLICT (the errdefs
// shape a live daemon returns for "volume is in use") when volumeInUse names
// it — the refusal that becomes runner.ErrDriveInUse.
func (f *fakeDocker) VolumeRemove(_ context.Context, volumeID string, opts client.VolumeRemoveOptions) (client.VolumeRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumeRemoves = append(f.volumeRemoves, volumeID)
	f.lastVolumeRemoveForce = opts.Force
	if _, ok := f.volumes[volumeID]; !ok {
		return client.VolumeRemoveResult{}, fakeNotFound{msg: "no such volume: " + volumeID}
	}
	if f.volumeInUse == volumeID {
		return client.VolumeRemoveResult{}, fakeConflict{msg: "remove " + volumeID + ": volume is in use"}
	}
	delete(f.volumes, volumeID)
	return client.VolumeRemoveResult{}, nil
}

// fakeConflict is the errdefs-shaped 409 a real daemon answers when a volume
// is still mounted by a container — the one error ReclaimDrive must NOT read
// as a failure to remove, but as "a run still holds this".
type fakeConflict struct{ msg string }

func (e fakeConflict) Error() string { return e.msg }
func (e fakeConflict) Conflict()     {}

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
