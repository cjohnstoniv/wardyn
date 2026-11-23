// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

// SiteFeatureSwitches is SiteConfig.FeatureSwitches, the site config's features block: the features new in 0.11 that
// are off until an admin turns them on. Each key is a POINTER for the byte-identical-GET reason the provider blocks
// give, and each key's Enabled is a pointer that reads off when absent, so an explicit false survives omitempty.
//
// Read a switch only through internal/api's siteFeatureOn, which also asks whether this release can turn the feature
// on: a stored true it cannot act on (a database edited by hand, a downgrade) reads off. Each feature owns the fields
// of its own key's type.
type SiteFeatureSwitches struct {
	OrgSites              *OrgSitesFeature              `json:"org_sites,omitempty"`
	WindowsVM             *WindowsVMFeature             `json:"windows_vm,omitempty"`
	RunMove               *RunMoveFeature               `json:"run_move,omitempty"`
	MachineSnapshots      *MachineSnapshotsFeature      `json:"machine_snapshots,omitempty"`
	LocalWindow           *LocalWindowFeature           `json:"local_window,omitempty"`
	PreviewURLs           *PreviewURLsFeature           `json:"preview_urls,omitempty"`
	ComponentRequestRules *ComponentRequestRulesFeature `json:"component_request_rules,omitempty"`
	OrgRunnerDrives       *OrgRunnerDrivesFeature       `json:"org_runner_drives,omitempty"`
}

// OrgSitesFeature is features.org_sites: sites that group runner pools and drives.
type OrgSitesFeature struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// WindowsVMFeature is features.windows_vm: Windows VM sandboxes from the organisation's own images.
type WindowsVMFeature struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// RunMoveFeature is features.run_move: moving a paused run to another machine.
type RunMoveFeature struct {
	Enabled *bool `json:"enabled,omitempty"`
	// ToPersonalRunners also lets a run move onto a person's own runner. Off when absent.
	ToPersonalRunners *bool `json:"to_personal_runners,omitempty"`
	// MaxCarriedGiB caps what one move carries. 0 or absent reads the move's default.
	MaxCarriedGiB int `json:"max_carried_gib,omitempty"`
}

// MachineSnapshotsFeature is features.machine_snapshots: snapshots, forks and warm pools of Linux VM sandboxes, and
// sleep on disconnect.
type MachineSnapshotsFeature struct {
	Enabled *bool `json:"enabled,omitempty"`
	// WarmPool is how many booted, unused VMs each runner keeps ready, per image.
	WarmPool []WarmPoolTarget `json:"warm_pool,omitempty"`
	// SleepAfterDisconnectSec sleeps a VM this long after its last viewer leaves. 0 or absent: never.
	SleepAfterDisconnectSec int `json:"sleep_after_disconnect_sec,omitempty"`
	// MaxPerPerson caps the snapshots one person keeps. 0 or absent reads the default.
	MaxPerPerson int `json:"max_per_person,omitempty"`
	// IdleDays deletes a snapshot nobody used for this many days. 0 or absent reads the default.
	IdleDays int `json:"idle_days,omitempty"`
}

// WarmPoolTarget is one features.machine_snapshots.warm_pool entry.
type WarmPoolTarget struct {
	Image string `json:"image"`
	Count int    `json:"count"`
}

// LocalWindowFeature is features.local_window: opening a Linux VM run in a window on the person's own computer.
type LocalWindowFeature struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// PreviewURLsFeature is features.preview_urls: preview URLs for a run's ports.
type PreviewURLsFeature struct {
	Enabled *bool `json:"enabled,omitempty"`
	// MaxVisibility is the widest a preview may be: private, org or public. Absent reads private.
	MaxVisibility string `json:"max_visibility,omitempty"`
}

// ComponentRequestRulesFeature is features.component_request_rules: request rules on custom components.
type ComponentRequestRulesFeature struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// OrgRunnerDrivesFeature is features.org_runner_drives: drives kept on an organisation Kubernetes runner.
type OrgRunnerDrivesFeature struct {
	Enabled *bool `json:"enabled,omitempty"`
}
