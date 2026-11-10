// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
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
	// minResolvConfSubpathAPIMinor is the minor of API 1.45, the first with a volume mount Subpath.
	minResolvConfSubpathAPIMinor = 45

	// componentDNSVolume and componentDNSSeed label the volume and its seed.
	componentDNSVolume = "dns-volume"
	componentDNSSeed   = "dns-seed"
)

func dnsVolumeName(runID uuid.UUID) string { return dnsVolumePrefix + runID.String() }
func dnsSeedName(runID uuid.UUID) string   { return dnsSeedPrefix + runID.String() }

// errDNSVolumeNotOurs says a volume answering to a run's DNS volume name is not
// that run's DNS volume. A name proves nothing about who made the object, so
// the driver neither seeds, mounts nor removes such a volume.
var errDNSVolumeNotOurs = errors.New("docker: a volume answers to this run's DNS volume name but is not Wardyn's DNS volume for it")

// dnsLabels is the label set on a run's DNS volume and its seed: the run's
// labels for the component, plus this runner's id when it has one.
func (d *Driver) dnsLabels(runID uuid.UUID, component string) map[string]string {
	var extra map[string]string
	if d.cfg.RunnerID != "" {
		extra = map[string]string{runner.LabelRunnerID: d.cfg.RunnerID}
	}
	return d.labels(runID, component, extra)
}

// dnsVolumeOwned says whether v is runID's DNS volume as this driver creates it:
// the exact name, the local driver with no driver options, this run's, this
// runner's and this deployment's labels for the DNS component, and none of a
// drive's labels. Anything else is somebody else's storage (the same predicate
// shape as driveVolumeAdoptable).
func (d *Driver) dnsVolumeOwned(v volume.Volume, runID uuid.UUID) error {
	name := dnsVolumeName(runID)
	l := v.Labels
	deployment, hasDeployment := l[labelDeployment]
	switch {
	case v.Name != name:
		return fmt.Errorf("%w: it is named %q", errDNSVolumeNotOurs, v.Name)
	case v.Driver != driveVolumeDriver || len(v.Options) != 0:
		return fmt.Errorf("%w: %q has driver %q and %d driver option(s)", errDNSVolumeNotOurs, name, v.Driver, len(v.Options))
	case l[labelManaged] != "true" || l[labelComponent] != componentDNSVolume || l[labelRun] != runID.String():
		return fmt.Errorf("%w: %q is not labelled as this run's DNS volume", errDNSVolumeNotOurs, name)
	case !hasDeployment || deployment != d.cfg.InternalNetwork || l[runner.LabelRunnerID] != d.cfg.RunnerID:
		return fmt.Errorf("%w: %q belongs to another runner or deployment", errDNSVolumeNotOurs, name)
	}
	for _, k := range []string{labelDrive, labelDriveHome, labelDriveSubject} {
		if _, has := l[k]; has {
			return fmt.Errorf("%w: %q carries the drive label %s", errDNSVolumeNotOurs, name, k)
		}
	}
	return nil
}

// ensureDNSVolume makes sure runID's DNS volume exists and is Wardyn's:
// inspect first, then create, then verify what the create returned. A create
// against a name the daemon already holds succeeds and returns the EXISTING
// volume with its own labels and driver, so a mismatch found at either step
// refuses the run before anything is written to it. The volume is the local
// driver with no driver options, as every volume this package creates is
// (driveVolumeDriver).
func (d *Driver) ensureDNSVolume(ctx context.Context, runID uuid.UUID) error {
	name := dnsVolumeName(runID)
	if res, err := d.cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{}); err == nil {
		return d.dnsVolumeOwned(res.Volume, runID)
	} else if !isNotFound(err) {
		return fmt.Errorf("docker: inspect the DNS volume: %w", err)
	}
	created, err := d.cli.VolumeCreate(ctx, client.VolumeCreateOptions{
		Name:   name,
		Driver: driveVolumeDriver,
		Labels: d.dnsLabels(runID, componentDNSVolume),
	})
	if err != nil {
		return fmt.Errorf("docker: create the DNS volume: %w", err)
	}
	return d.dnsVolumeOwned(created.Volume, runID)
}

// errDNSSeedNotOurs says a container answering to a run's DNS seed name is not that run's DNS seed. As with
// the volume, a name proves nothing about who made the object.
var errDNSSeedNotOurs = errors.New("docker: a container answers to this run's DNS seed name but is not Wardyn's DNS seed for it")

// dnsSeedOwned says whether a container is runID's DNS seed as this driver creates it: the DNS seed component,
// managed, this run's, this deployment's and this runner's labels.
func (d *Driver) dnsSeedOwned(insp container.InspectResponse, runID uuid.UUID) error {
	var l map[string]string
	if insp.Config != nil {
		l = insp.Config.Labels
	}
	deployment, hasDeployment := l[labelDeployment]
	if l[labelManaged] != "true" || l[labelComponent] != componentDNSSeed || l[labelRun] != runID.String() ||
		!hasDeployment || deployment != d.cfg.InternalNetwork || l[runner.LabelRunnerID] != d.cfg.RunnerID {
		return fmt.Errorf("%w: %q", errDNSSeedNotOurs, dnsSeedName(runID))
	}
	return nil
}

// removeDNSSeed removes runID's DNS seed if one is there: inspected by name, judged by dnsSeedOwned, and removed
// by the id that inspect returned, so a name taken between the two cannot redirect the removal. A seed that is
// not Wardyn's is left in place and returned as errDNSSeedNotOurs; none there is success.
func (d *Driver) removeDNSSeed(ctx context.Context, runID uuid.UUID) error {
	res, err := d.cli.ContainerInspect(ctx, dnsSeedName(runID), client.ContainerInspectOptions{})
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("docker: inspect the DNS seed: %w", err)
	}
	if err := d.dnsSeedOwned(res.Container, runID); err != nil {
		return err
	}
	return d.removeSeedByID(ctx, res.Container.ID)
}

// removeSeedByID force-removes the seed container with this id, the one this driver created or inspected, and
// any anonymous volume the image declared for it. The run's named DNS volume stays.
func (d *Driver) removeSeedByID(ctx context.Context, id string) error {
	if _, err := d.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !isNotFound(err) {
		return fmt.Errorf("docker: remove the DNS seed: %w", err)
	}
	return nil
}

// seedResolvConf makes runID's DNS volume and writes resolv.conf, naming
// proxyIP as the only nameserver, into it. A seed or volume under the run's
// names that is not Wardyn's refuses the run. The seed this call creates is
// kept by id for the copy and its removal.
func (d *Driver) seedResolvConf(ctx context.Context, runID uuid.UUID, proxyIP string) error {
	if err := d.ensureDNSVolume(ctx, runID); err != nil {
		return err
	}
	if err := d.removeDNSSeed(ctx, runID); err != nil {
		return fmt.Errorf("docker: seed the DNS volume: %w", err)
	}
	seedID, err := d.fillResolvConf(ctx, runID, dnsVolumeName(runID), proxyIP)
	if seedID != "" {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), driveSeedRemoveTimeout)
		defer cancel()
		if rerr := d.removeSeedByID(rctx, seedID); err == nil {
			err = rerr
		}
	}
	if err != nil {
		return fmt.Errorf("docker: seed the DNS volume: %w", err)
	}
	return nil
}

// fillResolvConf creates the seed with the volume mounted (NoCopy, so nothing
// from the image is copied in) and copies resolv.conf into it. The seed uses
// the proxy's image, which every run has pulled by now, never starts, and has
// no network. It returns the seed's id as soon as the create has made one, so
// the caller removes that container whatever the copy does.
func (d *Driver) fillResolvConf(ctx context.Context, runID uuid.UUID, volName, proxyIP string) (string, error) {
	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: dnsSeedName(runID),
		Config: &container.Config{Image: d.proxyImage(), Cmd: []string{"true"}, NetworkDisabled: true,
			Labels: d.dnsLabels(runID, componentDNSSeed)},
		HostConfig: &container.HostConfig{
			NetworkMode: "none",
			Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: volName, Target: dnsSeedTarget,
				VolumeOptions: &mount.VolumeOptions{NoCopy: true}}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("create the seed: %w", err)
	}
	body := []byte("nameserver " + proxyIP + "\n")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: dnsSeedTarget[1:] + "/" + resolvConfFile, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
		Uid: 0, Gid: 0, ModTime: time.Now(),
	}); err != nil {
		return created.ID, err
	}
	if _, err := tw.Write(body); err != nil {
		return created.ID, err
	}
	if err := tw.Close(); err != nil {
		return created.ID, err
	}
	// CopyUIDGID false: the header's owner (root) is applied as written.
	if _, err := d.cli.CopyToContainer(ctx, created.ID, client.CopyToContainerOptions{DestinationPath: "/", Content: &buf}); err != nil {
		return created.ID, fmt.Errorf("write resolv.conf: %w", err)
	}
	return created.ID, nil
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

// apiAtLeast says whether the Docker API version v ("1.45", "v1.45") is major.minor or later. An empty or
// unreadable version is not.
func apiAtLeast(v string, major, minor int) bool {
	maj, mnr, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	a, aerr := strconv.Atoi(maj)
	b, berr := strconv.Atoi(mnr)
	if !ok || aerr != nil || berr != nil {
		return false
	}
	return a > major || a == major && b >= minor
}

// resolvConfMountSupported says whether this daemon and this client can mount a single file from a volume: the
// Engine is 26 or later AND the API version the client talks is 1.45 or later. The second matters on its own
// because the client takes DOCKER_API_VERSION from the environment and then does not negotiate, so a new
// Engine can be spoken to over an older API that has no volume Subpath.
func (d *Driver) resolvConfMountSupported(engineVersion string) bool {
	return engineMajor(engineVersion) >= minResolvConfSubpathEngineMajor && apiAtLeast(d.cli.ClientVersion(), 1, minResolvConfSubpathAPIMinor)
}

// mountProxyResolver seeds runID's DNS volume and adds its mount to host, and
// returns the step that undoes it for a failed create. Where
// resolvConfMountSupported is false (an Engine older than 26, reachable only
// under WARDYN_ALLOW_HOST_REACHABLE_RUN_NETWORK, or an API pinned below 1.45)
// the run takes no mount and a warning: the agent keeps Docker's resolver, and
// its lookups write no row. The undo is returned with the error, since a seed
// that failed midway may have left the volume; it removes the volume only
// after the same ownership check as any other removal, and reports a removal
// that failed.
func (d *Driver) mountProxyResolver(ctx context.Context, runID uuid.UUID, proxyIP, engineVersion string, host *container.HostConfig) (func() error, error) {
	if !d.resolvConfMountSupported(engineVersion) {
		slog.Warn("docker: this Docker Engine or API version cannot mount a single file from a volume, so the run's lookups are not recorded as DNS rows",
			"engine", engineVersion, "api", d.cli.ClientVersion(), "needs_engine", minResolvConfSubpathEngineMajor,
			"needs_api", fmt.Sprintf("1.%d", minResolvConfSubpathAPIMinor))
		return nil, nil
	}
	undo := func() error { return d.removeDNSVolume(context.Background(), runID) }
	if err := d.seedResolvConf(ctx, runID, proxyIP); err != nil {
		return undo, err
	}
	host.Mounts = append(host.Mounts, resolvConfMount(runID))
	return undo, nil
}

// removeDNSVolume removes runID's DNS seed, if a crash left one, and then its
// DNS volume, by the exact name and never forced. The agent that mounted it
// must be gone first. The volume is inspected first and removed only when
// dnsVolumeOwned says it is Wardyn's: a volume that answers to the name but is
// not (including on a run that never mounted one) is left untouched with a
// warning, and the removal succeeds. A volume already gone is success; the
// error is the first other one.
func (d *Driver) removeDNSVolume(ctx context.Context, runID uuid.UUID) error {
	if err := d.removeDNSSeed(ctx, runID); errors.Is(err, errDNSSeedNotOurs) {
		slog.Warn("docker: leaving a container that is not this run's DNS seed in place", "container", dnsSeedName(runID), "reason", err.Error())
	} else if err != nil {
		return err
	}
	name := dnsVolumeName(runID)
	res, err := d.cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("docker: inspect the DNS volume: %w", err)
	}
	if oerr := d.dnsVolumeOwned(res.Volume, runID); oerr != nil {
		slog.Warn("docker: leaving a volume that is not this run's DNS volume in place", "volume", name, "reason", oerr.Error())
		return nil
	}
	if _, err := d.cli.VolumeRemove(ctx, name, client.VolumeRemoveOptions{}); err != nil && !isNotFound(err) {
		return fmt.Errorf("docker: remove the DNS volume: %w", err)
	}
	return nil
}

// withDNSListen adds the proxy's DNS listener to its environment.
func withDNSListen(env []string) []string { return append(env, proxy.DNSListenEnv+"=:53") }

// agentMountsResolver says whether the agent's host config mounts runID's DNS volume over /etc/resolv.conf: the
// agent's resolver is then its proxy, which is what a proxy's DNS listener is for.
func agentMountsResolver(host *container.HostConfig, runID uuid.UUID) bool {
	if host == nil {
		return false
	}
	return slices.ContainsFunc(host.Mounts, func(m mount.Mount) bool {
		return m.Type == mount.TypeVolume && m.Target == "/etc/resolv.conf" && m.Source == dnsVolumeName(runID)
	})
}

// createRollback collects the teardown steps of a CreateSandbox that fails, run in reverse. A resolver volume
// that cannot be removed keeps the proxy and the network: the proxy is the anchor the orphan sweep finds the run
// by, and its teardown removes the volume before the proxy, so dropping the proxy here would leave the volume
// with nothing to find it by.
type createRollback struct {
	undoNet    func()
	steps      []func()
	resolver   func() error
	dnsCleanup error
}

// add registers a step to run on failure, after those registered later.
func (r *createRollback) add(step func()) { r.steps = append(r.steps, step) }

// addResolver registers the resolver's cleanup, which may be nil when no resolver was set up. Its error is kept.
func (r *createRollback) addResolver(undo func() error) { r.resolver = undo }

// run undoes what the create made, most recent first, and returns err with the resolver cleanup's error joined
// when that cleanup failed and the proxy was therefore kept.
func (r *createRollback) run(err error) error {
	for i := len(r.steps) - 1; i >= 0; i-- {
		r.steps[i]()
	}
	if r.resolver != nil {
		r.dnsCleanup = r.resolver()
	}
	if r.dnsCleanup != nil {
		return errors.Join(err, fmt.Errorf("the run's proxy is kept for the orphan sweep to retry: %w", r.dnsCleanup))
	}
	r.undoNet()
	return err
}
