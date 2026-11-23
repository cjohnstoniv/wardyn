// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// createImported is CreateSandbox for a moved run, without the proxy and without the start: the per-run
// network, then the agent container, created as a create makes it. The agent pins "wardyn-proxy" to an
// address the network holds for the proxy, so the revive that starts the run starts its proxy there
// (ReplaceProxy reads the pin). It answers the sandbox, the container's id, and the undo that removes what
// it made; on an error it has already undone it.
func (d *Driver) createImported(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, string, func(error) error, error) {
	infoRes, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return runner.Sandbox{}, "", nil, fmt.Errorf("docker: info: %w", err)
	}
	info := infoRes.Info
	runtimeName, _, err := resolveRuntime(spec.ConfinementClass, info, d.cfg.ConfinementRuntimes)
	if err != nil {
		return runner.Sandbox{}, "", nil, err
	}
	// An exec-less runtime creates its agent only at its first exec, so there is no container to write into.
	if !runtimeSupportsExec(runtimeName) {
		return runner.Sandbox{}, "", nil, fmt.Errorf("docker: class %s resolves to %s on this host, which creates no container before the run starts, so a moved run cannot land on it", spec.ConfinementClass, runtimeName)
	}
	enforced := spec.ConfinementClass
	if enforced == "" {
		enforced = types.CC1
	}
	if err := d.preflightSpec(spec); err != nil {
		return runner.Sandbox{}, "", nil, err
	}
	if err := d.ensureImage(ctx, spec.Image, runtimeName, func() { spec.NotifyWaiting("image: Pulling: " + spec.Image) }); err != nil {
		return runner.Sandbox{}, "", nil, err
	}
	intNetID, err := d.createRunNetwork(ctx, spec.RunID, d.labels(spec.RunID, componentNetwork, spec.Labels))
	if err != nil {
		return runner.Sandbox{}, "", nil, err
	}
	// The rollback's removals are bounded apart from ctx, so a cancelled import does not strand them.
	rb := &createRollback{undoNet: func() {
		bg, cancel := context.WithTimeout(context.Background(), rollbackBudget)
		defer cancel()
		_, _ = d.cli.NetworkRemove(bg, intNetID, client.NetworkRemoveOptions{})
	}}
	fail := func(err error) (runner.Sandbox, string, func(error) error, error) {
		return runner.Sandbox{}, "", nil, rb.run(err)
	}
	proxyIP, err := d.importProxyAddr(ctx, intNetID)
	if err != nil {
		return fail(err)
	}

	idleCmd := []string{"sh", "-c", runner.AgentIdleScript}
	if spec.Interactive {
		idleCmd = []string{"agent-run", "--idle"}
	}
	spec.Env = d.withRunnerTrustedCA(spec.Env)
	env := append(envSlice(spec.Env), envSlice(spec.SecretEnv)...)
	record := spec.RecordWithin(d.cfg.Record)
	if record {
		env = append(env, "WARDYN_RECORDING"+"=1")
	}
	agentLabels := withRecordLabel(d.labels(spec.RunID, componentAgent, spec.Labels), record)
	if spec.Resources.DiskMiB > 0 && spec.Resources.DiskMiBSoft {
		agentLabels[labelDiskSoft] = "true"
	}
	agentCfg := &container.Config{Image: spec.Image, Hostname: "agent", Env: env, Labels: agentLabels, Tty: true, Cmd: idleCmd}
	agentHost := hardenedHostConfig(internalNetName(spec.RunID), runtimeName, spec.Resources, info)
	if spec.DesktopSignins {
		agentHost.Tmpfs[runner.SigninStorePath] = runner.SigninStoreTmpfs
	}
	for _, name := range runner.ProxyHostnames(spec.Env) {
		agentHost.ExtraHosts = append(agentHost.ExtraHosts, name+":"+proxyIP.String())
	}
	rb.add(func() { d.dropDrivePin(spec.RunID) })
	mounts, err := d.agentMounts(ctx, spec, runtimeSupportsRecursiveReadOnly(info, runtimeName))
	if err != nil {
		return fail(err)
	}
	agentHost.Mounts = append(agentHost.Mounts, mounts...)
	if d.cfg.TrustedCAFile != "" {
		agentHost.Mounts = append(agentHost.Mounts, mount.Mount{Type: mount.TypeBind, Source: d.cfg.TrustedCAFile, Target: runner.TrustedCAMountPath, ReadOnly: true})
	}
	undoDNS, err := d.mountProxyResolver(ctx, spec.RunID, proxyIP.String(), info.ServerVersion, agentHost)
	rb.addResolver(undoDNS)
	if err != nil {
		return fail(err)
	}
	if err := d.checkImageVolumesUnderCap(ctx, spec.Image, spec.Resources, agentHost, info); err != nil {
		return fail(err)
	}
	name := agentContainerName(spec.RunID)
	agentResp, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     agentCfg,
		HostConfig: agentHost,
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			internalNetName(spec.RunID): {NetworkID: intNetID, Aliases: []string{"agent"}},
		}},
		Name: name,
	})
	if err != nil {
		return fail(fmt.Errorf("docker: create agent: %w", err))
	}
	rb.add(func() {
		bg, cancel := context.WithTimeout(context.Background(), rollbackBudget)
		defer cancel()
		_, _ = d.cli.ContainerRemove(bg, agentResp.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	})
	if capErr := verifyCapsEnforced(agentResp.Warnings); capErr != nil {
		if !d.cfg.AllowUnenforceableCaps {
			return fail(capErr)
		}
		slog.Warn("wardynd: the daemon discarded a resource limit — proceeding because WARDYN_ALLOW_UNENFORCEABLE_CAPS=1; the sandbox may run without CPU/memory/pids limits",
			slog.String("detail", capErr.Error()))
	}
	if err := d.deliverManagedFiles(ctx, agentResp.ID, spec.ManagedFiles); err != nil {
		return fail(err)
	}
	d.keepExecOutput(name, spec.ExecOutput)
	d.notVMs.Store(name, struct{}{})
	sb := runner.Sandbox{Ref: name, Driver: driverName, EnforcedClass: enforced, ProxyAddr: proxyIP.String()}
	return sb, agentResp.ID, rb.run, nil
}

// rollbackBudget bounds each removal of a failed import's rollback.
const rollbackBudget = 30 * time.Second

// importProxyAddr is the address a moved run's proxy takes on its network: the subnet's last host, which
// the daemon, handing out addresses from the bottom, gives no other endpoint first.
func (d *Driver) importProxyAddr(ctx context.Context, netID string) (netip.Addr, error) {
	insp, err := d.cli.NetworkInspect(ctx, netID, client.NetworkInspectOptions{})
	if err != nil {
		return netip.Addr{}, fmt.Errorf("docker: inspect the run network: %w", err)
	}
	for _, c := range insp.Network.IPAM.Config {
		if c.Subnet.Addr().Is4() {
			return lastHost(c.Subnet)
		}
	}
	return netip.Addr{}, fmt.Errorf("docker: the run network %s has no IPv4 subnet", netID)
}

// lastHost is an IPv4 subnet's highest address below its broadcast address.
func lastHost(p netip.Prefix) (netip.Addr, error) {
	if !p.Addr().Is4() || p.Bits() > maxRunSubnetBits {
		return netip.Addr{}, fmt.Errorf("docker: run subnet %s is too small for a moved run's proxy", p)
	}
	a := p.Masked().Addr().As4()
	n := binary.BigEndian.Uint32(a[:]) | (1<<(32-p.Bits()) - 1)
	binary.BigEndian.PutUint32(a[:], n-1)
	return netip.AddrFrom4(a), nil
}
