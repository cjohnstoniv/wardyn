// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// The agent's resolver on Docker. Each run gets one Docker named volume that
// holds a single file, resolv.conf, naming the run's proxy as the only
// nameserver, and the agent mounts that file read-only at /etc/resolv.conf.
// Docker then uses the mount as the container's resolver file instead of its
// own, so the agent's lookups are ordinary packets to the proxy's DNS listener
// (proxy.DNSListenEnv) on the run network, on every runtime that mounts a file
// there. The file is written through a never-started seed container, as a
// drive is seeded (seedDriveVolume), so every step is a daemon API call and no
// host directory is involved.
const (
	dnsVolumePrefix = "wardyn-dns-"
	dnsSeedPrefix   = "wardyn-dns-seed-"
	// dnsSeedTarget is where the seed container mounts the volume: a path of
	// Wardyn's own, so the copy lands in the volume whatever the image holds.
	dnsSeedTarget = "/wardyn-dns-seed"
	// resolvConfFile is the volume's one file, and the subpath the agent mounts.
	resolvConfFile = "resolv.conf"
	// minResolvConfSubpathEngineMajor is the first Docker Engine major release
	// whose volume mounts take a Subpath (API 1.45, Engine 26.0). A run needs
	// Engine 28 unless the operator opts out (runNetworkIsolation), so only an
	// opted-out run on an older engine is without it.
	minResolvConfSubpathEngineMajor = 26

	// componentDNSVolume and componentDNSSeed label the volume and its seed.
	componentDNSVolume = "dns-volume"
	componentDNSSeed   = "dns-seed"
)

func dnsVolumeName(runID uuid.UUID) string { return dnsVolumePrefix + runID.String() }
func dnsSeedName(runID uuid.UUID) string   { return dnsSeedPrefix + runID.String() }

// seedResolvConf creates runID's DNS volume and writes resolv.conf, naming
// proxyIP as the only nameserver, into it. The volume is the local driver with
// no driver options, as every volume this package creates is (driveVolumeDriver).
func (d *Driver) seedResolvConf(ctx context.Context, runID uuid.UUID, proxyIP string) error {
	volName, seedName := dnsVolumeName(runID), dnsSeedName(runID)
	if _, err := d.cli.VolumeCreate(ctx, client.VolumeCreateOptions{
		Name:   volName,
		Driver: driveVolumeDriver,
		Labels: d.labels(runID, componentDNSVolume, nil),
	}); err != nil {
		return fmt.Errorf("docker: create the DNS volume: %w", err)
	}
	remove := func(ctx context.Context) error {
		if _, err := d.cli.ContainerRemove(ctx, seedName, client.ContainerRemoveOptions{Force: true}); err != nil && !isNotFound(err) {
			return fmt.Errorf("remove the seed: %w", err)
		}
		return nil
	}
	if err := remove(ctx); err != nil {
		return fmt.Errorf("docker: seed the DNS volume: %w", err)
	}
	err := d.fillResolvConf(ctx, runID, volName, seedName, proxyIP)
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), driveSeedRemoveTimeout)
	defer cancel()
	if rerr := remove(rctx); err == nil {
		err = rerr
	}
	if err != nil {
		return fmt.Errorf("docker: seed the DNS volume: %w", err)
	}
	return nil
}

// fillResolvConf creates the seed with the volume mounted (NoCopy, so nothing
// from the image is copied in) and copies resolv.conf into it. The seed uses
// the proxy's image, which every run has pulled by now, never starts, and has
// no network.
func (d *Driver) fillResolvConf(ctx context.Context, runID uuid.UUID, volName, seedName, proxyIP string) error {
	if _, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: seedName,
		Config: &container.Config{Image: d.proxyImage(), Cmd: []string{"true"}, NetworkDisabled: true,
			Labels: d.labels(runID, componentDNSSeed, nil)},
		HostConfig: &container.HostConfig{
			NetworkMode: "none",
			Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: volName, Target: dnsSeedTarget,
				VolumeOptions: &mount.VolumeOptions{NoCopy: true}}},
		},
	}); err != nil {
		return fmt.Errorf("create the seed: %w", err)
	}
	body := []byte("nameserver " + proxyIP + "\n")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: dnsSeedTarget[1:] + "/" + resolvConfFile, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
		Uid: 0, Gid: 0, ModTime: time.Now(),
	}); err != nil {
		return err
	}
	if _, err := tw.Write(body); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	// CopyUIDGID false: the header's owner (root) is applied as written.
	if _, err := d.cli.CopyToContainer(ctx, seedName, client.CopyToContainerOptions{DestinationPath: "/", Content: &buf}); err != nil {
		return fmt.Errorf("write resolv.conf: %w", err)
	}
	return nil
}

// resolvConfMount is the agent's mount of runID's DNS volume: the volume's one
// file, read-only, over /etc/resolv.conf. NoCopy keeps Docker from copying the
// image's file into the volume.
func resolvConfMount(runID uuid.UUID) mount.Mount {
	return mount.Mount{
		Type: mount.TypeVolume, Source: dnsVolumeName(runID), Target: "/etc/resolv.conf", ReadOnly: true,
		VolumeOptions: &mount.VolumeOptions{NoCopy: true, Subpath: resolvConfFile},
	}
}

// mountProxyResolver seeds runID's DNS volume and adds its mount to host, and
// returns the step that undoes it for a failed create. An engine older than
// minResolvConfSubpathEngineMajor (reachable only under
// WARDYN_ALLOW_HOST_REACHABLE_RUN_NETWORK) takes no mount and a warning: the
// agent keeps Docker's resolver, and its lookups write no row. The undo is
// returned with the error, since a seed that failed midway may have left the
// volume.
func (d *Driver) mountProxyResolver(ctx context.Context, runID uuid.UUID, proxyIP, engineVersion string, host *container.HostConfig) (func(), error) {
	if engineMajor(engineVersion) < minResolvConfSubpathEngineMajor {
		slog.Warn("docker: this Docker Engine cannot mount a single file from a volume, so the run's lookups are not recorded as DNS rows",
			"engine", engineVersion, "needs", minResolvConfSubpathEngineMajor)
		return nil, nil
	}
	undo := func() { d.removeDNSVolume(context.Background(), runID) }
	if err := d.seedResolvConf(ctx, runID, proxyIP); err != nil {
		return undo, err
	}
	host.Mounts = append(host.Mounts, resolvConfMount(runID))
	return undo, nil
}

// removeDNSVolume removes runID's DNS seed, if a crash left one, and then its
// DNS volume, by the exact name and never forced. The agent that mounted it
// must be gone first. A volume already gone is success; the error is the first
// other one.
func (d *Driver) removeDNSVolume(ctx context.Context, runID uuid.UUID) error {
	if _, err := d.cli.ContainerRemove(ctx, dnsSeedName(runID), client.ContainerRemoveOptions{Force: true}); err != nil && !isNotFound(err) {
		return fmt.Errorf("docker: remove the DNS seed: %w", err)
	}
	if _, err := d.cli.VolumeRemove(ctx, dnsVolumeName(runID), client.VolumeRemoveOptions{}); err != nil && !isNotFound(err) {
		return fmt.Errorf("docker: remove the DNS volume: %w", err)
	}
	return nil
}
