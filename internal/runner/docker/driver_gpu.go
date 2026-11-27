// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/system"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// EnvDockerGPU names the GPU kinds the operator turns on for runs on this host (#2958): a comma list of
// nvidia and render. Unset is off.
const EnvDockerGPU = "WARDYN_DOCKER_GPU"

// gpuNvidiaAll is the CDI device name the NVIDIA Container Toolkit's generated specs give every GPU.
const gpuNvidiaAll = "nvidia.com/gpu=all"

// renderNodesFn and deviceGIDFn are the host reads applyGPU and gpuOffered make, replaced by tests so no
// host /dev/dri is read. Like kvmDeviceGID, they assume wardynd shares the host with the Docker daemon.
var (
	renderNodesFn = renderNodes
	deviceGIDFn   = deviceGID
)

// GPUFromEnv parses WARDYN_DOCKER_GPU: empty or unset is nil (off); otherwise nvidia, render or both,
// comma-separated, trimmed, lower-cased and without duplicates. Any other word is an error, so a typo
// refuses boot instead of silently turning the GPU off.
func GPUFromEnv(getenv func(string) string) ([]types.GPUKind, error) {
	raw := strings.TrimSpace(getenv(EnvDockerGPU))
	if raw == "" {
		return nil, nil
	}
	var kinds []types.GPUKind
	for _, word := range strings.Split(raw, ",") {
		kind := types.GPUKind(strings.ToLower(strings.TrimSpace(word)))
		if kind == types.GPUNone || !kind.Valid() {
			return nil, fmt.Errorf("%s: %q is not a GPU kind: use nvidia, render or both, comma-separated", EnvDockerGPU, word)
		}
		if !slices.Contains(kinds, kind) {
			kinds = append(kinds, kind)
		}
	}
	return kinds, nil
}

// renderNodes lists the host's render nodes, sorted; never a /dev/dri/card* node.
func renderNodes() []string {
	nodes, _ := filepath.Glob("/dev/dri/renderD*")
	slices.Sort(nodes)
	return nodes
}

// deviceGID returns the numeric gid owning the device node at path, or "" when it cannot be read.
func deviceGID(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return strconv.FormatUint(uint64(st.Gid), 10)
	}
	return ""
}

// gpuOffered returns the configured GPU kinds this host really has: nvidia only when the daemon reports a CDI
// device named nvidia.com/gpu=..., render only when a render node exists.
func (d *Driver) gpuOffered(info system.Info) []types.GPUKind {
	var offered []types.GPUKind
	for _, kind := range d.cfg.GPU {
		switch kind {
		case types.GPUNvidia:
			if slices.ContainsFunc(info.DiscoveredDevices, func(dev system.DeviceInfo) bool {
				return dev.Source == "cdi" && strings.HasPrefix(dev.ID, "nvidia.com/gpu=")
			}) {
				offered = append(offered, kind)
			}
		case types.GPURender:
			if len(renderNodesFn()) > 0 {
				offered = append(offered, kind)
			}
		}
	}
	return offered
}

// gpuByClass is the per-class GPU capability: only CC1 on the runc runtime, and only the kinds the host has. A
// daemon whose default runtime is nvidia (the toolkit's legacy setup) offers none, since GPU runs use CDI with runc.
func (d *Driver) gpuByClass(info system.Info) map[types.ConfinementClass][]types.GPUKind {
	rt, _, err := resolveRuntime(types.CC1, info, d.cfg.ConfinementRuntimes)
	if err != nil || cmp.Or(rt, info.DefaultRuntime) != "runc" {
		return nil
	}
	offered := d.gpuOffered(info)
	if len(offered) == 0 {
		return nil
	}
	return map[types.ConfinementClass][]types.GPUKind{types.CC1: offered}
}

// resolveAgentRuntime is the runtime the run's class resolves to, refused (before anything is created) when the
// run asks for a GPU the host does not offer on it.
func (d *Driver) resolveAgentRuntime(spec runner.SandboxSpec, info system.Info) (string, error) {
	runtimeName, _, err := resolveRuntime(spec.ConfinementClass, info, d.cfg.ConfinementRuntimes)
	if err != nil {
		return "", err
	}
	return runtimeName, d.gpuAdmits(spec.Resources.GPU, runtimeName, info)
}

// gpuAdmits refuses, before anything is created, a GPU the host does not offer to a run on runtimeName.
func (d *Driver) gpuAdmits(kind types.GPUKind, runtimeName string, info system.Info) error {
	if kind == types.GPUNone {
		return nil
	}
	if slices.Contains(d.gpuByClass(info)[types.CC1], kind) && cmp.Or(runtimeName, info.DefaultRuntime) == "runc" {
		return nil
	}
	return fmt.Errorf("docker: this host gives no %s GPU to a run on runtime %s: GPU runs need runc (a daemon whose default runtime is nvidia, the toolkit's legacy setup, offers none; use CDI with runc); set %s and install the device, or start the run without a GPU", kind, cmp.Or(runtimeName, info.DefaultRuntime), EnvDockerGPU)
}

// applyGPU hands the GPU into hc. Nothing else about the host config changes: capabilities stay dropped,
// no-new-privileges and the default seccomp profile stay on.
func applyGPU(hc *container.HostConfig, kind types.GPUKind) {
	switch kind {
	case types.GPUNvidia:
		hc.Resources.DeviceRequests = append(hc.Resources.DeviceRequests, container.DeviceRequest{Driver: "cdi", DeviceIDs: []string{gpuNvidiaAll}})
	case types.GPURender:
		for _, n := range renderNodesFn() {
			hc.Devices = append(hc.Devices, container.DeviceMapping{PathOnHost: n, PathInContainer: n, CgroupPermissions: "rw"})
			// The agent is non-root with no capabilities, so the node's group joins as a supplementary
			// group; nodes can carry different groups, each is added once.
			if gid := deviceGIDFn(n); gid != "" && !slices.Contains(hc.GroupAdd, gid) {
				hc.GroupAdd = append(hc.GroupAdd, gid)
			}
		}
	}
}
