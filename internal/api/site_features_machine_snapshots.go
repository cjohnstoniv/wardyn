// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// features.machine_snapshots: snapshots, forks and warm pools of Linux VM sandboxes. This file holds the key's
// validate and its readers. It registers nothing: the key registers once snapshots work end to end, and until then
// it reads off and cannot be turned on or set.

// The bounds of the key's settings. A zero max_per_person or idle_days reads the default.
const (
	machineSnapshotsWarmCountMax        = 4
	machineSnapshotsMaxPerPersonMax     = 8
	machineSnapshotsMaxPerPersonDefault = 2
	machineSnapshotsIdleDaysMax         = 90
	machineSnapshotsIdleDaysDefault     = 14
	machineSnapshotsSleepMinSec         = 60
	machineSnapshotsSleepMaxSec         = 86400
)

// validateMachineSnapshotsFeature checks a write that changes the key, whether the switch is on or off. A refusal
// is a plain error, so it answers site_config_invalid. Sleeping a sandbox nobody watches is refused in this
// release whatever its value, since nothing here can sleep one yet.
func validateMachineSnapshotsFeature(_ context.Context, _ *Server, next, _ *types.SiteFeatureSwitches) error {
	if next == nil || next.MachineSnapshots == nil {
		return nil
	}
	m := next.MachineSnapshots
	if n := m.MaxPerPerson; n < 0 || n > machineSnapshotsMaxPerPersonMax {
		//lint:ignore ST1005 the sentence the admin reads, as the site config refusal shows it
		return fmt.Errorf("A person can keep 1 to %d snapshots. Set features.machine_snapshots.max_per_person to a number from 1 to %d, or leave it out for %d.",
			machineSnapshotsMaxPerPersonMax, machineSnapshotsMaxPerPersonMax, machineSnapshotsMaxPerPersonDefault)
	}
	if d := m.IdleDays; d < 0 || d > machineSnapshotsIdleDaysMax {
		//lint:ignore ST1005 the sentence the admin reads, as the site config refusal shows it
		return fmt.Errorf("A snapshot can sit unused for 1 to %d days before it is deleted. Set features.machine_snapshots.idle_days to a number from 1 to %d, or leave it out for %d.",
			machineSnapshotsIdleDaysMax, machineSnapshotsIdleDaysMax, machineSnapshotsIdleDaysDefault)
	}
	if sec := m.SleepAfterDisconnectSec; sec < 0 || (sec > 0 && (sec < machineSnapshotsSleepMinSec || sec > machineSnapshotsSleepMaxSec)) {
		//lint:ignore ST1005 the sentence the admin reads, as the site config refusal shows it
		return fmt.Errorf("Set features.machine_snapshots.sleep_after_disconnect_sec to 0, or to a number of seconds from %d to %d.",
			machineSnapshotsSleepMinSec, machineSnapshotsSleepMaxSec)
	}
	if m.SleepAfterDisconnectSec != 0 {
		//lint:ignore ST1005 the sentence the admin reads, as the site config refusal shows it
		return errors.New("This release cannot sleep a Linux VM sandbox yet. Set features.machine_snapshots.sleep_after_disconnect_sec to 0, or leave it out.")
	}
	seen := map[string]bool{}
	for _, w := range m.WarmPool {
		image := strings.TrimSpace(w.Image)
		switch {
		case image == "":
			//lint:ignore ST1005 the sentence the admin reads, as the site config refusal shows it
			return errors.New("Every features.machine_snapshots.warm_pool entry names an image.")
		case seen[image]:
			//lint:ignore ST1005 the sentence the admin reads, as the site config refusal shows it
			return fmt.Errorf("Each image is listed once in features.machine_snapshots.warm_pool, and %q is listed twice.", image)
		case w.Count < 0 || w.Count > machineSnapshotsWarmCountMax:
			//lint:ignore ST1005 the sentence the admin reads, as the site config refusal shows it
			return fmt.Errorf("A runner keeps 0 to %d warm machines of one image. Set the count of %q to a number from 0 to %d.",
				machineSnapshotsWarmCountMax, image, machineSnapshotsWarmCountMax)
		}
		seen[image] = true
	}
	return nil
}

// machineSnapshotsOn is the key's switch: on only once this release can turn it on and the stored enabled is true.
func machineSnapshotsOn(sc types.SiteConfig) bool { return siteFeatureOn(sc, featureMachineSnapshots) }

// machineSnapshotsSettings is the key's stored settings, read whether the switch is on or off, with the defaults
// filled in for a zero max_per_person or idle_days.
func machineSnapshotsSettings(sc types.SiteConfig) types.MachineSnapshotsFeature {
	var out types.MachineSnapshotsFeature
	if m := siteFeatureSettings(sc).MachineSnapshots; m != nil {
		out = *m
	}
	if out.MaxPerPerson == 0 {
		out.MaxPerPerson = machineSnapshotsMaxPerPersonDefault
	}
	if out.IdleDays == 0 {
		out.IdleDays = machineSnapshotsIdleDaysDefault
	}
	return out
}
