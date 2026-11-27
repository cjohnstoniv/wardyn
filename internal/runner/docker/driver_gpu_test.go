// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/system"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func gpuDriver(f *fakeDocker, kinds ...types.GPUKind) *Driver {
	return newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", GPU: kinds})
}

// withCDINvidia adds the CDI device the NVIDIA Container Toolkit's spec gives the daemon.
func withCDINvidia(f *fakeDocker) {
	f.info.DiscoveredDevices = []system.DeviceInfo{{Source: "cdi", ID: "nvidia.com/gpu=0"}}
}

func TestGPUFromEnv(t *testing.T) {
	env := func(v string, set bool) func(string) string {
		return func(k string) string {
			if k != EnvDockerGPU || !set {
				return ""
			}
			return v
		}
	}
	for _, tc := range []struct {
		name string
		get  func(string) string
		want []types.GPUKind
	}{
		{"unset", env("", false), nil},
		{"nvidia", env("nvidia", true), []types.GPUKind{types.GPUNvidia}},
		{"both", env("render,nvidia", true), []types.GPUKind{types.GPURender, types.GPUNvidia}},
		{"trimmed and lower-cased", env(" NVIDIA ", true), []types.GPUKind{types.GPUNvidia}},
		{"deduplicated", env("nvidia,nvidia", true), []types.GPUKind{types.GPUNvidia}},
	} {
		got, err := GPUFromEnv(tc.get)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s: GPUFromEnv = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	_, err := GPUFromEnv(env("cuda", true))
	if err == nil || !strings.Contains(err.Error(), EnvDockerGPU) {
		t.Errorf("a word that is no GPU kind must refuse and name %s, got %v", EnvDockerGPU, err)
	}
}

func TestGPUOffered_NeedsTheDevice(t *testing.T) {
	f := newFakeDocker()
	d := gpuDriver(f, types.GPUNvidia)
	if got := d.gpuOffered(f.info); len(got) != 0 {
		t.Errorf("nvidia without a CDI device offered %v, want none", got)
	}
	f.info.DiscoveredDevices = []system.DeviceInfo{{Source: "cdi", ID: "vendor.com/other=0"}, {Source: "other", ID: "nvidia.com/gpu=0"}}
	if got := d.gpuOffered(f.info); len(got) != 0 {
		t.Errorf("a CDI device of another vendor, or a non-CDI source, offered %v, want none", got)
	}
	withCDINvidia(f)
	if got := d.gpuOffered(f.info); !slices.Equal(got, []types.GPUKind{types.GPUNvidia}) {
		t.Errorf("nvidia with its CDI device offered %v, want [nvidia]", got)
	}
	// The kind must also be turned on: a host with the device and no env offers nothing.
	if got := gpuDriver(f).gpuOffered(f.info); len(got) != 0 {
		t.Errorf("an unconfigured driver offered %v, want none", got)
	}
}

func TestGPUByClass_CC1RuncOnly(t *testing.T) {
	f := newFakeDocker()
	withCDINvidia(f)
	d := gpuDriver(f, types.GPUNvidia)
	got := d.gpuByClass(f.info)
	if !slices.Equal(got[types.CC1], []types.GPUKind{types.GPUNvidia}) || len(got) != 1 {
		t.Fatalf("runc host gpuByClass = %v, want {CC1: [nvidia]}", got)
	}

	// CC1 overridden to another runtime: no GPU.
	f2 := newFakeDocker()
	f2.info = infoWithRuntimes("sysbox-runc")
	withCDINvidia(f2)
	d2 := newWithClient(f2, Config{ProxyImage: "wardyn-proxy:dev", GPU: []types.GPUKind{types.GPUNvidia}, ConfinementRuntimes: map[types.ConfinementClass]string{types.CC1: "sysbox-runc"}})
	if got := d2.gpuByClass(f2.info); got != nil {
		t.Errorf("CC1 on sysbox offered %v, want none", got)
	}

	// A daemon whose default runtime is nvidia offers none, and the refusal names runc and the nvidia default.
	f.info.DefaultRuntime = "nvidia"
	if got := d.gpuByClass(f.info); got != nil {
		t.Errorf("a daemon defaulting to the nvidia runtime offered %v, want none", got)
	}
	err := d.gpuAdmits(types.GPUNvidia, "", f.info)
	if err == nil || !strings.Contains(err.Error(), "runc") || !strings.Contains(err.Error(), "nvidia") {
		t.Errorf("gpuAdmits on an nvidia-default daemon = %v, want an error naming runc and the nvidia default runtime", err)
	}
	if err := d.gpuAdmits(types.GPUNone, "", f.info); err != nil {
		t.Errorf("no GPU asked must always be admitted, got %v", err)
	}
}

func TestCreateSandbox_GPUNvidiaHandsInCDI(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	withCDINvidia(f)
	d := gpuDriver(f, types.GPUNvidia)
	spec := testSpec()
	spec.Resources.GPU = types.GPUNvidia
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	host := f.containers[sb.Ref].host
	want := []container.DeviceRequest{{Driver: "cdi", DeviceIDs: []string{"nvidia.com/gpu=all"}}}
	if len(host.DeviceRequests) != 1 || host.DeviceRequests[0].Driver != want[0].Driver || !slices.Equal(host.DeviceRequests[0].DeviceIDs, want[0].DeviceIDs) {
		t.Errorf("DeviceRequests = %+v, want exactly %+v", host.DeviceRequests, want)
	}
	if !slices.Equal(host.CapDrop, []string{"ALL"}) || !slices.Contains(host.SecurityOpt, "no-new-privileges") {
		t.Errorf("hardening lost: CapDrop %v SecurityOpt %v", host.CapDrop, host.SecurityOpt)
	}
}

func TestCreateSandbox_GPURenderHandsInRenderNodesOnly(t *testing.T) {
	nodes, gids := renderNodesFn, deviceGIDFn
	t.Cleanup(func() { renderNodesFn, deviceGIDFn = nodes, gids })
	renderNodesFn = func() []string {
		return []string{"/dev/dri/renderD128", "/dev/dri/renderD129", "/dev/dri/renderD130"}
	}
	gidOf := map[string]string{"/dev/dri/renderD128": "44", "/dev/dri/renderD129": "109", "/dev/dri/renderD130": "44"}
	deviceGIDFn = func(p string) string { return gidOf[p] }

	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := gpuDriver(f, types.GPURender)
	spec := testSpec()
	spec.Resources.GPU = types.GPURender
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	host := f.containers[sb.Ref].host
	if len(host.Devices) != 3 {
		t.Fatalf("Devices = %+v, want the three render nodes", host.Devices)
	}
	for _, dev := range host.Devices {
		if !strings.HasPrefix(dev.PathOnHost, "/dev/dri/renderD") || dev.PathInContainer != dev.PathOnHost || dev.CgroupPermissions != "rw" {
			t.Errorf("device %+v is not a rw render node mapped to itself", dev)
		}
	}
	if !slices.Equal(host.GroupAdd, []string{"44", "109"}) {
		t.Errorf("GroupAdd = %v, want [44 109] (each distinct gid once, in node order)", host.GroupAdd)
	}
	if len(host.DeviceRequests) != 0 {
		t.Errorf("a render run carried device requests %+v", host.DeviceRequests)
	}
}

func TestCreateSandbox_GPUNotOfferedFailsBeforeCreate(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	withCDINvidia(f)
	d := gpuDriver(f) // the operator turned no GPU on
	spec := testSpec()
	spec.Resources.GPU = types.GPUNvidia
	if _, err := d.CreateSandbox(context.Background(), spec); err == nil {
		t.Fatal("a GPU run on a host with the GPU off must be refused")
	}
	if len(f.containers) != 0 || len(f.networks) != 0 {
		t.Errorf("nothing may be created before the refusal: containers=%d networks=%d", len(f.containers), len(f.networks))
	}
}

func TestCreateSandbox_NoGPUIsUnchanged(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	withCDINvidia(f)
	d := gpuDriver(f, types.GPUNvidia, types.GPURender)
	sb, err := d.CreateSandbox(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	host := f.containers[sb.Ref].host
	if len(host.DeviceRequests) != 0 || len(host.Devices) != 0 || len(host.GroupAdd) != 0 {
		t.Errorf("a run with no GPU got device requests %+v, devices %+v, groups %v", host.DeviceRequests, host.Devices, host.GroupAdd)
	}
}
