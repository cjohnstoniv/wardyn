// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// EnvRunSubnetPool and EnvRunSubnetSize are Config.RunSubnetPool's and Config.RunSubnetBits' settings.
const (
	EnvRunSubnetPool     = "WARDYN_DOCKER_RUN_SUBNET_POOL"
	EnvRunSubnetSize     = "WARDYN_DOCKER_RUN_SUBNET_SIZE"
	defaultRunSubnetBits = 28
	// maxRunSubnetBits leaves a run network room for its gateway, proxy, agent and a VM's relay.
	maxRunSubnetBits = 29
)

// RunSubnetPoolFromEnv reads EnvRunSubnetPool and EnvRunSubnetSize. Unset returns the zero prefix,
// which leaves each run network's subnet to the daemon's default address pools.
func RunSubnetPoolFromEnv(getenv func(string) string) (netip.Prefix, int, error) {
	raw, size := strings.TrimSpace(getenv(EnvRunSubnetPool)), strings.TrimSpace(getenv(EnvRunSubnetSize))
	if raw == "" {
		if size != "" {
			return netip.Prefix{}, 0, fmt.Errorf("%s is set but %s is not: set the range the run networks are carved from", EnvRunSubnetSize, EnvRunSubnetPool)
		}
		return netip.Prefix{}, 0, nil
	}
	pool, err := netip.ParsePrefix(raw)
	if err != nil || !pool.Addr().Is4() || pool != pool.Masked() {
		return netip.Prefix{}, 0, fmt.Errorf("%s=%q is not an IPv4 network range such as 10.200.0.0/16", EnvRunSubnetPool, raw)
	}
	bits := defaultRunSubnetBits
	if size != "" {
		if bits, err = strconv.Atoi(size); err != nil {
			return netip.Prefix{}, 0, fmt.Errorf("%s=%q is not a prefix length", EnvRunSubnetSize, size)
		}
	}
	if bits < pool.Bits() || bits > maxRunSubnetBits {
		return netip.Prefix{}, 0, fmt.Errorf("%s=%d must lie between the range's own length (/%d) and /%d", EnvRunSubnetSize, bits, pool.Bits(), maxRunSubnetBits)
	}
	return pool, bits, nil
}

// nextRunSubnet returns the first bits-long subnet of pool that overlaps none of used.
func nextRunSubnet(pool netip.Prefix, bits int, used []netip.Prefix) (netip.Prefix, bool) {
	a := pool.Addr().As4()
	base := uint64(a[0])<<24 | uint64(a[1])<<16 | uint64(a[2])<<8 | uint64(a[3])
	step := uint64(1) << (32 - bits)
	for i := range uint64(1) << (bits - pool.Bits()) {
		v := base + i*step
		cand := netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), bits)
		if !slices.ContainsFunc(used, cand.Overlaps) {
			return cand, true
		}
	}
	return netip.Prefix{}, false
}

// createPoolRunNetwork creates network name on the next free subnet of Config.RunSubnetPool. What is
// free is read from the daemon's networks on every create, never kept in memory, so a restart, a
// second replica and every other stack on the daemon count. The lock keeps this driver's creates
// from racing each other; a create that loses its subnet to another process anyway is retried.
func (d *Driver) createPoolRunNetwork(ctx context.Context, name string, labels map[string]string) (string, error) {
	d.subnetMu.Lock()
	defer d.subnetMu.Unlock()
	var lost []netip.Prefix
	var err error
	for range 3 {
		var subnet netip.Prefix
		if subnet, err = d.poolRunSubnet(ctx, lost); err != nil {
			return "", err
		}
		opts := client.NetworkCreateOptions{Driver: "bridge", Internal: true, Labels: labels,
			IPAM: &network.IPAM{Config: []network.IPAMConfig{{Subnet: subnet}}}}
		var res client.NetworkCreateResult
		if res, err = d.cli.NetworkCreate(ctx, name, opts); err == nil {
			return res.ID, nil
		}
		// Docker's "Pool overlaps with other one" (IPAM) and "networks have overlapping IPv4" (bridge).
		if !strings.Contains(err.Error(), "overlap") {
			break
		}
		lost = append(lost, subnet)
	}
	return "", fmt.Errorf("docker: create internal network: %w", err)
}

// poolRunSubnet picks the next subnet of the pool that no daemon network and none of lost overlaps.
func (d *Driver) poolRunSubnet(ctx context.Context, lost []netip.Prefix) (netip.Prefix, error) {
	res, err := d.cli.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("docker: list networks for the run subnet pool: %w", err)
	}
	used := slices.Clone(lost)
	for _, n := range res.Items {
		for _, c := range n.IPAM.Config {
			if c.Subnet.Addr().Is4() {
				used = append(used, c.Subnet)
			}
		}
	}
	subnet, ok := nextRunSubnet(d.cfg.RunSubnetPool, d.cfg.RunSubnetBits, used)
	if !ok {
		return netip.Prefix{}, fmt.Errorf("docker: create internal network: no free /%d subnet is left in %s=%s for this run's network (each sandbox holds one): widen the range, or end runs to free one",
			d.cfg.RunSubnetBits, EnvRunSubnetPool, d.cfg.RunSubnetPool)
	}
	return subnet, nil
}

// sweepRunNetworks removes the per-run networks, older than cutoff, of runs isOrphan says are gone,
// handing their subnets back to the pool. It catches the network a crash between network create and
// proxy start leaves with no container for SweepOrphanedSandboxes to key on.
func (d *Driver) sweepRunNetworks(ctx context.Context, cutoff time.Time, isOrphan func(runID uuid.UUID) bool) (int, error) {
	res, err := d.cli.NetworkList(ctx, client.NetworkListOptions{Filters: client.Filters{}.Add("label", labelComponent+"="+componentNetwork)})
	if err != nil {
		return 0, fmt.Errorf("docker: list networks for orphan sweep: %w", err)
	}
	var swept int
	var errs []error
	for _, n := range res.Items {
		l := n.Labels
		runID, perr := parseRunID(l[labelRun])
		if perr != nil || l[labelComponent] != componentNetwork || l[labelManaged] != "true" || l[runner.LabelRunnerID] != d.cfg.RunnerID ||
			d.otherDeployment(l) || n.Created.After(cutoff) || !isOrphan(runID) {
			continue
		}
		if _, err := d.cli.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{}); err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("docker: remove orphaned run network %s: %w", n.Name, err))
			continue
		}
		swept++
	}
	return swept, errors.Join(errs...)
}
