// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// An MCP server that starts from a command runs in a sandbox of its own (runner.MCPSandboxer): its container on
// a network of its own, and an MCP proxy holding the server's sealed egress policy on that network, on the
// control-plane-facing network and on the run's relay link. The link is the one network the MCP proxies share
// with the agent's proxy, which reaches each server only through its MCP proxy; the agent itself is on no MCP
// network and not on the link. Nothing of the host is in either container: no mount but a tmpfs at each one's
// /tmp, no image volume, no device but krun's own, no docker socket and no published port. Each can still write
// its own container layer. The link lives as long as the run: only teardown and the orphan sweep remove it, so a
// server's re-create keeps its relay address.

var (
	_ runner.MCPSandboxer    = (*Driver)(nil)
	_ runner.MCPProxyStopper = (*Driver)(nil)
)

const (
	// mcpHostImagePath is where the proxy image carries the program the MCP container runs. It is copied out of
	// the created MCP proxy into the created container at runner.MCPHostPath, never mounted; mcpHostMaxBytes
	// bounds the copy.
	mcpHostImagePath = "/usr/local/bin/wardyn-mcp-host"
	mcpHostMaxBytes  = 64 << 20
	// mcpHostPort is the port that program serves the server on in its container.
	mcpHostPort = "8765"
	// mcpScratchBytes sizes the MCP container's tmpfs at /tmp, its writable place beside its own layer.
	mcpScratchBytes = 256 << 20
	// labelMCPRelayAddr carries an MCP proxy's relay address, ip:port, for MCPSandboxStatus.
	labelMCPRelayAddr = "wardyn.mcp-relay-addr"
	// relayAgentProxyHost is the agent's proxy's place on the relay link, from the bottom: the first after the
	// network's own address and its gateway. The MCP proxies are pinned from the top (mcpRelayAddr).
	relayAgentProxyHost = 2
	// mcpFinalKillBound bounds the one kill a stop makes after its context has ended.
	mcpFinalKillBound = 5 * time.Second
)

var errMCPNoShell = errors.New("docker: an MCP server's sandbox takes no shell, exec or attach")

// mcpStart is what StartMCPSandbox needs of a created MCP proxy: its config, which reaches it on stdin at the
// start and so is held in memory until then, and its image.
type mcpStart struct {
	cfgJSON []byte
	image   string
}

// mcpAddrs are an MCP sandbox's pinned addresses: its proxy's and its container's on the MCP network, and its
// proxy's on the relay link.
type mcpAddrs struct {
	proxy, container, relay netip.Addr
}

// CreateMCPSandbox pulls both images, creates the sandbox's network (and the run's relay link on its first
// create), its MCP proxy and its container, and copies the server's files and the host program in. It starts
// nothing. On an error it removes what it created, but never the relay link.
func (d *Driver) CreateMCPSandbox(ctx context.Context, spec runner.MCPSandboxSpec) (runner.MCPSandbox, error) {
	if err := spec.Validate(); err != nil {
		return runner.MCPSandbox{}, err
	}
	id := spec.ID
	if id.RunID == uuid.Nil || id.Ordinal < 0 || spec.RelayRank < 0 {
		return runner.MCPSandbox{}, fmt.Errorf("docker: an MCP sandbox needs a run, an ordinal and a relay rank, not %s/%d/%d", id.RunID, id.Ordinal, spec.RelayRank)
	}
	// A registered runner's proxy decisions go through its decision sink, one producer per run.
	if d.runnerMode() {
		return runner.MCPSandbox{}, errors.New("docker: a registered runner runs no MCP sandbox")
	}
	infoRes, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return runner.MCPSandbox{}, fmt.Errorf("docker: info: %w", err)
	}
	info := infoRes.Info
	// The server is third-party code: its networks must have no address on the Docker host, which needs the
	// engine runNetworkIsolation needs. EnvAllowHostReachableRunNetwork admits an agent's run, never an MCP sandbox.
	if engineMajor(info.ServerVersion) < minIsolatedEngineMajor {
		return runner.MCPSandbox{}, fmt.Errorf("docker: %w: this Docker Engine is %q, and an MCP server's sandbox needs Docker Engine %s or later; "+
			"%s does not admit one: upgrade Docker Engine, or use a remote (HTTPS) MCP server", runner.ErrMCPSandboxHostReachable, info.ServerVersion, minIsolatedEngine, EnvAllowHostReachableRunNetwork)
	}
	runtimeName, _, err := resolveRuntime(id.Class, info, d.cfg.ConfinementRuntimes)
	if err != nil {
		return runner.MCPSandbox{}, err
	}
	for _, img := range []struct{ ref, runtime string }{{spec.Image, runtimeName}, {d.proxyImage(), ""}} {
		if err := d.ensureImage(ctx, img.ref, img.runtime, func() { spec.NotifyWaiting("image: Pulling: " + img.ref) }); err != nil {
			return runner.MCPSandbox{}, err
		}
	}
	relayPort, err := mcpRelayInbound(spec.ProxyConfig, spec.RelayTokenSHA256)
	if err != nil {
		return runner.MCPSandbox{}, err
	}
	linkSubnet, err := d.ensureRelayLink(ctx, spec, info.ServerVersion)
	if err != nil {
		return runner.MCPSandbox{}, err
	}
	created := []mcpUndo{}
	undo := func() {
		// Background, like CreateSandbox's rollback: a cancelled create must not strand what it made. A container
		// that cannot be removed keeps the MCP network, so the run-wide listing still finds the sandbox by both.
		kept := false
		for i := len(created) - 1; i >= 0; i-- {
			if u := created[i]; !(u.network && kept) {
				kept = u.remove() != nil || kept
			}
		}
	}
	sb, err := d.createMCPObjects(ctx, spec, info, runtimeName, linkSubnet, relayPort, &created)
	if err != nil {
		undo()
		return runner.MCPSandbox{}, err
	}
	return sb, nil
}

// mcpUndo is one removal of a failed create; network marks the MCP network's.
type mcpUndo struct {
	remove  func() error
	network bool
}

// createMCPObjects is CreateMCPSandbox's creates, each appending its removal to created.
func (d *Driver) createMCPObjects(ctx context.Context, spec runner.MCPSandboxSpec, info system.Info, runtimeName string,
	linkSubnet netip.Prefix, relayPort string, created *[]mcpUndo) (runner.MCPSandbox, error) {
	id := spec.ID
	netName := mcpNetName(id.RunID, id.Ordinal)
	netID, err := d.createNamedRunNetwork(ctx, netName, d.labels(id.RunID, componentMCPNetwork, spec.Labels), info.ServerVersion)
	if err != nil {
		return runner.MCPSandbox{}, err
	}
	*created = append(*created, mcpUndo{network: true, remove: func() error {
		rctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		defer cancel()
		_, err := d.cli.NetworkRemove(rctx, netID, client.NetworkRemoveOptions{})
		return err
	}})
	netSubnet, err := d.ipv4Subnet(ctx, netID)
	if err != nil {
		return runner.MCPSandbox{}, err
	}
	var addrs mcpAddrs
	if addrs.proxy, err = hostFromTop(netSubnet, 1); err == nil {
		addrs.container, err = hostFromTop(netSubnet, 2)
	}
	if err != nil {
		return runner.MCPSandbox{}, fmt.Errorf("docker: MCP network %s: %w", netName, err)
	}
	// One address per server on the link, from its rank: a re-create of the server takes the same one.
	if addrs.relay, err = mcpRelayAddr(linkSubnet, spec.RelayRank); err != nil {
		return runner.MCPSandbox{}, err
	}
	relayAddr := net.JoinHostPort(addrs.relay.String(), relayPort)
	cfgJSON, err := d.mcpProxyConfig(id.RunID, spec.ProxyConfig, addrs.container)
	if err != nil {
		return runner.MCPSandbox{}, err
	}
	proxyID, err := d.createMCPProxy(ctx, spec, cfgJSON, addrs, relayAddr)
	if proxyID != "" {
		*created = append(*created, mcpUndo{remove: func() error { return d.removeCreated(proxyID) }})
	}
	if err != nil {
		return runner.MCPSandbox{}, err
	}
	ctrID, err := d.createMCPContainer(ctx, spec, info, runtimeName, addrs)
	if ctrID != "" {
		*created = append(*created, mcpUndo{remove: func() error { return d.removeCreated(ctrID) }})
	}
	if err != nil {
		return runner.MCPSandbox{}, err
	}
	if err := d.deliverManagedFiles(ctx, ctrID, spec.ManagedFiles); err != nil {
		return runner.MCPSandbox{}, err
	}
	if err := d.copyMCPHost(ctx, proxyID, ctrID); err != nil {
		return runner.MCPSandbox{}, err
	}
	d.mcpStarts.Store(mcpProxyName(id.RunID, id.Ordinal), mcpStart{cfgJSON: cfgJSON, image: d.proxyImage()})
	return runner.MCPSandbox{ID: id, RelayAddr: relayAddr}, nil
}

// removeCreated removes a container a failed create made; one that is gone already counts as removed.
func (d *Driver) removeCreated(id string) error {
	rctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	_, err := d.cli.ContainerRemove(rctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	if isNotFound(err) {
		return nil
	}
	return err
}

// ensureRelayLink answers the subnet of the run's relay link, creating the link on the run's first MCP create.
// A link that is there is reused, so its subnet and every relay address on it hold across re-creates.
func (d *Driver) ensureRelayLink(ctx context.Context, spec runner.MCPSandboxSpec, engineVersion string) (netip.Prefix, error) {
	runID := spec.ID.RunID
	p, err := d.relayLinkSubnet(ctx, runID)
	if err == nil || !isNotFound(err) {
		return p, err
	}
	_, cerr := d.createNamedRunNetwork(ctx, relayLinkName(runID), d.labels(runID, componentRelayNetwork, spec.Labels), engineVersion)
	// Looked up again either way: a create that failed may have lost to another create of the run's.
	if p, err = d.relayLinkSubnet(ctx, runID); err != nil && cerr != nil {
		return netip.Prefix{}, cerr
	}
	return p, err
}

// relayLinkSubnet is the subnet of the run's relay link; a network under its name that is not it is refused.
func (d *Driver) relayLinkSubnet(ctx context.Context, runID uuid.UUID) (netip.Prefix, error) {
	name := relayLinkName(runID)
	res, err := d.cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err != nil {
		return netip.Prefix{}, err
	}
	if l := res.Network.Labels; l[labelRun] != runID.String() || l[labelComponent] != componentRelayNetwork || d.otherDeployment(l) {
		return netip.Prefix{}, fmt.Errorf("docker: a network answers to the run's relay link name %s but is not it", name)
	}
	return firstIPv4(res.Network.IPAM.Config, name)
}

func (d *Driver) ipv4Subnet(ctx context.Context, netID string) (netip.Prefix, error) {
	res, err := d.cli.NetworkInspect(ctx, netID, client.NetworkInspectOptions{})
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("docker: inspect network %s: %w", netID, err)
	}
	return firstIPv4(res.Network.IPAM.Config, netID)
}

func firstIPv4(cfgs []network.IPAMConfig, name string) (netip.Prefix, error) {
	for _, c := range cfgs {
		if c.Subnet.Addr().Is4() {
			return c.Subnet, nil
		}
	}
	return netip.Prefix{}, fmt.Errorf("docker: network %s has no IPv4 subnet", name)
}

// hostFromTop is the address n below the last of the IPv4 subnet p. Pinned addresses are taken from the top so
// they stay clear of the gateway Docker takes and of the addresses it hands out from the bottom (the agent's
// proxy on the relay link, pinned at relayAgentProxyHost); one that would reach the subnet's first three is refused.
func hostFromTop(p netip.Prefix, n int) (netip.Addr, error) {
	p = p.Masked()
	if !p.Addr().Is4() {
		return netip.Addr{}, fmt.Errorf("subnet %s is not IPv4", p)
	}
	size := uint64(1) << (32 - p.Bits())
	if n < 1 || uint64(n)+3 >= size {
		return netip.Addr{}, fmt.Errorf("subnet %s has no room for address %d from the top", p, n)
	}
	b := p.Addr().As4()
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], uint32(uint64(binary.BigEndian.Uint32(b[:]))+size-1-uint64(n)))
	return netip.AddrFrom4(out), nil
}

// hostFromBottom is the address n above the first of the IPv4 subnet p.
func hostFromBottom(p netip.Prefix, n int) (netip.Addr, error) {
	p = p.Masked()
	if !p.Addr().Is4() || n < 1 || uint64(n)+1 >= uint64(1)<<(32-p.Bits()) {
		return netip.Addr{}, fmt.Errorf("subnet %s has no room for address %d from the bottom", p, n)
	}
	b := p.Addr().As4()
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], binary.BigEndian.Uint32(b[:])+uint32(n))
	return netip.AddrFrom4(out), nil
}

// mcpRelayAddr is the relay address of the MCP server of rank on the run's relay link: n = 1 + rank from the top,
// so each rank has its own, and the first three from the bottom stay the network's, its gateway's and the agent's
// proxy's.
func mcpRelayAddr(link netip.Prefix, rank int) (netip.Addr, error) {
	if a, err := hostFromTop(link, 1+rank); err == nil && rank >= 0 {
		return a, nil
	}
	room := 0
	if link.Addr().Is4() && link.Bits() <= 30 {
		room = 1<<(32-link.Bits()) - 4
	}
	return netip.Addr{}, fmt.Errorf("docker: the run's relay link (%s) has room for %d MCP servers, and this is server number %d. "+
		"Give run networks a larger subnet (%s), or attach fewer MCP servers that start from a command", link.Masked(), room, rank+1, EnvRunSubnetSize)
}

// mcpRelayInbound is the port of the config's mcp_relay_inbound.listen, which the MCP proxy keeps when its host is
// rewritten to its relay-link address (bindProxyListeners). The config's token_sha256 must be the spec's.
func mcpRelayInbound(cfgJSON []byte, tokenSHA256 string) (string, error) {
	var c struct {
		Inbound *struct {
			Listen      string `json:"listen"`
			TokenSHA256 string `json:"token_sha256"`
		} `json:"mcp_relay_inbound"`
	}
	if err := json.Unmarshal(cfgJSON, &c); err != nil {
		return "", fmt.Errorf("docker: MCP proxy config: %w", err)
	}
	if c.Inbound == nil {
		return "", errors.New("docker: an MCP proxy's config needs mcp_relay_inbound")
	}
	if tokenSHA256 == "" || c.Inbound.TokenSHA256 != tokenSHA256 {
		return "", errors.New("docker: the MCP proxy config's mcp_relay_inbound.token_sha256 is not the spec's RelayTokenSHA256")
	}
	_, port, err := net.SplitHostPort(c.Inbound.Listen)
	if err != nil || port == "" {
		return "", fmt.Errorf("docker: MCP proxy config mcp_relay_inbound.listen %q has no port", c.Inbound.Listen)
	}
	return port, nil
}

// mcpProxyConfig is the control plane's MCP proxy config with this runner's trusted CA in, no spool directory
// (runnerSinkConfig), and mcp_relay_inbound.upstream set to the host program in the container. It adds nothing
// an MCP proxy's config may not carry: no upstream proxy, no allowed peers, no local models.
func (d *Driver) mcpProxyConfig(runID uuid.UUID, cfgJSON []byte, container netip.Addr) ([]byte, error) {
	cfgJSON, err := runner.WithTrustedCA(cfgJSON, d.cfg.TrustedCAPEM)
	if err != nil {
		return nil, err
	}
	if cfgJSON, err = d.runnerSinkConfig(runID, cfgJSON); err != nil {
		return nil, err
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(cfgJSON, &data); err != nil {
		return nil, fmt.Errorf("docker: MCP proxy config: %w", err)
	}
	var inbound map[string]json.RawMessage
	if err := json.Unmarshal(data["mcp_relay_inbound"], &inbound); err != nil || inbound == nil {
		return nil, errors.New("docker: an MCP proxy's config needs mcp_relay_inbound")
	}
	if inbound["upstream"], err = json.Marshal(net.JoinHostPort(container.String(), mcpHostPort)); err != nil {
		return nil, err
	}
	if data["mcp_relay_inbound"], err = json.Marshal(inbound); err != nil {
		return nil, err
	}
	return json.Marshal(data)
}

// createMCPProxy creates the MCP proxy, not started: on the MCP network at its pinned address, then joined to the
// control-plane-facing network and to the relay link at its pinned relay address. Its DNS listener is its MCP
// network address, port 53, the container's one resolver. It answers the id of what it created, also with an error.
func (d *Driver) createMCPProxy(ctx context.Context, spec runner.MCPSandboxSpec, cfgJSON []byte, addrs mcpAddrs, relayAddr string) (string, error) {
	runID, ord := spec.ID.RunID, spec.ID.Ordinal
	var cp struct {
		URL string `json:"control_plane_url"`
	}
	if err := json.Unmarshal(cfgJSON, &cp); err != nil {
		return "", fmt.Errorf("docker: MCP proxy config: %w", err)
	}
	env := append(proxyEnvFromJSON(runID, cp.URL), proxy.DNSListenEnv+"="+net.JoinHostPort(addrs.proxy.String(), "53"))
	labels := d.labels(runID, componentMCPProxy, spec.Labels)
	labels[labelMCPRelayAddr] = relayAddr
	cfg := &container.Config{
		Image:    d.proxyImage(),
		Hostname: "wardyn-mcp-proxy",
		Labels:   labels,
		Env:      env,
		// The config's only way in, as for the agent's proxy (startProxyWithMAC).
		OpenStdin: true,
		StdinOnce: true,
	}
	if len(d.cfg.ProxyCmd) > 0 {
		cfg.Cmd = d.cfg.ProxyCmd
	}
	netName := mcpNetName(runID, ord)
	host := &container.HostConfig{
		NetworkMode:   container.NetworkMode(netName),
		CapDrop:       []string{"ALL"},
		SecurityOpt:   []string{"no-new-privileges"},
		Tmpfs:         map[string]string{"/tmp": "rw,nosuid,nodev,noexec,size=64m"},
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
		// The control plane in host mode, as for the agent's proxy; consulted only by the control-plane forward.
		ExtraHosts: append([]string{"host.docker.internal:host-gateway"}, d.cfg.TestProxyExtraHosts...),
		Resources:  proxyResources(false),
	}
	resp, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     cfg,
		HostConfig: host,
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			netName: {IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: addrs.proxy}},
		}},
		Name: mcpProxyName(runID, ord),
	})
	if err != nil {
		return "", fmt.Errorf("docker: create MCP proxy: %w", err)
	}
	for _, join := range []struct {
		net string
		ep  *network.EndpointSettings
	}{
		{d.cfg.InternalNetwork, &network.EndpointSettings{}},
		{relayLinkName(runID), &network.EndpointSettings{IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: addrs.relay}}},
	} {
		if _, err := d.cli.NetworkConnect(ctx, join.net, client.NetworkConnectOptions{Container: resp.ID, EndpointConfig: join.ep}); err != nil {
			return resp.ID, fmt.Errorf("docker: connect MCP proxy to %s: %w", join.net, err)
		}
	}
	return resp.ID, nil
}

// createMCPContainer creates the MCP container, not started: the run's image hardened as the agent is, on the MCP
// network only at its pinned address, its one resolver its proxy, no mount but a tmpfs at /tmp beside its own
// writable layer, no image volume, and the host program as its entrypoint. Its environment is the server's own, Wardyn's closed list, and the proxy
// variables last, so they win. It answers the id of what it created, also with an error.
func (d *Driver) createMCPContainer(ctx context.Context, spec runner.MCPSandboxSpec, info system.Info, runtimeName string, addrs mcpAddrs) (string, error) {
	runID, ord := spec.ID.RunID, spec.ID.Ordinal
	netName := mcpNetName(runID, ord)
	host := hardenedHostConfig(netName, runtimeName, spec.Resources, info)
	host.Tmpfs = nil
	host.Mounts = []mount.Mount{{Type: mount.TypeTmpfs, Target: "/tmp", TmpfsOptions: &mount.TmpfsOptions{
		SizeBytes: mcpScratchBytes, Options: [][]string{{"noexec"}, {"nosuid"}, {"nodev"}},
	}}}
	host.DNS = []netip.Addr{addrs.proxy}
	host.ExtraHosts = []string{"wardyn-mcp-proxy:" + addrs.proxy.String()}
	if err := d.refuseMCPImageVolumes(ctx, spec.Image, host); err != nil {
		return "", err
	}
	proxyURL := "http://wardyn-mcp-proxy:" + strconv.Itoa(runner.ProxyListenPort)
	env := slices.Concat(envSlice(spec.Env), envSlice(spec.SecretEnv), envSlice(d.withRunnerTrustedCA(spec.PlatformEnv)), []string{
		"HTTP_PROXY=" + proxyURL, "HTTPS_PROXY=" + proxyURL, "http_proxy=" + proxyURL, "https_proxy=" + proxyURL,
		"NO_PROXY=wardyn-mcp-proxy,localhost,127.0.0.1,::1", "no_proxy=wardyn-mcp-proxy,localhost,127.0.0.1,::1",
	})
	resp, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:      spec.Image,
			Hostname:   "mcp",
			Env:        env,
			Labels:     d.labels(runID, componentMCP, spec.Labels),
			Entrypoint: []string{runner.MCPHostPath},
		},
		HostConfig: host,
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			netName: {IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: addrs.container}},
		}},
		Name: mcpContainerName(runID, ord),
	})
	if err != nil {
		return "", fmt.Errorf("docker: create MCP container: %w", err)
	}
	// As for the agent: a limit the daemon discarded refuses the sandbox before anything runs in it.
	if capErr := verifyCapsEnforced(resp.Warnings); capErr != nil {
		if !d.cfg.AllowUnenforceableCaps {
			return resp.ID, capErr
		}
		slog.Warn("wardynd: the daemon discarded a resource limit of an MCP sandbox — proceeding because WARDYN_ALLOW_UNENFORCEABLE_CAPS=1",
			slog.String("detail", capErr.Error()))
	}
	return resp.ID, nil
}

// refuseMCPImageVolumes refuses an image whose VOLUME no mount of host covers: the daemon would back it with an
// anonymous volume, a writable place on the Docker host outside the container (openVolumes, as the agent's check).
func (d *Driver) refuseMCPImageVolumes(ctx context.Context, image string, host *container.HostConfig) error {
	insp, err := d.cli.ImageInspect(ctx, image)
	if err != nil {
		return fmt.Errorf("docker: read image %s to check its volumes: %w", image, err)
	}
	if insp.Config == nil {
		return nil
	}
	if open := openVolumes(slices.Collect(maps.Keys(insp.Config.Volumes)), coveredTargets(host)); len(open) > 0 {
		return fmt.Errorf("docker: image %s declares VOLUME %s, and an MCP server's sandbox takes no volume: use an image without it",
			image, strings.Join(open, ", "))
	}
	return nil
}

// copyMCPHost copies the host program out of the created MCP proxy and into the created container at
// runner.MCPHostPath: one archive at /etc holding its directory (root, 0755) and the program (root, 0555),
// extracted root-owned. Its directory must not be there yet, the rule deliverManagedFiles keeps: a directory the
// image ships could be one its user owns.
func (d *Driver) copyMCPHost(ctx context.Context, proxyID, ctrID string) error {
	dir := path.Dir(runner.MCPHostPath)
	_, err := d.cli.ContainerStatPath(ctx, ctrID, client.ContainerStatPathOptions{Path: dir})
	if err == nil {
		return fmt.Errorf("docker: %s already exists in the MCP server's image; the host program is copied only into a directory the copy creates", dir)
	}
	if !isNotFound(err) {
		return fmt.Errorf("docker: stat %s: %w", dir, err)
	}
	hdr, body, err := d.firstArchiveEntry(ctx, proxyID, mcpHostImagePath, mcpHostMaxBytes+1)
	if err != nil {
		return fmt.Errorf("docker: read %s from the MCP proxy: %w", mcpHostImagePath, err)
	}
	if hdr.Typeflag != tar.TypeReg || len(body) > mcpHostMaxBytes || int64(len(body)) != hdr.Size {
		return fmt.Errorf("docker: %s in the proxy image is not a regular file of at most %d bytes", mcpHostImagePath, mcpHostMaxBytes)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	base := path.Base(dir)
	for _, h := range []*tar.Header{
		{Name: base + "/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: managedFileEpoch},
		{Name: base + "/" + path.Base(runner.MCPHostPath), Typeflag: tar.TypeReg, Mode: 0o555, Size: int64(len(body)), ModTime: managedFileEpoch},
	} {
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
	}
	if _, err := tw.Write(body); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	// CopyUIDGID false: the daemon would otherwise chown the tree to the image's user (deliverManagedFiles).
	if _, err := d.cli.CopyToContainer(ctx, ctrID, client.CopyToContainerOptions{DestinationPath: path.Dir(dir), Content: &buf, CopyUIDGID: false}); err != nil {
		return fmt.Errorf("docker: copy the host program into the MCP container: %w", err)
	}
	return nil
}

// StartMCPSandbox starts the MCP proxy with its config, waits until it says it is listening, then starts the
// container. On a failure it stops the proxy it started; the row's end removes the rest. Only the process that
// created the sandbox holds its proxy's config, so a sandbox another process created is refused
// (runner.ErrMCPSandboxNotCreatedHere).
func (d *Driver) StartMCPSandbox(ctx context.Context, id runner.MCPSandboxID) error {
	name := mcpProxyName(id.RunID, id.Ordinal)
	v, ok := d.mcpStarts.LoadAndDelete(name)
	if !ok {
		return fmt.Errorf("docker: MCP sandbox %s/%d: %w; kill it and create it again", id.RunID, id.Ordinal, runner.ErrMCPSandboxNotCreatedHere)
	}
	st := v.(mcpStart)
	marker := d.proxyPrintsReadyMarker(ctx, st.image)
	att, _, err := d.startWithConfig(ctx, id.RunID, name, st.cfgJSON, marker)
	if err == nil {
		defer att.Close()
		if marker {
			err = d.awaitProxyReady(ctx, name, att)
		} else {
			err = d.watchProxyExit(ctx, name)
		}
	}
	if err == nil {
		if _, err = d.cli.ContainerStart(ctx, mcpContainerName(id.RunID, id.Ordinal), client.ContainerStartOptions{}); err != nil {
			err = fmt.Errorf("docker: start MCP container: %w", err)
		}
	}
	if err != nil {
		// The stop outlives a cancelled start, bounded by its own stop timeout and a kill.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout+mcpFinalKillBound)
		defer cancel()
		return errors.Join(err, d.stopMCPProxy(sctx, id.RunID, id.Ordinal))
	}
	return nil
}

// KillMCPSandbox stops the MCP proxy as StopMCPProxies does, then removes the container and the MCP network. The
// relay link stays.
func (d *Driver) KillMCPSandbox(ctx context.Context, id runner.MCPSandboxID) error {
	d.drainSink(ctx, id.RunID)
	return d.killMCP(ctx, id.RunID, id.Ordinal)
}

// KillRunMCPSandboxes kills every MCP sandbox of the run found by listing, never the relay link or an agent object.
func (d *Driver) KillRunMCPSandboxes(ctx context.Context, runID uuid.UUID) error {
	ords, err := d.runMCPOrdinals(ctx, runID)
	if err != nil || len(ords) == 0 {
		return err
	}
	d.drainSink(ctx, runID)
	return eachMCPOrdinal(ords, func(ord int) error { return d.killMCP(ctx, runID, ord) })
}

// eachMCPOrdinal runs fn for every ordinal at once, so the run's proxies' stop timeouts overlap instead of adding
// up, and joins the errors.
func eachMCPOrdinal(ords []int, fn func(ord int) error) error {
	errs := make([]error, len(ords))
	var wg sync.WaitGroup
	for i, ord := range ords {
		wg.Go(func() { errs[i] = fn(ord) })
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (d *Driver) killMCP(ctx context.Context, runID uuid.UUID, ord int) error {
	d.mcpStarts.Delete(mcpProxyName(runID, ord))
	err := d.stopMCPProxy(ctx, runID, ord)
	if _, rerr := d.cli.ContainerRemove(ctx, mcpContainerName(runID, ord), client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); rerr != nil && !isNotFound(rerr) {
		err = errors.Join(err, fmt.Errorf("docker: remove MCP container: %w", rerr))
	}
	if err != nil {
		return err // the network stays for a retry to find the sandbox by
	}
	if _, err := d.cli.NetworkRemove(ctx, mcpNetName(runID, ord), client.NetworkRemoveOptions{}); err != nil && !isNotFound(err) {
		return fmt.Errorf("docker: remove MCP network: %w", err)
	}
	return nil
}

// removeRunMCPSandboxes is teardown's: every MCP sandbox of the run, then its relay link.
func (d *Driver) removeRunMCPSandboxes(ctx context.Context, runID uuid.UUID) error {
	err := d.KillRunMCPSandboxes(ctx, runID)
	if _, lerr := d.cli.NetworkRemove(ctx, relayLinkName(runID), client.NetworkRemoveOptions{}); lerr != nil && !isNotFound(lerr) {
		err = errors.Join(err, fmt.Errorf("docker: remove the relay link: %w", lerr))
	}
	return err
}

// StopMCPProxies stops every MCP proxy of the agent ref's run as stopProxy does the agent's: a drain on a runner,
// a stop with stopTimeout so it delivers its buffered decisions, a kill only if the stop fails, then the remove.
// The MCP containers stay; with no proxy they reach nothing.
func (d *Driver) StopMCPProxies(ctx context.Context, agentRef string) error {
	runID, err := d.proxyRunID(ctx, agentRef)
	if err != nil {
		return err
	}
	ords, err := d.runMCPOrdinals(ctx, runID)
	if err != nil || len(ords) == 0 {
		return err
	}
	d.drainSink(ctx, runID)
	return eachMCPOrdinal(ords, func(ord int) error { return d.stopMCPProxy(ctx, runID, ord) })
}

func (d *Driver) stopMCPProxy(ctx context.Context, runID uuid.UUID, ord int) error {
	name := mcpProxyName(runID, ord)
	timeout := int(stopTimeout.Seconds())
	if _, err := d.cli.ContainerStop(ctx, name, client.ContainerStopOptions{Timeout: &timeout}); err != nil && !isNotFound(err) {
		// A stop cut short by its context still ends in one kill, bounded, so the proxy does not outlive the call.
		kctx := ctx
		if ctx.Err() != nil {
			var cancel context.CancelFunc
			kctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), mcpFinalKillBound)
			defer cancel()
		}
		if _, kerr := d.cli.ContainerKill(kctx, name, client.ContainerKillOptions{Signal: "KILL"}); kerr != nil && !isNotFound(kerr) && !isNotRunning(kerr) {
			return fmt.Errorf("docker: stop MCP proxy: %w; kill MCP proxy: %w", err, kerr)
		}
	}
	if _, err := d.cli.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true}); err != nil && !isNotFound(err) {
		return fmt.Errorf("docker: remove MCP proxy: %w", err)
	}
	return nil
}

// MCPSandboxStatus reports the sandbox live only while both its container and its MCP proxy are running: a cut
// stops the proxy and leaves the container, so a cut sandbox reads as dead.
func (d *Driver) MCPSandboxStatus(ctx context.Context, id runner.MCPSandboxID) (runner.MCPSandbox, error) {
	sb := runner.MCPSandbox{ID: id}
	running := func(name string) (container.InspectResponse, bool, error) {
		res, err := d.cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
		if isNotFound(err) {
			return container.InspectResponse{}, false, nil
		}
		if err != nil {
			return container.InspectResponse{}, false, fmt.Errorf("docker: inspect %s: %w", name, err)
		}
		return res.Container, res.Container.State != nil && res.Container.State.Running, nil
	}
	p, proxyUp, err := running(mcpProxyName(id.RunID, id.Ordinal))
	if err != nil {
		return sb, err
	}
	_, ctrUp, err := running(mcpContainerName(id.RunID, id.Ordinal))
	if err != nil {
		return sb, err
	}
	if p.Config != nil {
		sb.RelayAddr = p.Config.Labels[labelMCPRelayAddr]
	}
	sb.Live = proxyUp && ctrUp
	return sb, nil
}

// runMCPOrdinals lists the ordinals of the run's MCP sandboxes by their networks and by their containers, so a
// sandbox is found while any of its objects is, also a container whose network is gone.
func (d *Driver) runMCPOrdinals(ctx context.Context, runID uuid.UUID) ([]int, error) {
	res, err := d.cli.NetworkList(ctx, client.NetworkListOptions{Filters: client.Filters{}.Add("label", labelRun+"="+runID.String())})
	if err != nil {
		return nil, fmt.Errorf("docker: list the run's MCP networks: %w", err)
	}
	var ords []int
	add := func(l map[string]string, components []string, name string) {
		if l[labelRun] != runID.String() || !slices.Contains(components, l[labelComponent]) || l[labelManaged] != "true" || d.otherDeployment(l) {
			return
		}
		if ord, ok := mcpOrdinalOf(name, runID); ok && !slices.Contains(ords, ord) {
			ords = append(ords, ord)
		}
	}
	for _, n := range res.Items {
		add(n.Labels, []string{componentMCPNetwork}, n.Name)
	}
	if lister, ok := d.cli.(containerListerAPI); ok {
		cres, err := lister.ContainerList(ctx, client.ContainerListOptions{
			All: true, // a created or exited sandbox is the one most likely left behind
			// Label filters are ANDed: the run's, and any component (matched above).
			Filters: client.Filters{}.Add("label", labelComponent).Add("label", labelRun+"="+runID.String()),
		})
		if err != nil {
			return nil, fmt.Errorf("docker: list the run's MCP containers: %w", err)
		}
		for _, c := range cres.Items {
			for _, name := range c.Names {
				add(c.Labels, []string{componentMCP, componentMCPProxy}, name)
			}
		}
	}
	slices.Sort(ords)
	return ords, nil
}

// mcpOrdinalOf is the ordinal an MCP object of runID is named for (mcpNetName, mcpProxyName, mcpContainerName).
func mcpOrdinalOf(name string, runID uuid.UUID) (int, bool) {
	name = strings.TrimPrefix(name, "/")
	_, rest, ok := strings.Cut(name, runID.String()+"-")
	if !ok {
		return 0, false
	}
	ord, err := strconv.Atoi(rest)
	if err != nil || ord < 0 {
		return 0, false
	}
	return ord, slices.Contains([]string{mcpNetName(runID, ord), mcpProxyName(runID, ord), mcpContainerName(runID, ord)}, name)
}

// joinRelayLink connects the run's proxy cid to the run's relay link, when its MCP sandboxes made one, pinned at
// relayAgentProxyHost so no address the daemon hands out can be an MCP proxy's. The agent container never joins it.
func (d *Driver) joinRelayLink(ctx context.Context, runID uuid.UUID, cid string) error {
	link, err := d.relayLinkSubnet(ctx, runID)
	if isNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	at, err := hostFromBottom(link, relayAgentProxyHost)
	if err != nil {
		return fmt.Errorf("docker: relay link %s: %w", relayLinkName(runID), err)
	}
	ep := &network.EndpointSettings{IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: at}}
	if _, err := d.cli.NetworkConnect(ctx, relayLinkName(runID), client.NetworkConnectOptions{Container: cid, EndpointConfig: ep}); err != nil {
		return fmt.Errorf("docker: connect proxy to the relay link: %w", err)
	}
	return nil
}

// refuseMCPRef refuses a shell, exec or attach into an MCP container or MCP proxy: a ref under
// runner.MCPSandboxNamePrefix, or a container that carries an MCP component label. The agent's own name needs no
// look; a container that is not there is left for the call itself to answer.
func (d *Driver) refuseMCPRef(ctx context.Context, ref string) error {
	name := strings.TrimPrefix(ref, "/")
	if strings.HasPrefix(name, runner.MCPSandboxNamePrefix) {
		return errMCPNoShell
	}
	if _, err := runIDFromAgentName(name); err == nil || name == "" {
		return nil
	}
	res, err := d.cli.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("docker: cannot tell whether %s is an MCP sandbox: %w", ref, err)
	}
	if strings.HasPrefix(strings.TrimPrefix(res.Container.Name, "/"), runner.MCPSandboxNamePrefix) ||
		res.Container.Config != nil && slices.Contains([]string{componentMCP, componentMCPProxy}, res.Container.Config.Labels[labelComponent]) {
		return errMCPNoShell
	}
	return nil
}
