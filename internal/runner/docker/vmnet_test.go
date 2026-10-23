// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/vm/qemu"
)

// proxyConfigOf is the config the run's proxy was started with (written to its stdin).
func proxyConfigOf(t *testing.T, f *fakeDocker, name string) map[string]json.RawMessage {
	t.Helper()
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(f.stdinOf(name), &cfg); err != nil {
		t.Fatalf("proxy %s config: %v", name, err)
	}
	return cfg
}

// A desktop VM's create starts its network relay from the proxy image, hardened as the other VM
// containers are, on the run's internal network only, with net/ as its one mount, aimed at the run's
// proxy; and that proxy's config is marked vm.
func TestVMCreateStartsTheNetworkRelay(t *testing.T) {
	r := newVMRig(t)
	spec := vmSpec()
	spec.Labels = map[string]string{"wardyn.test": "x"}
	ctx := context.Background()
	if _, err := r.d.CreateSandbox(ctx, spec); err != nil {
		t.Fatal(err)
	}
	c := r.container(t, vmnetName(spec.RunID))
	if c.cfg.Image != r.d.proxyImage() || !slices.Equal(c.cfg.Entrypoint, []string{vmnetEntrypoint}) ||
		!slices.Equal(c.cfg.Cmd, []string{"-listen", "/vm/net/net.sock", "-proxy", "wardyn-proxy:3128"}) {
		t.Fatalf("relay image %q entrypoint %v cmd %v: wardyn-vmnet from the proxy image, on net.sock, to the proxy",
			c.cfg.Image, c.cfg.Entrypoint, c.cfg.Cmd)
	}
	if u := c.cfg.User; u != r.vc.user() || u == "" || u == "0" || strings.HasPrefix(u, "0:") {
		t.Fatalf("relay runs as %q, want the VM containers' non-root %q", u, r.vc.user())
	}
	h := c.host
	if h.Privileged || !slices.Equal(h.CapDrop, []string{"ALL"}) || !slices.Contains(h.SecurityOpt, "no-new-privileges") ||
		!h.ReadonlyRootfs || len(h.CapAdd) != 0 || len(h.Devices) != 0 || len(h.GroupAdd) != 0 || h.PidsLimit == nil || h.Memory == 0 {
		t.Fatalf("relay host config not hardened: %+v", h)
	}
	if string(h.NetworkMode) != internalNetName(spec.RunID) || len(c.connectedTo) != 0 || len(h.PortBindings) != 0 || h.PublishAllPorts ||
		h.NetworkMode.IsHost() {
		t.Fatalf("relay network %q joins %v ports %v: the run's internal network only, nothing published",
			h.NetworkMode, c.connectedTo, h.PortBindings)
	}
	dir := r.vc.runDir(spec.RunID)
	if len(h.Mounts) != 1 || h.Mounts[0] != (mount.Mount{Type: mount.TypeBind, Source: filepath.Join(dir, "net"), Target: qemu.NetDir}) ||
		len(h.Binds) != 0 {
		t.Fatalf("relay mounts %+v binds %v: net/ only", h.Mounts, h.Binds)
	}
	if mountSource(r.container(t, agentContainerName(spec.RunID)), qemu.NetDir) != filepath.Join(dir, "net") {
		t.Fatal("QEMU and the relay do not share net/")
	}
	if fi, err := os.Stat(filepath.Join(dir, "net")); err != nil || fi.Mode().Perm()&0o007 != 0 || fi.Mode().Perm() != 0o700 {
		t.Fatalf("net/ is %v (%v), want 0700 and no world bits", fi.Mode(), err)
	}
	pi, err := r.f.ContainerInspect(ctx, proxyContainerName(spec.RunID), client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ip := pi.Container.NetworkSettings.Networks[internalNetName(spec.RunID)].IPAddress.String()
	if !slices.Equal(h.ExtraHosts, []string{"wardyn-proxy:" + ip}) {
		t.Fatalf("relay hosts %v, want wardyn-proxy at the proxy's run-network address %s", h.ExtraHosts, ip)
	}
	if c.cfg.Labels[labelComponent] != componentVMNet || c.cfg.Labels[labelRun] != spec.RunID.String() || c.cfg.Labels["wardyn.test"] != "x" {
		t.Fatalf("relay labels %v: its own component, the run's labels", c.cfg.Labels)
	}
	if vm := proxyConfigOf(t, r.f, proxyContainerName(spec.RunID))["vm"]; string(vm) != "true" {
		t.Fatalf("the VM's proxy config vm = %s, want true", vm)
	}
}

// A container run starts no relay, and its proxy config has no vm key, even when the control
// plane sent one: only the driver that starts a relay puts a proxy into VM mode.
func TestContainerCreateStartsNoRelayAndNoVMKey(t *testing.T) {
	r := newVMRig(t)
	spec := testSpec()
	spec.ProxyConfig.VM = true
	if _, err := r.d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	r.f.mu.Lock()
	_, relay := r.f.containers[vmnetName(spec.RunID)]
	r.f.mu.Unlock()
	if relay {
		t.Fatal("a container run got a network relay")
	}
	if _, ok := proxyConfigOf(t, r.f, proxyContainerName(spec.RunID))["vm"]; ok {
		t.Fatal("a container run's proxy config carries the vm key")
	}
}

// Stop and kill remove the relay before the run's network, which Docker refuses to remove while a
// container is still on it: a relay that will not go fails the teardown and keeps the network, and a
// retry finishes both.
func TestVMTeardownAndKillRemoveTheRelayBeforeTheNetwork(t *testing.T) {
	for verb, end := range map[string]func(*Driver, context.Context, string) error{
		"stop": (*Driver).StopSandbox,
		"kill": (*Driver).KillSandbox,
	} {
		r := newVMRig(t)
		spec := vmSpec()
		ctx := context.Background()
		sb, err := r.d.CreateSandbox(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		relay := vmnetName(spec.RunID)
		r.f.mu.Lock()
		r.f.failRemove = map[string]int{relay: 2} // the teardown's removal and the VM cleanup's
		r.f.mu.Unlock()
		err = end(r.d, ctx, sb.Ref)
		r.f.mu.Lock()
		_, netLeft := r.f.networks[internalNetName(spec.RunID)]
		r.f.mu.Unlock()
		if err == nil || !netLeft {
			t.Fatalf("%s with a relay that will not go = %v, network kept %v: want an error and the network kept", verb, err, netLeft)
		}
		if err := end(r.d, ctx, sb.Ref); err != nil {
			t.Fatalf("%s retry: %v", verb, err)
		}
		r.f.mu.Lock()
		_, netLeft = r.f.networks[internalNetName(spec.RunID)]
		r.f.mu.Unlock()
		if c := r.container(t, relay); !c.removed || netLeft {
			t.Fatalf("%s retry: relay removed %v, network left %v", verb, c.removed, netLeft)
		}
	}
}

// A VM's proxy is never replaced in this release (errVMUnsupported): the refusal keeps the running
// proxy, still marked vm.
func TestVMReplaceProxyRefusalKeepsTheVMProxy(t *testing.T) {
	r := newVMRig(t)
	spec := vmSpec()
	ctx := context.Background()
	sb, err := r.d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.d.ReplaceProxy(ctx, sb.Ref, []byte(`{"control_plane_url":"http://127.0.0.1:8080"}`)); err == nil {
		t.Fatal("ReplaceProxy of a VM was accepted")
	}
	if c := r.container(t, proxyContainerName(spec.RunID)); c.removed {
		t.Fatal("a refused ReplaceProxy removed the VM's proxy")
	}
	if vm := proxyConfigOf(t, r.f, proxyContainerName(spec.RunID))["vm"]; string(vm) != "true" {
		t.Fatalf("the VM's proxy config vm = %s after a refused ReplaceProxy, want true", vm)
	}
}

// VM containers, the relay among them, never run as root: a root runner's are uid and gid 65532.
func TestVMUserIsNeverRoot(t *testing.T) {
	if u, g := vmUser(0, 0); u != 65532 || g != 65532 {
		t.Fatalf("a root runner's VM containers run as %d:%d, want 65532:65532", u, g)
	}
	if u, g := vmUser(1000, 1001); u != 1000 || g != 1001 {
		t.Fatalf("a runner as 1000:1001 gets VM containers as %d:%d, want its own", u, g)
	}
}

// The proxy image carries the relay at the path the driver starts it from, and its own entrypoint
// is still wardyn-proxy: only startVMNet picks the relay.
func TestProxyImageCarriesTheRelay(t *testing.T) {
	b, err := os.ReadFile("../../../deploy/compose/Dockerfile.proxy")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if !strings.Contains(src, "COPY --from=builder /out/wardyn-vmnet "+vmnetEntrypoint+"\n") ||
		!strings.Contains(src, "go build -o /out/wardyn-vmnet ./cmd/wardyn-vmnet") {
		t.Fatalf("Dockerfile.proxy does not build wardyn-vmnet into %s", vmnetEntrypoint)
	}
	eps := regexp.MustCompile(`(?m)^ENTRYPOINT .*$`).FindAllString(src, -1)
	if len(eps) != 1 || eps[0] != `ENTRYPOINT ["/usr/local/bin/wardyn-proxy"]` {
		t.Fatalf("Dockerfile.proxy entrypoints %q, want the proxy's alone", eps)
	}
}
