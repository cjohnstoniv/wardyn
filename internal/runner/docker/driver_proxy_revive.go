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

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// proxyConfigEnv is the proxy sidecar's config variable: the one place its
// rendered config, and the per-run MITM CA key inside it, rests.
const proxyConfigEnv = "WARDYN_PROXY_CONFIG_JSON"

var _ runner.ProxyReviver = (*Driver)(nil)

// startProxy creates the wardyn-proxy sidecar for runID on the per-run
// network, joins it to the control-plane-facing network and starts it. ip,
// when valid, is the address it must take on the per-run network (the one the
// agent's hosts entry pins). On any failure it removes what it created.
func (d *Driver) startProxy(ctx context.Context, runID uuid.UUID, labels map[string]string, env []string, ip netip.Addr) (string, error) {
	cfg := &container.Config{
		Image:    d.cfg.ProxyImage,
		Hostname: "wardyn-proxy",
		Labels:   labels,
		Env:      env,
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
	if _, err := d.cli.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{}); err != nil {
		remove()
		return "", fmt.Errorf("docker: start proxy: %w", err)
	}
	return resp.ID, nil
}

// ProxyConfig reads the agent ref's proxy sidecar's rendered config back from
// its env (runner.ProxyReviver). The sidecar may be running or stopped (a
// lost run's, see StopProxy); a missing one is an error, because nothing
// else holds the per-run MITM CA the sandbox trusts.
func (d *Driver) ProxyConfig(ctx context.Context, ref string) ([]byte, error) {
	id, err := d.proxyRunID(ctx, ref)
	if err != nil {
		return nil, err
	}
	res, err := d.cli.ContainerInspect(ctx, proxyContainerName(id), client.ContainerInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("docker: inspect proxy: %w", err)
	}
	if res.Container.Config != nil {
		for _, kv := range res.Container.Config.Env {
			if v, ok := strings.CutPrefix(kv, proxyConfigEnv+"="); ok {
				return []byte(v), nil
			}
		}
	}
	return nil, fmt.Errorf("docker: proxy of agent %s carries no %s", ref, proxyConfigEnv)
}

// ReplaceProxy swaps the agent ref's proxy sidecar for a new one running
// cfgJSON on the current proxy image (runner.ProxyReviver). The old sidecar
// is removed first: it holds the name, and its address is the one the agent
// pins. The new one takes that address, read from the agent's own hosts entry
// (immutable, and still there after a stopped proxy gave its address back),
// and re-joins the control-plane-facing network as at create. Every check
// that can fail without touching the old sidecar runs before the remove.
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
	old, err := d.cli.ContainerInspect(ctx, proxyContainerName(id), client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("docker: inspect proxy: %w", err)
	}
	var labels map[string]string
	if old.Container.Config != nil {
		labels = old.Container.Config.Labels
	}
	if err := d.ensureImage(ctx, d.cfg.ProxyImage, func() {}); err != nil {
		return err
	}
	// A failed remove may still have removed it.
	if _, err := d.cli.ContainerRemove(ctx, proxyContainerName(id), client.ContainerRemoveOptions{Force: true}); err != nil && !isNotFound(err) {
		return fmt.Errorf("%w: docker: remove proxy: %w", runner.ErrProxyReplaceFailed, err)
	}
	if _, err := d.startProxy(ctx, id, labels, proxyEnvFromJSON(id, cfgJSON, cp.ControlPlaneURL), ip); err != nil {
		return fmt.Errorf("%w: %w", runner.ErrProxyReplaceFailed, err)
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
