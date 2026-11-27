// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "slices"

// LocalFolderRules are the organisation's walls for folders from a person's computer (#2990): whether one may go
// to a run that is not on that computer, whether the sandbox's changes may come back, the most one sync session
// carries each way, and ignore patterns added to the deployment's. They only narrow: a deny stays set, a cap only
// shrinks, patterns only add.
type LocalFolderRules struct {
	DenyRemote bool     `json:"deny_remote,omitempty"`
	DenyPull   bool     `json:"deny_pull,omitempty"`
	MaxPushMiB int      `json:"max_push_mib,omitempty"` // 0 is no cap
	MaxPullMiB int      `json:"max_pull_mib,omitempty"` // 0 is no cap
	Deny       []string `json:"deny,omitempty"`
}

// Clone is a deep copy, nil for nil.
func (r *LocalFolderRules) Clone() *LocalFolderRules {
	if r == nil {
		return nil
	}
	out := *r
	out.Deny = slices.Clone(r.Deny)
	return &out
}

// Meet is the stricter of r and o, nil when both are nil. A field one side omits takes the other side's value.
func (r *LocalFolderRules) Meet(o *LocalFolderRules) *LocalFolderRules {
	if r == nil {
		return o.Clone()
	}
	if o == nil {
		return r.Clone()
	}
	out := &LocalFolderRules{
		DenyRemote: r.DenyRemote || o.DenyRemote, DenyPull: r.DenyPull || o.DenyPull,
		MaxPushMiB: smallerCap(r.MaxPushMiB, o.MaxPushMiB), MaxPullMiB: smallerCap(r.MaxPullMiB, o.MaxPullMiB),
		Deny: slices.Clone(r.Deny),
	}
	for _, p := range o.Deny {
		if !slices.Contains(out.Deny, p) {
			out.Deny = append(out.Deny, p)
		}
	}
	return out
}

// WidensAsOverlay reports whether the overlay o would loosen base. An overlay's omitted field inherits the base's
// value, so the one widening it can express is a positive cap larger than the base's positive cap.
func (o *LocalFolderRules) WidensAsOverlay(base *LocalFolderRules) bool {
	if o == nil || base == nil {
		return false
	}
	return capRaises(o.MaxPushMiB, base.MaxPushMiB) || capRaises(o.MaxPullMiB, base.MaxPullMiB)
}

// AtLeastAsStrict reports whether the whole spec r narrows at least as much as the whole spec o. Here an omitted
// field is no wall, so r without a cap is looser than o with one.
func (r *LocalFolderRules) AtLeastAsStrict(o *LocalFolderRules) bool {
	if o == nil {
		return true
	}
	if r == nil {
		return !o.DenyRemote && !o.DenyPull && o.MaxPushMiB <= 0 && o.MaxPullMiB <= 0 && len(o.Deny) == 0
	}
	return (r.DenyRemote || !o.DenyRemote) && (r.DenyPull || !o.DenyPull) &&
		capAtMost(r.MaxPushMiB, o.MaxPushMiB) && capAtMost(r.MaxPullMiB, o.MaxPullMiB) &&
		!slices.ContainsFunc(o.Deny, func(p string) bool { return !slices.Contains(r.Deny, p) })
}

// LocalFolderWallsRecord is what a run's row keeps (agent_runs.local_folder_walls): the walls dispatch resolved and,
// per wall, the layer that set it. A nil Walls is no walls.
type LocalFolderWallsRecord struct {
	Walls *LocalFolderRules `json:"walls,omitempty"`
	// Layers is keyed by a wall's json name: deny_remote, deny_pull, max_push_mib, max_pull_mib. deny has no
	// entry: the server refuses nothing on it.
	Layers map[string]LocalFolderWallLayer `json:"layers,omitempty"`
}

// LocalFolderWallLayer is the layer that set one wall: its kind (policyref's layer names), the unit's name, and
// whether the local_folders knob is locked, which locks every wall in it.
type LocalFolderWallLayer struct {
	Layer  string `json:"layer"`
	Name   string `json:"name,omitempty"`
	Locked bool   `json:"locked,omitempty"`
}

func smallerCap(a, b int) int {
	switch {
	case a <= 0:
		return max(b, 0)
	case b <= 0:
		return a
	}
	return min(a, b)
}

func capAtMost(a, b int) bool { return b <= 0 || (a > 0 && a <= b) }

func capRaises(o, base int) bool { return o > 0 && base > 0 && o > base }
