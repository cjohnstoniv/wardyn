// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	macosguest "github.com/cjohnstoniv/wardyn/deploy/macos-guest"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/vmnet/prepproxy"
)

// componentMacPrep labels a macOS base job's stage-2 network, prep proxy and relay. It is none of the
// components the orphan sweeps key on: a job is not a run, and the organisation's isOrphan would call its
// id one. runnerd removes them when the job's helper exits (StartMacPrepNet's undo), and at its start
// what a crash left (SweepMacPrep).
const componentMacPrep = "macos-prep"

// macPrepID names the Docker objects of the stage-2 network of the job holding ledger entry entryID.
func macPrepID(entryID string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("wardyn-macos-prep:"+entryID))
}

// StartMacPrepNet brings up the stage-2 network of a prepare or a tools update (design 3.6): an internal
// network; the prep-only wardyn-proxy on it, configured by prepproxy.Config (Apple's update hosts from
// prep-allowlist.json on 443 only, no credential, no MITM, serving only that network's subnet) on its
// stdin, as startProxyOn configures a run's; and a wardyn-vmnet relay serving frames on stdio with -proxy
// at that proxy. It answers the relay's attach descriptor, which runnerd hands the job's helper as net_fd,
// and the undo that removes all three. On an error it has already undone them.
func (d *Driver) StartMacPrepNet(ctx context.Context, entryID string) (*os.File, func(), error) {
	id := macPrepID(entryID)
	labels := d.labels(id, componentMacPrep, map[string]string{runner.LabelRunnerID: d.cfg.RunnerID})
	var rollback []func()
	undo := func() {
		for i := len(rollback) - 1; i >= 0; i-- {
			rollback[i]()
		}
	}
	fail := func(err error) (*os.File, func(), error) {
		undo()
		return nil, nil, err
	}
	netID, err := d.createRunNetwork(ctx, id, labels)
	if err != nil {
		return nil, nil, err
	}
	rollback = append(rollback, func() { _, _ = d.cli.NetworkRemove(context.Background(), netID, client.NetworkRemoveOptions{}) })
	insp, err := d.cli.NetworkInspect(ctx, netID, client.NetworkInspectOptions{})
	if err != nil {
		return fail(fmt.Errorf("docker: inspect the macOS prep network: %w", err))
	}
	var peers []netip.Prefix
	for _, c := range insp.Network.IPAM.Config {
		if c.Subnet.Addr().Is4() {
			peers = append(peers, c.Subnet)
		}
	}
	cfg, err := prepproxy.Config(macosguest.PrepAllowlistJSON, runner.ProxyListenPort, peers)
	if err != nil {
		return fail(fmt.Errorf("docker: %w", err))
	}
	proxyID, err := d.startPrepProxy(ctx, id, labels, cfg)
	if proxyID != "" {
		rollback = append(rollback, func() {
			_, _ = d.cli.ContainerRemove(context.Background(), proxyID, client.ContainerRemoveOptions{Force: true})
		})
	}
	if err != nil {
		return fail(err)
	}
	res, err := d.cli.ContainerInspect(ctx, proxyID, client.ContainerInspectOptions{})
	if err != nil {
		return fail(fmt.Errorf("docker: inspect the macOS prep proxy: %w", err))
	}
	var proxyIP netip.Addr
	if ns := res.Container.NetworkSettings; ns != nil {
		if ep := ns.Networks[internalNetName(id)]; ep != nil {
			proxyIP = ep.IPAddress
		}
	}
	if !proxyIP.IsValid() {
		return fail(fmt.Errorf("docker: the macOS prep proxy has no IP on %q", internalNetName(id)))
	}
	rollback = append(rollback, func() {
		_, _ = d.cli.ContainerRemove(context.Background(), vmnetName(id), client.ContainerRemoveOptions{Force: true})
	})
	attach, err := d.startMacRelay(ctx, id, labels, proxyIP.String())
	if err != nil {
		return fail(err)
	}
	return attach, undo, nil
}

// startPrepProxy creates and starts the prep proxy on id's internal network, joined to the network a
// run's proxy reaches out through, with cfgJSON on its stdin (WARDYN_PROXY_CONFIG_STDIN=1): startProxyOn's
// container and hardening, without a run's control-plane hosts, decision sink or local models, none of
// which a prep has. It answers the container's id once created, with any error after that.
func (d *Driver) startPrepProxy(ctx context.Context, id uuid.UUID, labels map[string]string, cfgJSON []byte) (string, error) {
	cfg := &container.Config{Image: d.proxyImage(), Hostname: "wardyn-proxy", Labels: labels,
		Env: []string{proxyConfigStdinEnv + "=1"}, OpenStdin: true, StdinOnce: true}
	if d.cfg.ProxyBinaryHostPath != "" {
		cfg.Entrypoint = []string{"/usr/local/bin/wardyn-proxy"}
	}
	if len(d.cfg.ProxyCmd) > 0 {
		cfg.Cmd = d.cfg.ProxyCmd
	}
	host := &container.HostConfig{
		NetworkMode:   container.NetworkMode(internalNetName(id)),
		CapDrop:       []string{"ALL"},
		SecurityOpt:   []string{"no-new-privileges"},
		Tmpfs:         map[string]string{"/tmp": "rw,nosuid,nodev,noexec,size=64m"},
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
		Resources:     proxyResources(false),
	}
	if d.cfg.ProxyBinaryHostPath != "" {
		host.Binds = []string{d.cfg.ProxyBinaryHostPath + ":/usr/local/bin/wardyn-proxy:ro"}
	}
	resp, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{Config: cfg, HostConfig: host, Name: proxyContainerName(id)})
	if err != nil {
		return "", fmt.Errorf("docker: create the macOS prep proxy: %w", err)
	}
	if _, err := d.cli.NetworkConnect(ctx, d.cfg.InternalNetwork, client.NetworkConnectOptions{
		Container: resp.ID, EndpointConfig: &network.EndpointSettings{},
	}); err != nil {
		return resp.ID, fmt.Errorf("docker: connect the macOS prep proxy to %s: %w", d.cfg.InternalNetwork, err)
	}
	// Nothing here reads the prep proxy's stdout, so the attach is stdin only and the start is watched
	// for an early exit, as startProxyOn does for a proxy image without the ready marker.
	att, err := d.startWithConfig(ctx, id, resp.ID, cfgJSON, false)
	if err != nil {
		return resp.ID, err
	}
	defer att.Close()
	return resp.ID, d.watchProxyExit(ctx, resp.ID)
}

// SweepMacPrep removes what a crashed runnerd left of its jobs' stage-2 networks: this runner's and this
// deployment's macos-prep containers, then their networks, except those of a job whose ledger entry is in
// keep. runnerd runs it once at start, before it hands any job a network. It answers how many it removed.
func (d *Driver) SweepMacPrep(ctx context.Context, keep []string) (int, error) {
	kept := make(map[string]bool, len(keep))
	for _, e := range keep {
		kept[macPrepID(e).String()] = true
	}
	stale := func(l map[string]string) bool {
		return l[labelComponent] == componentMacPrep && l[labelManaged] == "true" && l[runner.LabelRunnerID] == d.cfg.RunnerID &&
			!d.otherDeployment(l) && !kept[l[labelRun]]
	}
	filters := client.Filters{}.Add("label", labelComponent+"="+componentMacPrep)
	var errs []error
	swept := 0
	if lister, ok := d.cli.(containerListerAPI); ok {
		res, err := lister.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
		if err != nil {
			return 0, fmt.Errorf("docker: list leftover macOS prep containers: %w", err)
		}
		for _, c := range res.Items {
			if !stale(c.Labels) {
				continue
			}
			if _, err := d.cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true}); err != nil && !isNotFound(err) {
				errs = append(errs, fmt.Errorf("docker: remove leftover macOS prep container %s: %w", c.ID, err))
				continue
			}
			swept++
		}
	}
	res, err := d.cli.NetworkList(ctx, client.NetworkListOptions{Filters: filters})
	if err != nil {
		return swept, errors.Join(append(errs, fmt.Errorf("docker: list leftover macOS prep networks: %w", err))...)
	}
	for _, n := range res.Items {
		if !stale(n.Labels) {
			continue
		}
		if _, err := d.cli.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{}); err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("docker: remove leftover macOS prep network %s: %w", n.Name, err))
			continue
		}
		swept++
	}
	return swept, errors.Join(errs...)
}
