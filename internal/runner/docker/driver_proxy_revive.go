// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// proxyConfigStdinEnv tells the proxy sidecar its config arrives on stdin
// (#1176): the rendered config (run token, per-run MITM CA key, upstream-proxy
// credential) is written there once at start and lives only in the proxy's
// memory, so neither the container's config nor its environment holds it.
const proxyConfigStdinEnv = "WARDYN_PROXY_CONFIG_STDIN"

// proxyConfigWriteTimeout bounds the stdin write of the config to a proxy.
const proxyConfigWriteTimeout = 30 * time.Second

var _ runner.ProxyReviver = (*Driver)(nil)
var _ runner.SandboxStarter = (*Driver)(nil)

// startProxy creates the wardyn-proxy sidecar for runID on the per-run
// network, joins it to the control-plane-facing network, starts it and hands
// it cfgJSON on stdin. ip, when valid, is the address it must take on the
// per-run network (the one the agent's hosts entry pins). On any failure it
// removes what it created.
func (d *Driver) startProxy(ctx context.Context, runID uuid.UUID, labels map[string]string, env []string, cfgJSON []byte, ip netip.Addr) (string, error) {
	cfg := &container.Config{
		Image:    d.cfg.ProxyImage,
		Hostname: "wardyn-proxy",
		Labels:   labels,
		Env:      env,
		// stdin is the config's only way in: open, and closed for good once
		// the one write below detaches, so a later start of this container
		// reads nothing and the proxy exits non-zero.
		OpenStdin: true,
		StdinOnce: true,
	}
	if d.cfg.ProxyBinaryHostPath != "" {
		cfg.Entrypoint = []string{"/usr/local/bin/wardyn-proxy"}
	}
	if len(d.cfg.ProxyCmd) > 0 {
		cfg.Cmd = d.cfg.ProxyCmd
	}
	host := &container.HostConfig{
		// Proxy attaches to the internal net at create; it is NOT NetworkMode
		// none — it must bridge out via the wardyn-internal network joined
		// below. Hardened the same way as the agent.
		NetworkMode:    container.NetworkMode(internalNetName(runID)),
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		ReadonlyRootfs: false,
		Tmpfs:          map[string]string{"/tmp": "rw,nosuid,nodev,noexec,size=64m"},
		AutoRemove:     false,
		// Never restarted by the daemon: a restart would re-run it with no
		// config (stdin is gone), and only the control plane starts a proxy.
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
		// Map host.docker.internal to the docker host gateway so the brokered
		// control-plane forward (resolveTrustedURL) can reach a wardynd running on
		// the host in host mode. Docker Desktop injects this alias automatically;
		// native docker needs the explicit host-gateway mapping. Scoped to the
		// proxy — only it forwards to the control plane, and the alias is consulted
		// ONLY by the trusted forward path, never by the agent's policy-governed
		// egress (which still denies host IPs via the private-IP guard). General
		// egress is NOT broadened.
		ExtraHosts: []string{"host.docker.internal:host-gateway"},
		// The proxy only relays HTTP, so a tight resource envelope still leaves
		// ample headroom while bounding a compromised proxy: its own PID cap
		// (fork-bomb guard) and a modest memory cap (MemorySwap pinned so the
		// cap is not silently doubled via swap).
		Resources: proxyResources(),
	}
	if d.cfg.ProxyBinaryHostPath != "" {
		host.Binds = []string{d.cfg.ProxyBinaryHostPath + ":/usr/local/bin/wardyn-proxy:ro"}
	}
	var netCfg *network.NetworkingConfig
	if ip.IsValid() {
		netCfg = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			internalNetName(runID): {IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: ip}},
		}}
	}
	resp, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:           cfg,
		HostConfig:       host,
		NetworkingConfig: netCfg,
		Name:             proxyContainerName(runID),
	})
	if err != nil {
		return "", fmt.Errorf("docker: create proxy: %w", err)
	}
	remove := func() {
		_, _ = d.cli.ContainerRemove(context.Background(), resp.ID, client.ContainerRemoveOptions{Force: true})
	}
	// Connect the proxy to the control-plane-facing network so it can reach
	// the control plane. This network is the ONLY route off the per-run
	// segment, and only the proxy is on it.
	if _, err := d.cli.NetworkConnect(ctx, d.cfg.InternalNetwork, client.NetworkConnectOptions{
		Container:      resp.ID,
		EndpointConfig: &network.EndpointSettings{},
	}); err != nil {
		remove()
		return "", fmt.Errorf("docker: connect proxy to %s: %w", d.cfg.InternalNetwork, err)
	}
	if err := d.startWithConfig(ctx, resp.ID, cfgJSON); err != nil {
		remove()
		return "", err
	}
	return resp.ID, nil
}

// startWithConfig attaches to the created proxy's stdin, starts it, writes
// cfgJSON there and closes it: the proxy reads its config to EOF. Attaching
// before the start is what `docker run -i` does, so no byte is written before
// the proxy's stdin exists.
func (d *Driver) startWithConfig(ctx context.Context, id string, cfgJSON []byte) error {
	att, err := d.cli.ContainerAttach(ctx, id, client.ContainerAttachOptions{Stream: true, Stdin: true})
	if err != nil {
		return fmt.Errorf("docker: attach proxy stdin: %w", err)
	}
	defer att.Close()
	if _, err := d.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("docker: start proxy: %w", err)
	}
	_ = att.Conn.SetWriteDeadline(time.Now().Add(proxyConfigWriteTimeout))
	if _, err := att.Conn.Write(cfgJSON); err != nil {
		return fmt.Errorf("docker: write proxy config: %w", err)
	}
	if err := att.CloseWrite(); err != nil {
		return fmt.Errorf("docker: close proxy stdin: %w", err)
	}
	return nil
}

// EnsureProxyImage — see runner.ProxyReviver. ReplaceProxy also ensures the
// image itself (a cheap local check once it is cached); calling this first
// keeps a slow FIRST pull out of the window between the revive claim and the
// new proxy actually starting (F2).
func (d *Driver) EnsureProxyImage(ctx context.Context) error {
	return d.ensureImage(ctx, d.cfg.ProxyImage, func() {})
}

// CanReplaceProxy — see runner.ProxyReviver: Docker always can.
func (d *Driver) CanReplaceProxy(context.Context, string) error { return nil }

// ReplaceProxy swaps the agent ref's proxy sidecar, if it still has one, for
// a new one running cfgJSON on the current proxy image (runner.ProxyReviver).
// The old sidecar is removed first: it holds the name, and its address is the
// one the agent pins. The new one takes that address, read from the agent's
// own hosts entry (immutable, and still there after the old proxy gave its
// address back), carries the agent's labels as a proxy, and re-joins the
// control-plane-facing network as at create. Every check that can fail
// without touching the old sidecar runs before the remove. Nothing is put
// back when the new one does not start: the config is the control plane's,
// not the container's (#1176), so a later revive rebuilds from it.
func (d *Driver) ReplaceProxy(ctx context.Context, ref string, cfgJSON []byte) error {
	id, err := d.proxyRunID(ctx, ref)
	if err != nil {
		return err
	}
	var cp struct {
		ControlPlaneURL string `json:"control_plane_url"`
	}
	if err := json.Unmarshal(cfgJSON, &cp); err != nil {
		return fmt.Errorf("docker: replace proxy: config: %w", err)
	}
	agent, err := d.cli.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("docker: inspect agent for its proxy address: %w", err)
	}
	ip := pinnedProxyIP(agent.Container.HostConfig)
	if !ip.IsValid() {
		return fmt.Errorf("docker: agent %s pins no proxy address", ref)
	}
	var agentLabels map[string]string
	if agent.Container.Config != nil {
		agentLabels = agent.Container.Config.Labels
	}
	if err := d.ensureImage(ctx, d.cfg.ProxyImage, func() {}); err != nil {
		return err
	}
	// A failed remove may still have removed it.
	if _, err := d.cli.ContainerRemove(ctx, proxyContainerName(id), client.ContainerRemoveOptions{Force: true}); err != nil && !isNotFound(err) {
		return fmt.Errorf("%w: docker: remove proxy: %w", runner.ErrProxyReplaceFailed, err)
	}
	if _, err := d.startProxy(ctx, id, wardynLabels(id, componentProxy, agentLabels),
		proxyEnvFromJSON(id, cp.ControlPlaneURL), cfgJSON, ip); err != nil {
		return fmt.Errorf("%w: %w", runner.ErrProxyReplaceFailed, err)
	}
	return nil
}

// StartSandbox starts the agent ref's kept, stopped container again
// (runner.SandboxStarter): `docker start`, which re-runs its main process
// (`agent-run --idle`) over the writable layer the run left behind. It
// refuses unless the run's proxy sidecar is running: a stopped or removed
// proxy has given its address back, and an agent started first could take the address its own
// hosts entry pins as wardyn-proxy. The recording dirs are prepared again as
// at create.
func (d *Driver) StartSandbox(ctx context.Context, ref string) error {
	id, err := d.proxyRunID(ctx, ref)
	if err != nil {
		return err
	}
	p, err := d.cli.ContainerInspect(ctx, proxyContainerName(id), client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("docker: inspect proxy before starting the agent: %w", err)
	}
	if p.Container.State == nil || !p.Container.State.Running {
		return fmt.Errorf("docker: the proxy of agent %s is not running; refusing to start the agent", ref)
	}
	if _, err := d.cli.ContainerStart(ctx, ref, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("docker: start agent: %w", err)
	}
	if d.cfg.Record {
		d.prepareRecordingDirs(ctx, ref)
	}
	return nil
}

// pinnedProxyIP is the wardyn-proxy address in the agent's hosts entries
// (CreateSandbox's "wardyn-proxy:<ip>").
func pinnedProxyIP(host *container.HostConfig) netip.Addr {
	if host == nil {
		return netip.Addr{}
	}
	for _, h := range host.ExtraHosts {
		if v, ok := strings.CutPrefix(h, "wardyn-proxy:"); ok {
			if ip, err := netip.ParseAddr(v); err == nil {
				return ip
			}
		}
	}
	return netip.Addr{}
}
