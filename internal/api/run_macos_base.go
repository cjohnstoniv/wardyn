// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// A macOS VM run's base (CreateRunRequest.MacOSBase): Standard, the one prepare builds, or one of the Mac's
// other bases, each derived from Standard (runnerwire.MacVMCaps.Bases). The name is kept on the run's
// vm_machine from create, so dispatch sends it whenever the run starts, and the Mac then records the base
// it started the run on there.

// What a run that picks a base is told (MAC-B08, MAC-B09).
const (
	macOSBaseNotInPool = "No Mac in this pool has the macOS base %s. Choose another base or another pool."
	macOSBaseDenied    = "%s doesn't allow the macOS base %s. Choose another base."
)

// macOSBaseRequestRefusal is the 400 for a macos_base the request cannot carry: one on a run that is no
// macOS VM, or one that is no base's name; "" when it can.
func macOSBaseRequestRefusal(req createRunRequest) string {
	switch {
	case req.MacOSBase == "":
		return ""
	case !types.MachineKind(req.MachineKind).IsMacOSVM():
		return "macos_base is for a macOS VM run (machine_kind macos_vm)."
	case !types.ValidMacOSBaseName(req.MacOSBase):
		return "macos_base is not a macOS base name: Standard, or lowercase letters, digits, '.', '_' and '-'."
	}
	return ""
}

// macOSBaseMachine is the vm_machine a run that picks a base is created with: the base's name alone, which
// the Mac's own record of the machine replaces at start. nil for every other run.
func macOSBaseMachine(req createRunRequest) *types.VMMachine {
	if req.MacOSBase == "" || !types.MachineKind(req.MachineKind).IsMacOSVM() {
		return nil
	}
	return &types.VMMachine{MacOSBase: req.MacOSBase}
}

// macOSBaseOf is the base a macOS VM run picked, for the sandbox spec; "" is Standard.
func macOSBaseOf(run types.AgentRun) string {
	if run.VMMachine == nil || !run.MachineKind.IsMacOSVM() {
		return ""
	}
	return run.VMMachine.MacOSBase
}

// macOSBaseDeniedMsg is MAC-B09 for a macOS VM run whose base some list in lists (the allowed_macos_bases of
// the profile and restrictions that bind it; an empty list allows any) does not allow, said for profile;
// "" when every list does, or the run is no macOS VM. It is the sentence of macOSBaseDeniedRefusal, for
// the doors that re-judge a run already placed (revive, an end extension).
func macOSBaseDeniedMsg(run types.AgentRun, profile string, lists [][]string) string {
	if !run.MachineKind.IsMacOSVM() {
		return ""
	}
	base := macOSBaseOf(run)
	for _, l := range lists {
		if !types.MacOSBaseAllowed(l, base) {
			return fmt.Sprintf(macOSBaseDenied, profile, cmp.Or(base, types.MacOSStandardBase))
		}
	}
	return ""
}

// macOSChosenBase is the base a run picked (limit's Base) on the Mac whose caps are mc, as macOSVMOffer
// judges it: Standard's own facts, or a ready other base's, with the harnesses it serves. known is false
// when the Mac has no ready base of that name.
func macOSChosenBase(mc *runnerwire.MacVMCaps, limit []macOSBaseLimit) (base runnerwire.MacVMBase, harnesses []string, known bool) {
	if len(limit) == 0 || limit[0].Base == "" || limit[0].Base == types.MacOSStandardBase {
		return mc.Base, mc.Harnesses, true
	}
	for _, d := range mc.Bases {
		if d.Name == limit[0].Base && d.State == runnerwire.MacVMDerivedReady {
			return runnerwire.MacVMBase{State: runnerwire.MacVMBaseReady, GuestOS: d.GuestOS, CreatedAt: d.CreatedAt, BuildVersion: d.BuildVersion}, d.Harnesses, true
		}
	}
	return mc.Base, nil, false
}

// macOSBaseNotFound is MAC-B08 for base, said for one Mac and for a pool alike.
func macOSBaseNotFound(base string) *placement.Refusal {
	return macOSRefusal("macos_base", fmt.Sprintf(macOSBaseNotInPool, base))
}

// macOSBaseDeniedRefusal is the allowed_macos_bases door: 403 placement_denied naming the profile (MAC-B09),
// with the authz.denied row a governance door writes; nil when the caller's profile allows base, or does not
// govern them.
func (s *Server) macOSBaseDeniedRefusal(ctx context.Context, base string, ceiling governanceCeiling) *runRefusal {
	if ceiling.Unit == nil || s.runUngoverned(ctx) || types.MacOSBaseAllowed(ceiling.Limits.AllowedMacOSBases, base) {
		return nil
	}
	d := authz.Deny(authz.ReasonGovernanceUnit, "runs.placement", fmt.Sprintf(macOSBaseDenied, ceiling.Unit.Name, cmp.Or(base, types.MacOSStandardBase))).
		WithPolicy(s.ceilingPolicy(ctx, ceiling))
	f := runError(placement.ReasonPlacementDenied.Status(), string(placement.ReasonPlacementDenied), d.Sentence)
	f.decision = &d
	f.body.Field = "macos_base"
	return f
}

// poolMacOSBases is the other bases of a pool whose eligible Macs all offer macOS VMs (offered, each one's
// caps), by name: ready only where every Mac holds it ready, missing where one lacks it or failed to
// rebuild it, else rebuilding at the least progress.
func poolMacOSBases(offered []*runnerwire.MacVMCaps) []client.PoolMacOSBase {
	var names []string
	for _, mc := range offered {
		for _, d := range mc.Bases {
			names = append(names, d.Name)
		}
	}
	slices.Sort(names)
	var out []client.PoolMacOSBase
	for _, name := range slices.Compact(names) {
		b := client.PoolMacOSBase{Name: name, State: client.MacOSBaseReady}
		var ready []runnerwire.MacVMDerivedBase
		progress := 100
		for _, mc := range offered {
			j := slices.IndexFunc(mc.Bases, func(d runnerwire.MacVMDerivedBase) bool { return d.Name == name })
			switch {
			case j >= 0 && mc.Bases[j].State == runnerwire.MacVMDerivedReady:
				ready = append(ready, mc.Bases[j])
			case j >= 0 && mc.Bases[j].State == runnerwire.MacVMDerivedRebuilding:
				if b.State != client.MacOSBaseMissing {
					b.State = client.MacOSBaseRebuilding
				}
				progress = min(progress, mc.Bases[j].Progress)
			default: // absent, failed, or a newer runner's state
				b.State = client.MacOSBaseMissing
			}
		}
		switch b.State {
		case client.MacOSBaseRebuilding:
			b.Progress = progress
		case client.MacOSBaseReady:
			b.GuestOS, b.Harnesses, b.CreatedAt = ready[0].GuestOS, slices.Clone(ready[0].Harnesses), ready[0].CreatedAt
			for _, d := range ready[1:] {
				if d.GuestOS != b.GuestOS {
					b.GuestOS = ""
				}
				b.Harnesses = slices.DeleteFunc(b.Harnesses, func(h string) bool { return !slices.Contains(d.Harnesses, h) })
				if d.CreatedAt != "" && (b.CreatedAt == "" || d.CreatedAt < b.CreatedAt) {
					b.CreatedAt = d.CreatedAt
				}
			}
		}
		out = append(out, b)
	}
	return out
}
