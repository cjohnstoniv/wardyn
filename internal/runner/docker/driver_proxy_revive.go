// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// proxyStartWatch/proxyStartWatchInterval bound how long startProxy waits to
// see whether a just-started proxy stays up, versus exiting right back out
// because it refused its own rendered config. Kept short: this sits on the
// create-latency path for every run, and a HEALTHY proxy reaches a STABLE
// Running well inside it — the watch exists to catch the FAST config-load
// failure, not to babysit a slow one.
//
// proxyStartSettle is how long Running must have HELD before the watch trusts
// it (Driver.proxySettle's production value, set by New()). Docker flips
// container.State.Running to true the INSTANT ContainerStart returns — well
// before the sidecar's own process has read and decoded its config. Against a
// real daemon, a real wardyn-proxy given an unknown config key was observed
// dying 47-451ms after that first Running sighting, and a watch that returned
// on the FIRST Running observation (no settle check) caught that failure only
// ~2/10 times. Any exit inside the settle window is reported with the SAME
// fixed cause string below: from the driver's side, a proxy that never
// stayed up long enough to matter IS a config-load failure, whatever
// actually killed it.
//
// The settle window is measured from max(the container's own StartedAt, the
// watch's own start), never from StartedAt alone: a daemon whose clock lags
// the host's can report a StartedAt already several seconds in the past on
// the very FIRST inspect, which would let that inspect look already settled.
// See proxySettleSince.
const (
	proxyStartWatch         = 3 * time.Second
	proxyStartWatchInterval = 200 * time.Millisecond
	proxyStartSettle        = 1 * time.Second
)

// proxyConfigStdinEnv tells the proxy sidecar its config arrives on stdin:
// the rendered config (run token, per-run MITM CA key, upstream-proxy
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
		Image:    d.proxyImage(),
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
		// control-plane forward can reach a wardynd running on the host in
		// host mode. Docker Desktop injects this alias automatically; native
		// docker needs the explicit mapping. Scoped to the proxy and
		// consulted ONLY by the trusted forward path, never by the agent's
		// policy-governed egress (still denies host IPs via the private-IP
		// guard) — general egress is NOT broadened. On a runner both this name
		// and the control-plane name map to the relay's target below, so the
		// proxy's control-plane clamp covers the one address either resolves to.
		ExtraHosts: []string{"host.docker.internal:host-gateway"},
		// SECURITY: the proxy only relays HTTP, so a tight resource envelope
		// still leaves ample headroom while bounding a compromised proxy: its
		// own PID cap (fork-bomb guard) and a modest memory cap (MemorySwap
		// pinned so the cap isn't silently doubled via swap).
		Resources: proxyResources(runner.ConfigHasAzureGates(cfgJSON)),
	}
	if d.cfg.ProxyBinaryHostPath != "" {
		host.Binds = []string{d.cfg.ProxyBinaryHostPath + ":/usr/local/bin/wardyn-proxy:ro"}
	}
	if d.cfg.ControlPlaneRelay != nil {
		name, target, err := d.relayHostsEntry(cfgJSON)
		if err != nil {
			return "", err
		}
		if err := d.checkRelayOutsideRunSubnet(ctx, runID, target); err != nil {
			return "", err
		}
		host.ExtraHosts = []string{"host.docker.internal:" + target, name + ":" + target}
	}
	host.ExtraHosts = append(host.ExtraHosts, d.cfg.TestProxyExtraHosts...) // empty outside tests (New)
	cfgJSON, err := runner.WithTrustedCA(cfgJSON, d.cfg.TrustedCAPEM)
	if err != nil {
		return "", err
	}
	if cfgJSON, err = withUpstreamProxy(cfgJSON, d.cfg.UpstreamProxyURL); err != nil {
		return "", err
	}
	cfgJSON, err = d.runnerSpoolConfig(ctx, runID, cfgJSON, cfg, host)
	if err != nil {
		return "", err
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
	if err := d.startWithConfig(ctx, runID, resp.ID, cfgJSON); err != nil {
		remove()
		return "", err
	}
	// Watch briefly for the proxy exiting right back out — the shape of it
	// refusing its own rendered config at boot (a strict-decode error, an
	// unreadable MITM key, …). Without this, CreateSandbox pressed straight on
	// to the IP lookup, which found no IP on a dead container and reported
	// the generic "proxy has no IP…" instead of the proxy's own named cause.
	//
	// On a watch failure the container is deliberately NOT removed here: on
	// the CreateSandbox path the caller removes it after also rolling back
	// the per-run network; on the ReplaceProxy path it stays so its logs
	// still name the cause, and the next revive removes it by name.
	if err := d.watchProxyExit(ctx, resp.ID); err != nil {
		return resp.ID, err
	}
	return resp.ID, nil
}

// startWithConfig attaches to the created proxy's stdin, starts it, writes
// cfgJSON there and closes it: the proxy reads its config to EOF. Attaching
// before the start is what `docker run -i` does, so no byte is written before
// the proxy's stdin exists. The config's listen address is first bound to the
// proxy's own address on runID's per-run network (see listenOnRunNetwork).
func (d *Driver) startWithConfig(ctx context.Context, runID uuid.UUID, id string, cfgJSON []byte) error {
	att, err := d.cli.ContainerAttach(ctx, id, client.ContainerAttachOptions{Stream: true, Stdin: true})
	if err != nil {
		return fmt.Errorf("docker: attach proxy stdin: %w", err)
	}
	defer att.Close()
	if _, err := d.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("docker: start proxy: %w", err)
	}
	if cfgJSON, err = d.listenOnRunNetwork(ctx, runID, id, cfgJSON); err != nil {
		return err
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

// listenOnRunNetwork returns cfgJSON with its listen address set to the
// started proxy id's address on runID's per-run network, keeping the port.
// The proxy also sits on the control-plane-facing network, which other
// containers share; binding to the per-run address keeps its listener off
// that network, so only the run's own agent can reach it. The address exists
// only once the container has started, which is why this runs between the
// start and the config write.
func (d *Driver) listenOnRunNetwork(ctx context.Context, runID uuid.UUID, id string, cfgJSON []byte) ([]byte, error) {
	res, err := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("docker: inspect proxy for its listen address: %w", err)
	}
	var addr netip.Addr
	if ns := res.Container.NetworkSettings; ns != nil {
		if ep := ns.Networks[internalNetName(runID)]; ep != nil {
			addr = ep.IPAddress
		}
	}
	if !addr.IsValid() || addr.IsUnspecified() {
		return nil, fmt.Errorf("docker: proxy has no IP on the per-run internal network %q to listen on", internalNetName(runID))
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(cfgJSON, &data); err != nil {
		return nil, fmt.Errorf("docker: proxy config: %w", err)
	}
	var listen string
	if err := json.Unmarshal(data["listen"], &listen); err != nil {
		return nil, fmt.Errorf("docker: proxy config listen: %w", err)
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, fmt.Errorf("docker: proxy config listen %q: %w", listen, err)
	}
	if data["listen"], err = json.Marshal(net.JoinHostPort(addr.Unmap().String(), port)); err != nil {
		return nil, err
	}
	return json.Marshal(data)
}

// watchProxyExit polls id for up to proxyStartWatch, returning a named error
// the moment it observes the container exited with a non-zero code (it
// refused its config at start) and nil once it has observed Running held for
// at least d.proxySettle (the common case) or once the window elapses without
// either — a proxy still mid-start at that point is left to the caller's own
// next step (the IP lookup, or a subsequent ProxyConfig read) rather than
// this watch inventing a second timeout.
func (d *Driver) watchProxyExit(ctx context.Context, id string) error {
	watchStart := time.Now()
	deadline := watchStart.Add(proxyStartWatch)
	for {
		res, err := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err != nil {
			// Can't observe it — leave the verdict to whatever inspects next.
			return nil
		}
		st := res.Container.State
		if st != nil {
			// Checked BEFORE the Running/settle branch, and regardless of
			// Running: a container that has already exited is never also
			// reported Running, so there is no ordering ambiguity — but
			// checking the terminal state first keeps that true even if a
			// future daemon/fake ever reported both transiently.
			if st.ExitCode != 0 {
				return fmt.Errorf("docker: proxy exited at config load (exit %d): %s", st.ExitCode, d.proxyExitLogTail(ctx, id))
			}
			if st.Running && time.Since(proxySettleSince(st, watchStart)) >= d.proxySettle {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(proxyStartWatchInterval):
		}
	}
}

// proxySettleSince resolves the moment watchProxyExit measures its settle
// window from: the LATER of the container's own StartedAt (Docker's
// RFC3339Nano timestamp) and watchStart, never earlier than watchStart. A
// missing/unparseable StartedAt leaves watchStart standing; the case this
// guards is a PARSEABLE StartedAt further in the past than watchStart, which
// a clock-skewed daemon can report on the very first inspect — using it
// directly would let that inspect already look "settled". max() refuses to
// count any time before the watch itself began, so a skewed StartedAt can
// only make the wait LONGER or be ignored, never shorter.
func proxySettleSince(st *container.State, watchStart time.Time) time.Time {
	since := watchStart
	if t, err := time.Parse(time.RFC3339Nano, st.StartedAt); err == nil && t.After(since) {
		since = t
	}
	return since
}

// proxyExitLogTail reads the last ~20 lines the proxy wrote before dying, so
// the failure the operator sees names the actual decode/config error instead
// of just an exit code. Best-effort: a log-read failure yields a placeholder
// rather than losing the exit-code error it is decorating.
func (d *Driver) proxyExitLogTail(ctx context.Context, id string) string {
	rc, err := d.cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Tail: "20"})
	if err != nil {
		return "(no log: " + err.Error() + ")"
	}
	defer rc.Close()
	var buf bytes.Buffer
	// No TTY on the proxy container (driver_proxy_revive.go's container.Config
	// leaves Tty unset), so the stream is stdcopy-multiplexed; StdCopy strips
	// the frame headers so the tail is clean text, not binary garbage.
	_, _ = stdcopy.StdCopy(&buf, &buf, rc)
	return strings.TrimSpace(buf.String())
}

// EnsureProxyImage — see runner.ProxyReviver. ReplaceProxy also ensures the
// image itself (a cheap local check once cached); calling this first keeps a
// slow FIRST pull out of the window between the revive claim and the new
// proxy actually starting.
func (d *Driver) EnsureProxyImage(ctx context.Context) error {
	return d.ensureImage(ctx, d.proxyImage(), "", func() {})
}

// CanReplaceProxy — see runner.ProxyReviver: Docker always can.
func (d *Driver) CanReplaceProxy(context.Context, string) error { return nil }

// ReplaceProxy swaps the agent ref's proxy sidecar, if it still has one, for
// a new one running cfgJSON on the current proxy image (runner.ProxyReviver).
// The old sidecar is removed first: it holds the name, and its address is the
// one the agent pins. The new one takes that address, read from the agent's
// own hosts entry (immutable, still there after the old proxy gave its
// address back), carries the agent's labels as a proxy, and re-joins the
// control-plane-facing network as at create. Every check that can fail
// without touching the old sidecar runs before the remove. Nothing is put
// back when the new one doesn't start: the config is the control plane's,
// not the container's, so a later revive rebuilds from it.
//
// If the NEW proxy then exits at config load, startProxy's watch reports that
// (wrapped in ErrProxyReplaceFailed below) and leaves the exited container in
// place, so its logs still name the cause; the next revive removes it by name
// before starting another.
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
	// A config the new proxy would refuse at load must not cost the running one: load it here, before anything is
	// removed. This error is not ErrProxyReplaceFailed, so the revive keeps the old proxy.
	if _, err := proxy.LoadConfigBytes(cfgJSON); err != nil {
		return fmt.Errorf("docker: replace proxy: the new config does not load: %w", err)
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
	if err := d.ensureImage(ctx, d.proxyImage(), "", func() {}); err != nil {
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
// over the writable layer the run left behind. Refuses unless the run's
// proxy sidecar is running: a stopped or removed proxy has given its address
// back, and an agent started first could take the address its own hosts
// entry pins as wardyn-proxy. Recording dirs are prepared again as at create.
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
	if d.recordsAgent(ctx, ref) {
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

// relayHostsEntry names the run's control-plane hostname and the address the runner's relay
// is reachable at (design 0.9 §4.2): the proxy's hosts map the name there, where the relay
// listens and forwards the proxy's TLS, unread, to the org. The proxy keeps its URL and CA
// exactly as dispatched; only the name resolves differently, and only in the proxy, never
// the agent.
func (d *Driver) relayHostsEntry(cfgJSON []byte) (name, target string, err error) {
	var cp struct {
		ControlPlaneURL string `json:"control_plane_url"`
	}
	if err := json.Unmarshal(cfgJSON, &cp); err != nil {
		return "", "", fmt.Errorf("docker: relay: config: %w", err)
	}
	u, err := url.Parse(cp.ControlPlaneURL)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Hostname() == "" {
		return "", "", fmt.Errorf("docker: relay: the control-plane URL %q is not an https URL with a hostname", cp.ControlPlaneURL)
	}
	if _, err := netip.ParseAddr(u.Hostname()); err == nil {
		return "", "", fmt.Errorf("docker: relay: the control-plane URL %q names an IP address, which the runner cannot route to its relay; use a hostname", cp.ControlPlaneURL)
	}
	target, err = d.cfg.ControlPlaneRelay(u)
	if err != nil {
		return "", "", fmt.Errorf("docker: relay: %w", err)
	}
	if err := validRelayTarget(target); err != nil {
		return "", "", fmt.Errorf("docker: relay: %w", err)
	}
	return u.Hostname(), target, nil
}

// checkRelayOutsideRunSubnet refuses a relay target inside the run's own bridge subnet. Docker may give a
// per-run network a pool that contains the target (a VM engine's 192.168.5.2 sits in 192.168.0.0/20); the
// proxy would then route the relay onto its own bridge, where nothing answers, and the run could never
// reach the organisation. host-gateway names no address, so it cannot overlap.
func (d *Driver) checkRelayOutsideRunSubnet(ctx context.Context, runID uuid.UUID, target string) error {
	ip, err := netip.ParseAddr(target)
	if err != nil {
		return nil
	}
	name := internalNetName(runID)
	insp, err := d.cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err != nil {
		return fmt.Errorf("docker: relay: inspect the run's network %s: %w", name, err)
	}
	for _, c := range insp.Network.IPAM.Config {
		if c.Subnet.Contains(ip.Unmap()) {
			return fmt.Errorf("docker: relay: this run's network %s was given the subnet %s, which contains the relay target %s, so the run's proxy would look for the relay on its own bridge. "+
				"Move the engine's default-address-pools off %s, or point the runner's relay at an address outside %s (--relay-host-target)", name, c.Subnet, target, c.Subnet, c.Subnet)
		}
	}
	return nil
}

// validRelayTarget accepts "host-gateway" or a unicast IP literal. Loopback is the
// container's own, so it can never be the relay.
func validRelayTarget(t string) error {
	if t == "host-gateway" {
		return nil
	}
	ip, err := netip.ParseAddr(t)
	if err != nil {
		return fmt.Errorf("the relay target %q is neither host-gateway nor an IP address", t)
	}
	if ip.Zone() != "" {
		return fmt.Errorf("the relay target %q is a zoned address, which a hosts entry cannot carry", t)
	}
	// Unmap, so ::ffff:127.0.0.1 and ::ffff:0.0.0.0 are judged as the IPv4 they are.
	if u := ip.Unmap(); u.IsUnspecified() || u.IsMulticast() || u.IsLoopback() {
		return fmt.Errorf("the relay target %q is not a routable host address (unspecified, multicast or loopback)", t)
	}
	return nil
}
