// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// proxyStartWatch/proxyStartWatchInterval bound how long startProxy waits to
// see whether a just-started proxy stays up, versus exiting right back out
// because it refused its own rendered config (#894/#984). Kept short: this
// sits on the create-latency path for every run, and a HEALTHY proxy reaches
// a STABLE Running well inside it — the watch exists to catch the FAST,
// config-load failure, not to babysit a slow one.
//
// proxyStartSettle is how long Running must have HELD before the watch trusts
// it (Driver.proxySettle's production value, set by New()). Docker flips
// container.State.Running to true the INSTANT ContainerStart returns — well
// before the sidecar's own process has read and decoded its config. Against a
// real daemon, a real wardyn-proxy given an unknown config key was observed
// dying 47-451ms after that first Running sighting, and a watch that returned
// on the FIRST Running observation (no settle check) caught that failure only
// ~2/10 times (F1). Any exit inside the settle window — a bad config, an OOM
// kill, anything — is reported with the SAME fixed cause string below: from
// the driver's side, a proxy that never stayed up long enough to matter IS a
// config-load failure, whatever actually killed it.
//
// The settle window is measured from max(the container's own StartedAt, the
// watch's own start) — never from StartedAt alone (L1): a daemon whose clock
// lags the host's (a remote daemon, or Docker Desktop's VM clock after the
// host sleeps) can report a StartedAt already several seconds in the past on
// the very FIRST inspect, which would let that inspect look already settled
// and reopen F1 exactly the way a genuinely-fast exit does. See
// proxySettleSince.
const (
	proxyStartWatch         = 3 * time.Second
	proxyStartWatchInterval = 200 * time.Millisecond
	proxyStartSettle        = 1 * time.Second
)

// proxyConfigEnv is the proxy sidecar's config variable: the one place its
// rendered config, and the per-run MITM CA key inside it, rests.
const proxyConfigEnv = "WARDYN_PROXY_CONFIG_JSON"

var _ runner.ProxyReviver = (*Driver)(nil)
var _ runner.SandboxStarter = (*Driver)(nil)

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
	// Watch briefly for the proxy exiting right back out — the shape of it
	// refusing its own rendered config at boot (a strict-decode error, an
	// unreadable MITM key, …). Without this, CreateSandbox pressed straight on
	// to the IP lookup, which found no IP on a dead container and reported the
	// generic "proxy has no IP…" — never the proxy's own, named cause — and
	// k8s already surfaces that cause (sandbox.go:163-166), so Docker was the
	// odd substrate out.
	//
	// On a watch failure the container is deliberately NOT removed here (F2):
	// on the CreateSandbox path the caller removes it after also rolling back
	// the per-run network (driver_network.go); on the ReplaceProxy path the
	// exited container is the only copy left of the run's rendered config and
	// MITM CA (the OLD proxy is already gone by the time startProxy runs), and
	// removing it here would make that run unrevivable forever.
	if err := d.watchProxyExit(ctx, resp.ID); err != nil {
		return resp.ID, err
	}
	return resp.ID, nil
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
// RFC3339Nano timestamp) and watchStart, the watch's own start time — never
// earlier than watchStart (L1). A missing/unparseable StartedAt leaves
// watchStart standing, same as before; the new case this guards is a
// PARSEABLE StartedAt that is nonetheless further in the past than
// watchStart, which a clock-skewed daemon can report on the very first
// inspect. Using it directly (as F1's original fix did) would let that first
// inspect already look "settled" — max() refuses to count any time before the
// watch itself began, so a skewed StartedAt can only ever make the wait
// LONGER (if it were somehow later than watchStart) or be ignored entirely,
// never shorter.
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
// image itself (a cheap local check once it is cached); calling this first
// keeps a slow FIRST pull out of the window between the revive claim and the
// new proxy actually starting (F2).
func (d *Driver) EnsureProxyImage(ctx context.Context) error {
	return d.ensureImage(ctx, d.cfg.ProxyImage, func() {})
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
//
// If the NEW proxy then exits at config load, startProxy's watch reports that
// (wrapped in ErrProxyReplaceFailed below) WITHOUT removing the exited
// container (F2): by this point the OLD proxy is already gone, so the exited
// new one is the only copy left of the run's rendered config and MITM CA —
// removing it too would make the run unrevivable forever. ProxyConfig can
// still read a stopped-or-exited proxy's env, so a subsequent revive attempt
// (with a corrected config) has something to read back.
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

// StartSandbox starts the agent ref's kept, stopped container again
// (runner.SandboxStarter): `docker start`, which re-runs its main process
// (`agent-run --idle`) over the writable layer the run left behind. It
// refuses unless the run's proxy sidecar is running: a stopped proxy has given
// its address back, and an agent started first could take the address its own
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
