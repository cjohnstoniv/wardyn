// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/google/uuid"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run's hold on its managed drive volumes, from the moment they are judged to its teardown.
//
// SECURITY: the agent mounts a volume by NAME, at its create, which for an exec-less runtime is
// the first Exec, long after the volume was judged. Nothing held the volume in between, so a
// reclaim could remove it, and another person's first run could make one under the same name
// (two drives that fold to one name, or a home_override moved to a second person), which the
// agent would then mount. The hold is one container per run, created and never started, with no
// network, that mounts every such volume (NoCopy, so it never writes into one): while it exists
// the daemon refuses to remove any of them, and ReclaimDrive answers "in use".
const (
	drivePinPrefix    = "wardyn-drive-pin-"
	componentDrivePin = "drive-pin"
	drivePinTarget    = "/wardyn-drive-pin"
)

func drivePinName(runID uuid.UUID) string { return drivePinPrefix + runID.String() }

// errDriveVolumeReplaced says a drive volume answering to its name once held is not the one the run
// judged: its labels changed, or it is gone. The daemon makes an absent volume for a mount, with the
// mount's labels (driveVolumeLabels), so a volume removed between its check and the hold comes back
// as this drive's: empty, and refused only when the judged one carried other labels.
var errDriveVolumeReplaced = errors.New("your drive's volume was deleted while your run was starting; start the run again")

// errRunContainerNotOurs says a container answering to one of a run's own names (its drive seed or
// its drive hold) is not Wardyn's for that run. As with the DNS seed, a name proves nothing about
// who made the object.
var errRunContainerNotOurs = errors.New("a container has a name this run uses, but Wardyn did not create it for this run")

// judgedVolume is one drive's volume as ensureDriveVolume judged it.
type judgedVolume struct {
	drive *types.DriveMount
	vol   volume.Volume
}

// pinDriveVolumes creates runID's hold over vols and judges each volume again under it: the labels
// must be those ensureDriveVolume judged. On any refusal the hold it made is removed. A container
// under the hold's name that is not this run's refuses the run and is left in place.
func (d *Driver) pinDriveVolumes(ctx context.Context, runID uuid.UUID, vols []judgedVolume) error {
	if len(vols) == 0 {
		return nil
	}
	name := drivePinName(runID)
	if err := d.removeRunContainer(ctx, name, runID, componentDrivePin); err != nil {
		return fmt.Errorf("docker: hold the run's drive volumes: %w", err)
	}
	mounts := make([]mount.Mount, len(vols))
	for i, v := range vols {
		mounts[i] = mount.Mount{Type: mount.TypeVolume, Source: v.drive.ObjectName, Target: fmt.Sprintf("%s/%d", drivePinTarget, i),
			ReadOnly: true, VolumeOptions: &mount.VolumeOptions{NoCopy: true, Labels: driveVolumeLabels(v.drive)}}
	}
	extra := map[string]string{}
	if d.cfg.RunnerID != "" {
		extra[runner.LabelRunnerID] = d.cfg.RunnerID
	}
	// The proxy's image, which every run has pulled by now, as the DNS seed uses.
	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{Image: d.proxyImage(), Cmd: []string{"true"}, NetworkDisabled: true,
			Labels: d.labels(runID, componentDrivePin, extra)},
		HostConfig: &container.HostConfig{NetworkMode: "none", Mounts: mounts},
	})
	if err != nil {
		return fmt.Errorf("docker: hold the run's drive volumes: %w", err)
	}
	for _, v := range vols {
		res, ierr := d.cli.VolumeInspect(ctx, v.drive.ObjectName, client.VolumeInspectOptions{})
		switch {
		case ierr != nil && !isNotFound(ierr):
			err = fmt.Errorf("docker: inspect user drive volume %q once held: %w", v.drive.ObjectName, ierr)
		case ierr != nil || !maps.Equal(res.Volume.Labels, v.vol.Labels):
			err = fmt.Errorf("docker: user drive volume %q changed while this run was starting: %w", v.drive.ObjectName, errDriveVolumeReplaced)
		}
		if err != nil {
			break
		}
	}
	if err != nil {
		if rerr := d.removeSeedByID(context.WithoutCancel(ctx), created.ID); rerr != nil {
			err = errors.Join(err, rerr)
		}
		return err
	}
	return nil
}

// dropDrivePin removes runID's hold for a create that failed; one it cannot remove stays for the orphan sweep,
// which keys a teardown on it.
func (d *Driver) dropDrivePin(runID uuid.UUID) {
	_ = d.removeRunContainer(context.Background(), drivePinName(runID), runID, componentDrivePin)
}

// removeRunContainer removes the container under name if it is runID's component as this driver
// creates it: inspected by name, judged by runContainerLabelsOwned, and removed by the id that
// inspect returned, so a name taken between the two cannot redirect the removal. One that is not
// Wardyn's is left in place and returned as errRunContainerNotOurs; none there is success.
func (d *Driver) removeRunContainer(ctx context.Context, name string, runID uuid.UUID, component string) error {
	res, err := d.cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("docker: inspect %s: %w", name, err)
	}
	var l map[string]string
	if res.Container.Config != nil {
		l = res.Container.Config.Labels
	}
	if !d.runContainerLabelsOwned(l, runID, component) {
		return fmt.Errorf("%w: %q", errRunContainerNotOurs, name)
	}
	return d.removeSeedByID(ctx, res.Container.ID)
}
